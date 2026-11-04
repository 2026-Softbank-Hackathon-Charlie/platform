package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/gateway"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/appspec"
)

func TestDeployHappyPath(t *testing.T) {
	te := newTestEnv(t)
	te.register("shop", "gke", "eks")
	te.start()
	d := te.deployment("shop", sha("c1"), "gke", "eks")
	te.enqueue(d.ID)
	got := te.wait(d.ID)

	if got.Status != store.DeployHealthy || got.Error != "" || got.StartedAt == nil || got.FinishedAt == nil {
		t.Fatalf("deployment: %+v\n%s", got, te.dump(d.ID))
	}
	evs := te.events(d.ID)
	want := []string{"phase:building", "phase:committing", "phase:syncing", "done:healthy"}
	if p := phases(evs); !slices.Equal(p, want) {
		t.Fatalf("phases %v, want %v", p, want)
	}
	if !hasEvent(evs, EventLog, "Step 1/2") || !hasEvent(evs, EventLog, "build fake-build-1 started") {
		t.Errorf("build output missing:\n%s", te.dump(d.ID))
	}
	if !hasEvent(evs, EventDone, "https://shop.hopcharlie.test") {
		t.Errorf("done event does not name the app URL:\n%s", te.dump(d.ID))
	}
	if got.BuildID != "fake-build-1" || len(got.Images) != 2 || !strings.Contains(got.Images[0].Image, "registry.test/hopcharlie/apps/shop/web@sha256:") {
		t.Errorf("build id %q, images %+v", got.BuildID, got.Images)
	}
	for _, e := range got.Environments {
		if e.Status != EnvHealthy || e.SyncStatus != SyncSynced || e.HealthStatus != HealthHealthy {
			t.Errorf("environment %+v", e)
		}
		if want := "http://shop-web.shop." + e.Env + ".test:8080"; e.URL != want {
			t.Errorf("%s url %q, want %q", e.Env, e.URL, want)
		}
		if !hasEvent(evs, EventEnvironment, "Synced, Healthy") {
			t.Errorf("no environment event for %s", e.Env)
		}
	}

	// One commit with both files, in the ADR 0011 format.
	files, msgs := te.git.snapshot()
	if len(msgs) != 2 || !strings.Contains(msgs[1], d.ID) {
		t.Fatalf("gitops commits %q", msgs)
	}
	for _, env := range []string{"gke", "eks"} {
		b := files["apps/shop/"+env+".yaml"]
		if !strings.Contains(string(b), `env: "`+env+`"`) || !strings.Contains(string(b), `deployment: "`+d.ID+`"`) {
			t.Errorf("apps/shop/%s.yaml:\n%s", env, b)
		}
	}
	if r := te.argo.refreshed(); !slices.Contains(r, "shop-gke") || !slices.Contains(r, "shop-eks") {
		t.Errorf("refreshes %v", r)
	}

	// Gateway: both environments, writes to the first.
	app, ok := te.gw.app("shop")
	if !ok || app.WriteBackend != "gke" || len(app.Backends) != 2 || app.Backends["eks"] != "http://shop-web.shop.eks.test:8080" {
		t.Errorf("gateway app %+v", app)
	}

	// The spec at the commit became the app's.
	a, _ := te.store.GetApp(context.Background(), "shop")
	if a.SpecCommit != sha("c1") {
		t.Errorf("spec commit %q", a.SpecCommit)
	}
	var spec appspec.Spec
	if err := json.Unmarshal(a.Spec, &spec); err != nil || len(spec.Services) != 2 {
		t.Errorf("spec %s (%v)", a.Spec, err)
	}
}

func TestDeployGKEImageRegistry(t *testing.T) {
	for _, tc := range []struct {
		name, registry string
		envs           []string
	}{
		{name: "disabled", envs: []string{"gke", "eks"}},
		{name: "GKE first", registry: testGKERegistry, envs: []string{"gke", "eks", "onprem"}},
		{name: "EKS first", registry: testGKERegistry, envs: []string{"eks", "gke"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			te := newTestEnv(t)
			te.cfg.GKEImageRegistry = tc.registry
			te.register("shop", tc.envs...)
			te.start()
			d := te.deployment("shop", sha("c1"), tc.envs...)
			te.enqueue(d.ID)
			got := te.wait(d.ID)
			if got.Status != store.DeployHealthy {
				t.Fatalf("deployment %s: %s", got.Status, got.Error)
			}
			files, _ := te.git.snapshot()
			for _, image := range got.Images {
				if !strings.HasPrefix(image.Image, "registry.test/hopcharlie/apps/shop/") {
					t.Fatalf("canonical image was changed: %s", image.Image)
				}
				for _, env := range tc.envs {
					want := image.Image
					if tc.registry != "" && env == "gke" {
						_, digest, _ := strings.Cut(image.Image, "@")
						want = tc.registry + "/shop/" + image.Service + "@" + digest
					}
					if !strings.Contains(string(files[FilePath("shop", env)]), `image: "`+want+`"`) {
						t.Errorf("%s image for %s: want %s", env, image.Service, want)
					}
				}
			}
		})
	}
}

func TestDeployBuildFailure(t *testing.T) {
	te := newTestEnv(t)
	te.builder.err = errors.New("docker build web: exit status 1: COPY failed")
	te.register("shop", "gke")
	te.start()
	d := te.deployment("shop", sha("c1"), "gke")
	te.enqueue(d.ID)
	got := te.wait(d.ID)
	if got.Status != store.DeployFailed || !strings.Contains(got.Error, "COPY failed") {
		t.Fatalf("deployment %s: %q", got.Status, got.Error)
	}
	evs := te.events(d.ID)
	if last := evs[len(evs)-1]; last.Type != EventDone || last.Phase != store.DeployFailed || !strings.Contains(last.Message, "COPY failed") {
		t.Errorf("done event %+v", last)
	}
	if _, msgs := te.git.snapshot(); len(msgs) != 1 {
		t.Errorf("a failed build committed: %q", msgs)
	}
	if got.Environments[0].Status != EnvPending {
		t.Errorf("environment %+v", got.Environments[0])
	}
}

func TestDeployInvalidSpecAtCommit(t *testing.T) {
	te := newTestEnv(t)
	te.register("shop", "gke")
	te.github.push("main", sha("bad"), map[string]string{
		"platform.yaml": "name: shop\nservices:\n  - name: web\n    port: 70000\n    env:\n      PORT: \"1\"\n",
		"Dockerfile":    "FROM scratch\n",
	})
	te.start()
	d := te.deployment("shop", sha("bad"), "gke")
	te.enqueue(d.ID)
	got := te.wait(d.ID)
	if got.Status != store.DeployFailed || !strings.Contains(got.Error, "services[0].port: must be between 1 and 65535") {
		t.Fatalf("deployment %s: %q", got.Status, got.Error)
	}
	evs := te.events(d.ID)
	if !hasEvent(evs, EventLog, "platform.yaml: services[0].port") || !hasEvent(evs, EventLog, "platform.yaml: services[0].env.PORT") {
		t.Errorf("validation errors missing from the events:\n%s", te.dump(d.ID))
	}
	if te.builder.count() != 0 {
		t.Error("built an invalid spec")
	}
	if a, _ := te.store.GetApp(context.Background(), "shop"); a.SpecCommit != "" {
		t.Errorf("invalid spec stored at %s", a.SpecCommit)
	}

	// A missing Dockerfile and a renamed app fail too.
	te.github.push("main", sha("nodocker"), map[string]string{"platform.yaml": shopSpec, "Dockerfile": "FROM scratch\n"})
	d = te.deployment("shop", sha("nodocker"), "gke")
	te.enqueue(d.ID)
	if got := te.wait(d.ID); !strings.Contains(got.Error, "services[1].dockerfile: worker/Dockerfile does not exist") {
		t.Errorf("missing Dockerfile: %q", got.Error)
	}
	te.github.push("main", sha("renamed"), map[string]string{"platform.yaml": strings.Replace(shopSpec, "name: shop", "name: store", 1), "Dockerfile": "x", "worker/Dockerfile": "x"})
	d = te.deployment("shop", sha("renamed"), "gke")
	te.enqueue(d.ID)
	if got := te.wait(d.ID); !strings.Contains(got.Error, `names the app "store"`) {
		t.Errorf("renamed app: %q", got.Error)
	}
}

func TestDeployArgoTimeout(t *testing.T) {
	te := newTestEnv(t)
	te.cfg.SyncTimeout = 150 * time.Millisecond
	te.argo.missing["shop-eks"] = true // the ApplicationSet never generates it
	te.register("shop", "gke", "eks")
	te.start()
	d := te.deployment("shop", sha("c1"), "gke", "eks")
	te.enqueue(d.ID)
	got := te.wait(d.ID)
	if got.Status != store.DeployFailed || !strings.Contains(got.Error, "did not report every environment healthy within 150ms") || !strings.Contains(got.Error, "eks") {
		t.Fatalf("deployment %s: %q", got.Status, got.Error)
	}
	gke, eks := got.Environments[0], got.Environments[1]
	if gke.Status != EnvHealthy || eks.Status != EnvFailed || !strings.Contains(eks.Message, "waiting for Argo CD to create Application shop-eks") {
		t.Errorf("environments %+v %+v", gke, eks)
	}
	if _, ok := te.gw.app("shop"); ok {
		t.Error("a failed deployment registered the app with the gateway")
	}
}

func TestDeployDegradedHealth(t *testing.T) {
	te := newTestEnv(t)
	te.cfg.SyncTimeout = 150 * time.Millisecond
	te.argo.health["shop-gke"] = HealthDegraded
	te.register("shop", "gke")
	te.start()
	d := te.deployment("shop", sha("c1"), "gke")
	te.enqueue(d.ID)
	got := te.wait(d.ID)
	e := got.Environments[0]
	if got.Status != store.DeployFailed || e.Status != EnvDegraded || e.HealthStatus != HealthDegraded || !strings.Contains(e.Message, "progress deadline") {
		t.Fatalf("deployment %s %q, environment %+v", got.Status, got.Error, e)
	}
	if !hasEvent(te.events(d.ID), EventEnvironment, "Synced, Degraded") {
		t.Errorf("events:\n%s", te.dump(d.ID))
	}
}

func TestDeploySyncFailure(t *testing.T) {
	te := newTestEnv(t)
	te.argo.failed["shop-gke"] = "one or more objects failed to apply"
	te.register("shop", "gke")
	te.start()
	d := te.deployment("shop", sha("c1"), "gke")
	te.enqueue(d.ID)
	got := te.wait(d.ID)
	if got.Status != store.DeployFailed || got.Environments[0].Status != EnvFailed || !strings.Contains(got.Error, "failed to apply") {
		t.Fatalf("deployment %s %q %+v", got.Status, got.Error, got.Environments)
	}
}

func TestDeployAcceptsALaterGitOpsRevision(t *testing.T) {
	te := newTestEnv(t)
	te.git.others = 1 // another app commits right after us; Argo CD syncs to that
	te.register("shop", "gke")
	te.start()
	d := te.deployment("shop", sha("c1"), "gke")
	te.enqueue(d.ID)
	if got := te.wait(d.ID); got.Status != store.DeployHealthy {
		t.Fatalf("deployment %s %q", got.Status, got.Error)
	}

}

func TestDeployWaitsForTheCommit(t *testing.T) {
	te := newTestEnv(t)
	te.cfg.SyncTimeout = 100 * time.Millisecond
	te.argo.revision = te.git.head() // stuck at the revision before our commit
	te.register("shop", "gke")
	te.start()
	d := te.deployment("shop", sha("c1"), "gke")
	te.enqueue(d.ID)
	got := te.wait(d.ID)
	if got.Status != store.DeployFailed || !strings.Contains(got.Environments[0].Message, "Argo CD is at revision") {
		t.Fatalf("stale revision: %s %+v", got.Status, got.Environments[0])
	}
	if n := len(te.argo.refreshed()); n < 2 {
		t.Errorf("refresh requested %d times while waiting for the commit", n)
	}
}

func TestDeploySupersedeWhileBuilding(t *testing.T) {
	te := newTestEnv(t)
	te.register("shop", "gke", "eks")
	te.start()
	old := te.deployment("shop", sha("c1"), "gke")
	started, release := te.builder.hold(old.ID)
	te.enqueue(old.ID)
	<-started

	te.github.push("main", sha("c2"), shopFiles)
	newer := te.deployment("shop", sha("c2"), "gke", "eks") // covers gke
	te.enqueue(newer.ID)
	if got := te.get(old.ID); got.Status != store.DeploySuperseded || got.FinishedAt == nil {
		t.Fatalf("old deployment is %s", got.Status)
	}
	if got := te.wait(newer.ID); got.Status != store.DeployHealthy {
		t.Fatalf("newer deployment %s %q", got.Status, got.Error)
	}
	release() // the old build finishes after the newer deployment committed
	waitIdle(t, te.ex)

	_, msgs := te.git.snapshot()
	for _, m := range msgs {
		if strings.Contains(m, old.ID) {
			t.Fatalf("the superseded deployment committed: %q", msgs)
		}
	}
	got := te.get(old.ID)
	if got.Status != store.DeploySuperseded || len(got.Images) != 0 {
		t.Errorf("old deployment changed after it was superseded: %+v", got)
	}
	evs := te.events(old.ID)
	if last := evs[len(evs)-1]; last.Type != EventDone || last.Phase != store.DeploySuperseded || !strings.Contains(last.Message, newer.ID) {
		t.Errorf("last event of the old deployment %+v", last)
	}

	// A newer deployment to fewer environments does not supersede.
	a := te.deployment("shop", sha("c2"), "gke", "eks")
	startedA, releaseA := te.builder.hold(a.ID)
	te.enqueue(a.ID)
	<-startedA
	b := te.deployment("shop", sha("c2"), "gke")
	te.enqueue(b.ID)
	if got := te.get(a.ID); store.DeploymentFinished(got.Status) {
		t.Errorf("a deployment to gke superseded one to gke and eks: %s", got.Status)
	}
	releaseA()
	te.wait(a.ID)
	te.wait(b.ID)
}

func TestRedeployNeverRunsBesideADeployment(t *testing.T) {
	te := newTestEnv(t)
	te.register("shop", "gke", "eks")
	te.start()
	first := te.deployment("shop", sha("c1"), "gke", "eks")
	te.enqueue(first.ID)
	base := te.wait(first.ID)
	setHealth := func(h string) {
		te.argo.mu.Lock()
		te.argo.health["shop-gke"] = h
		te.argo.mu.Unlock()
	}

	// A redeploy is syncing when a deployment comes: the deployment
	// supersedes it, though it goes to fewer environments.
	setHealth("Progressing")
	r := te.redeployment(base, "gke", "eks")
	te.enqueue(r.ID)
	waitFor(t, func() bool { return te.get(r.ID).Status == store.DeploySyncing })
	push := te.deployment("shop", sha("c1"), "gke")
	te.enqueue(push.ID)
	if got := te.get(r.ID); got.Status != store.DeploySuperseded {
		t.Fatalf("the redeploy is %s, want superseded by the deployment", got.Status)
	}
	setHealth("")
	if got := te.wait(push.ID); got.Status != store.DeployHealthy {
		t.Fatalf("deployment %s %q", got.Status, got.Error)
	}

	// A deployment is building when a redeploy comes: the redeploy
	// supersedes itself and the deployment goes on.
	push = te.deployment("shop", sha("c1"), "gke")
	started, release := te.builder.hold(push.ID)
	te.enqueue(push.ID)
	<-started
	r = te.redeployment(base, "gke", "eks")
	te.enqueue(r.ID)
	got := te.get(r.ID)
	evs := te.events(r.ID)
	if got.Status != store.DeploySuperseded || !strings.Contains(evs[len(evs)-1].Message, push.ID+", which started meanwhile; it uses the current secrets") {
		t.Fatalf("the redeploy is %s\n%s", got.Status, te.dump(r.ID))
	}
	release()
	if got := te.wait(push.ID); got.Status != store.DeployHealthy {
		t.Fatalf("deployment %s %q", got.Status, got.Error)
	}
	if _, msgs := te.git.snapshot(); slices.ContainsFunc(msgs, func(m string) bool { return strings.Contains(m, r.ID) }) {
		t.Fatalf("the superseded redeploy committed: %q", msgs)
	}

	// After a restart, in either order.
	for _, redeployFirst := range []bool{false, true} {
		te := newTestEnv(t)
		te.register("shop", "gke")
		var push, r store.Deployment
		if redeployFirst {
			r = te.redeployment(base, "gke")
			push = te.deployment("shop", sha("c1"), "gke")
		} else {
			push = te.deployment("shop", sha("c1"), "gke")
			r = te.redeployment(base, "gke")
		}
		te.start()
		if got := te.wait(push.ID); got.Status != store.DeployHealthy {
			t.Fatalf("redeploy first %v: deployment %s %q", redeployFirst, got.Status, got.Error)
		}
		if got := te.get(r.ID); got.Status != store.DeploySuperseded {
			t.Fatalf("redeploy first %v: the redeploy is %s\n%s", redeployFirst, got.Status, te.dump(r.ID))
		}
	}
}

func waitIdle(t *testing.T, ex *Executor) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		ex.mu.Lock()
		n := len(ex.running) + len(ex.queue)
		ex.mu.Unlock()
		if n == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("executor is still busy")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestDeployResumeAfterRestart(t *testing.T) {
	te := newTestEnv(t)
	te.register("shop", "gke")
	ctx := context.Background()

	// A previous server built the images and was syncing when it stopped.
	built := te.deployment("shop", sha("c1"), "gke")
	built.Status, built.BuildID = store.DeploySyncing, "fake-build-0"
	now := time.Now().UTC()
	built.StartedAt = &now
	for _, sv := range []string{"web", "worker"} {
		built.Images = append(built.Images, store.DeploymentImage{Service: sv, Image: ImageRepository("registry.test", "shop", sv) + "@sha256:" + strings.Repeat("a", 64)})
	}
	if _, err := te.store.UpdateDeployment(ctx, built); err != nil {
		t.Fatal(err)
	}
	// A newer deployment to the same environment was still queued.
	queued := te.deployment("shop", sha("c1"), "gke")

	te.start()
	if got := te.wait(queued.ID); got.Status != store.DeployHealthy {
		t.Fatalf("queued deployment %s %q\n%s", got.Status, got.Error, te.dump(queued.ID))
	}
	if got := te.get(built.ID); got.Status != store.DeploySuperseded {
		t.Fatalf("the older unfinished deployment is %s, want superseded", got.Status)
	}

	// A lone interrupted deployment resumes without building again.
	te2 := newTestEnv(t)
	te2.register("shop", "gke")
	d := te2.deployment("shop", sha("c1"), "gke")
	d.Status, d.Images, d.StartedAt = store.DeployCommitting, built.Images, &now
	if _, err := te2.store.UpdateDeployment(ctx, d); err != nil {
		t.Fatal(err)
	}
	te2.start()
	got := te2.wait(d.ID)
	if got.Status != store.DeployHealthy || te2.builder.count() != 0 {
		t.Fatalf("resumed deployment %s %q, builds %d", got.Status, got.Error, te2.builder.count())
	}
	if !hasEvent(te2.events(d.ID), EventPhase, "resumed after a restart (was committing)") || !hasEvent(te2.events(d.ID), EventLog, "not building again") {
		t.Errorf("events:\n%s", te2.dump(d.ID))
	}
}

func TestDeployRecommitIsANoop(t *testing.T) {
	te := newTestEnv(t)
	te.register("shop", "gke")
	te.start()
	d := te.deployment("shop", sha("c1"), "gke")
	te.enqueue(d.ID)
	te.wait(d.ID)
	_, before := te.git.snapshot()

	// The same deployment rendered again is the same bytes, so a resumed
	// deployment that had committed writes nothing.
	got := te.get(d.ID)
	spec, _ := appspec.Check([]byte(shopSpec))
	images := imagesFor(got.Images, *spec)
	files, _ := te.git.snapshot()
	again, err := Render(File{App: "shop", Instance: "shop", Env: "gke", Commit: got.Commit, Deployment: got.ID, Spec: *spec, Images: images})
	if err != nil || string(again) != string(files["apps/shop/gke.yaml"]) {
		t.Fatalf("render differs (%v):\n%s\nvs\n%s", err, again, files["apps/shop/gke.yaml"])
	}
	head, changed, err := te.git.Write(context.Background(), map[string][]byte{"apps/shop/gke.yaml": again}, "again")
	if err != nil || changed || head != te.git.head() {
		t.Fatalf("rewrite: %s %v %v", head, changed, err)
	}
	if _, after := te.git.snapshot(); len(after) != len(before) {
		t.Errorf("commits %q, then %q", before, after)
	}
}

func TestDeployGatewayWritesHeld(t *testing.T) {
	te := newTestEnv(t)
	te.gw.apps["shop"] = gateway.AppInfo{Name: "shop", Backends: map[string]string{"gke": "http://old"}, WriteBackend: "gke", WritesHeld: true}
	te.register("shop", "gke")
	te.start()
	d := te.deployment("shop", sha("c1"), "gke")
	te.enqueue(d.ID)
	got := te.wait(d.ID)
	if got.Status != store.DeployFailed || !strings.Contains(got.Error, "holds shop's writes") {
		t.Fatalf("deployment %s %q", got.Status, got.Error)
	}
	if app, _ := te.gw.app("shop"); app.Backends["gke"] != "http://old" || len(te.gw.puts) != 0 {
		t.Errorf("gateway changed while writes were held: %+v", app)
	}
}

func TestDeployKeepsTheGatewayRouting(t *testing.T) {
	te := newTestEnv(t)
	// A migration moved shop to eks with a read canary on gke.
	te.gw.apps["shop"] = gateway.AppInfo{Name: "shop", Backends: map[string]string{"gke": "http://a", "eks": "http://b", "gone": "http://c"},
		WriteBackend: "eks", ReadWeights: map[string]int{"gke": 10, "eks": 90, "gone": 0}}
	te.register("shop", "gke", "eks")
	te.start()
	d := te.deployment("shop", sha("c1"), "gke") // only gke
	te.enqueue(d.ID)
	if got := te.wait(d.ID); got.Status != store.DeployHealthy {
		t.Fatalf("deployment %s %q", got.Status, got.Error)
	}
	app, _ := te.gw.app("shop")
	if app.WriteBackend != "eks" || app.Backends["gke"] != "http://shop-web.shop.gke.test:8080" || app.Backends["eks"] != "http://b" || app.Backends["gone"] != "" {
		t.Errorf("gateway app %+v", app)
	}
	if put := te.gw.puts[0]; put.ReadWeights["gke"] != 10 || put.ReadWeights["eks"] != 90 {
		t.Errorf("read weights %v", put.ReadWeights)
	}

	// Deploying the same again changes nothing at the gateway.
	d = te.deployment("shop", sha("c1"), "gke")
	te.enqueue(d.ID)
	te.wait(d.ID)
	if len(te.gw.puts) != 1 || !hasEvent(te.events(d.ID), EventLog, "already sends") {
		t.Errorf("puts %d, events:\n%s", len(te.gw.puts), te.dump(d.ID))
	}
}

func TestDeployWithoutPublicServiceOrGateway(t *testing.T) {
	te := newTestEnv(t)
	te.cfg.Gateway, te.cfg.Backends = nil, nil
	te.register("shop", "gke")
	te.start()
	d := te.deployment("shop", sha("c1"), "gke")
	te.enqueue(d.ID)
	if got := te.wait(d.ID); got.Status != store.DeployHealthy || got.Environments[0].URL != "" {
		t.Fatalf("deployment %+v", got)
	}
}

func TestRemoveApp(t *testing.T) {
	te := newTestEnv(t)
	te.register("shop", "gke", "eks")
	te.start()
	d := te.deployment("shop", sha("c1"), "gke", "eks")
	te.enqueue(d.ID)
	te.wait(d.ID)
	other := te.deployment("shop", sha("c1"), "gke")
	started, release := te.builder.hold(other.ID)
	defer release()
	te.enqueue(other.ID)
	<-started

	// Writes held: nothing is forgotten, so the removal can be repeated.
	info, _ := te.gw.app("shop")
	info.WritesHeld = true
	te.gw.apps["shop"] = info
	if err := te.ex.RemoveApp(context.Background(), "shop"); !errors.Is(err, ErrWritesHeld) {
		t.Fatalf("remove while writes are held: %v", err)
	}
	if got := te.get(other.ID); got.Status != store.DeploySuperseded {
		t.Errorf("unfinished deployment is %s", got.Status)
	}
	info.WritesHeld = false
	te.gw.apps["shop"] = info

	te.argo.mu.Lock()
	refreshes := te.argo.appSetRefreshes
	te.argo.mu.Unlock()
	if err := te.ex.RemoveApp(context.Background(), "shop"); err != nil {
		t.Fatal(err)
	}
	// The ApplicationSet reads the removal now, not when its cache expires.
	te.argo.mu.Lock()
	if te.argo.appSetRefreshes != refreshes+1 {
		t.Errorf("ApplicationSet refreshes %d, want %d", te.argo.appSetRefreshes, refreshes+1)
	}
	te.argo.mu.Unlock()
	files, msgs := te.git.snapshot()
	if len(files) != 0 || msgs[len(msgs)-1] != "delete app shop" {
		t.Errorf("gitops files %v, commits %q", files, msgs)
	}
	if _, ok := te.gw.app("shop"); ok {
		t.Error("the gateway still has the app")
	}
	if _, err := te.store.GetApp(context.Background(), "shop"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("app still stored: %v", err)
	}
	if err := te.ex.RemoveApp(context.Background(), "shop"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("second removal: %v", err)
	}

	// The gitops repository unreachable is ErrGitOps, and the app stays.
	te.register("cart", "gke")
	te.git.err = errors.New("connection refused")
	if err := te.ex.RemoveApp(context.Background(), "cart"); !errors.Is(err, ErrGitOps) {
		t.Errorf("remove with gitops down: %v", err)
	}
}

func TestNewChecksConfig(t *testing.T) {
	te := newTestEnv(t)
	cfg := te.cfg
	cfg.Backends = nil
	if _, err := New(cfg); err == nil {
		t.Error("New accepted a gateway without backends")
	}
	cfg = te.cfg
	cfg.GitHub = nil
	if _, err := New(cfg); err == nil {
		t.Error("New accepted no GitHub")
	}
}
