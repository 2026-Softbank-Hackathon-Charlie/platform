package deploy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/gateway"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/appspec"
	"sigs.k8s.io/yaml"
)

// Adding and removing an environment of an app (ADR 0043).

func TestRenderPeerAbsent(t *testing.T) {
	spec, errs := appspec.Check([]byte(pgSpec))
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	f := File{App: "shop", Instance: "shop", Env: "gke", Commit: "c", Deployment: "dep_1", Spec: *spec, Images: map[string]string{"web": "r/web@sha256:aa"},
		Sealed:     &Sealed{PGApp: map[string]string{"password": "Ag1"}, PGCA: map[string]string{"ca.crt": "Ag2", "ca.key": "Ag3"}},
		DBTopology: &DBTopology{Primary: "gke", Peer: "eks"}}
	present, err := Render(f)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(present, []byte("peerPresent")) {
		t.Fatalf("a present peer is left out, so a pair registered in two environments keeps its files:\n%s", present)
	}

	f.DBTopology = &DBTopology{Primary: "gke", Peer: "eks", PeerAbsent: true}
	absent, err := Render(f)
	if err != nil {
		t.Fatal(err)
	}
	want := "  dbTopology:\n    primary: \"gke\"\n    peer: \"eks\"\n    peerPresent: false\n  sealedSecrets:\n"
	if !strings.Contains(string(absent), want) {
		t.Fatalf("rendered:\n%s\nwant it to have:\n%s", absent, want)
	}
	// Everything else is the paired file's.
	if got := strings.Replace(string(absent), "    peerPresent: false\n", "", 1); got != string(present) {
		t.Fatalf("peerPresent false changed more than the key:\n%s", absent)
	}
	var parsed struct {
		Values struct {
			DBTopology map[string]any `json:"dbTopology"`
		} `json:"values"`
	}
	if err := yaml.Unmarshal(absent, &parsed); err != nil {
		t.Fatal(err)
	}
	if v, ok := parsed.Values.DBTopology["peerPresent"].(bool); !ok || v {
		t.Fatalf("peerPresent parses as %#v, want the boolean false", parsed.Values.DBTopology["peerPresent"])
	}

	// The peer's file is never rendered while the peer is absent.
	f.Env = "eks"
	if _, err := Render(f); err == nil || !strings.Contains(err.Error(), "only the primary's file") {
		t.Fatalf("peer file with an absent peer: %v", err)
	}
}

// registerAlone registers app in env with a database pair whose peer is
// not one of its environments, as POST /v1/apps does for one environment
// (ADR 0043).
func (se *storageEnv) registerAlone(name, env, peer string) store.AppDatabase {
	se.t.Helper()
	spec, errs := appspec.Check([]byte(pgSpec))
	if len(errs) > 0 {
		se.t.Fatal(errs)
	}
	now := time.Now().UTC()
	row, err := NewAppDatabase(se.cipher, name, []string{env, peer}, now)
	if err != nil {
		se.t.Fatal(err)
	}
	err = se.store.CreateAppWithDatabase(context.Background(), store.App{
		Name: name, Repo: "org/" + name, Branch: "main", Environments: []string{env}, AutoDeploy: true,
		Spec: specJSON(*spec), CreatedBy: "op", CreatedAt: now,
	}, row)
	if err != nil {
		se.t.Fatal(err)
	}
	return row
}

// environmentDeployment stores a queued deployment of trigger environment
// that repeats d to envs, as POST /v1/apps/{name}/environments makes it.
func (te *testEnv) environmentDeployment(d store.Deployment, envs ...string) store.Deployment {
	te.t.Helper()
	te.n++
	e := store.Deployment{
		ID: fmt.Sprintf("dep_e%011d", te.n), App: d.App, Branch: d.Branch, Instance: d.Instance, Ref: d.Ref, Commit: d.Commit,
		Trigger: TriggerEnvironment, TriggeredBy: "op", Status: store.DeployQueued, Images: d.Images, Spec: d.Spec,
		CreatedAt: time.Date(2026, 11, 3, 0, 0, te.n, 0, time.UTC),
	}
	for _, env := range envs {
		e.Environments = append(e.Environments, store.DeploymentEnvironment{Env: env, Status: EnvPending})
	}
	if err := te.store.CreateDeployment(context.Background(), e); err != nil {
		te.t.Fatal(err)
	}
	return e
}

type topologyFile struct {
	Values struct {
		DBTopology *struct {
			Primary     string `json:"primary"`
			Peer        string `json:"peer"`
			PeerPresent *bool  `json:"peerPresent"`
		} `json:"dbTopology"`
		SealedSecrets struct {
			PGApp map[string]string `json:"pgApp"`
			PGCA  map[string]string `json:"pgCA"`
		} `json:"sealedSecrets"`
	} `json:"values"`
}

func (se *storageEnv) topology(instance, env string) (topologyFile, bool) {
	se.t.Helper()
	files, _ := se.git.snapshot()
	raw, ok := files[FilePath(instance, env)]
	if !ok {
		return topologyFile{}, false
	}
	var f topologyFile
	if err := yaml.Unmarshal(raw, &f); err != nil {
		se.t.Fatal(err)
	}
	return f, true
}

// TestDeployDatabaseBornAloneThenPeerAdded: an app registered in one
// environment is born with the full topology and peerPresent false; once
// the peer is added, a deployment of trigger environment takes it there
// without a build, and the primary's file drops peerPresent.
func TestDeployDatabaseBornAloneThenPeerAdded(t *testing.T) {
	se := newStorageEnv(t)
	se.github.push("main", sha("p1"), map[string]string{"platform.yaml": pgSpec, "Dockerfile": "FROM scratch\n"})
	row := se.registerAlone("shop", "gke", "eks")
	se.start()
	ctx := context.Background()

	first := se.deploy(sha("p1"), "gke")
	if first.Status != store.DeployHealthy {
		t.Fatalf("first: %s %q\n%s", first.Status, first.Error, se.dump(first.ID))
	}
	f, ok := se.topology("shop", "gke")
	if tp := f.Values.DBTopology; !ok || tp == nil || tp.Primary != "gke" || tp.Peer != "eks" || tp.PeerPresent == nil || *tp.PeerPresent {
		t.Fatalf("gke dbTopology %+v", f.Values.DBTopology)
	}
	if len(f.Values.SealedSecrets.PGApp) != 7 || len(f.Values.SealedSecrets.PGCA) != 2 {
		t.Fatal("the primary is born without its sealed account and CA")
	}
	if got := se.ctrl["gke"].open(t, "shop", "app-pg-ca", f.Values.SealedSecrets.PGCA["ca.crt"]); got != row.CACert {
		t.Fatal("the primary's CA is not the pair's")
	}
	if _, ok := se.topology("shop", "eks"); ok {
		t.Fatal("a file for the peer that is not an environment of the app")
	}

	// The peer is added.
	if err := se.store.SetAppEnvironments(ctx, "shop", []string{"gke"}, []string{"gke", "eks"}); err != nil {
		t.Fatal(err)
	}
	stored, _ := se.store.GetDeployment(ctx, first.ID)
	builds := se.builder.count()
	d := se.environmentDeployment(stored, "gke", "eks")
	se.enqueue(d.ID)
	got := se.wait(d.ID)
	if got.Status != store.DeployHealthy || se.builder.count() != builds {
		t.Fatalf("environment deployment %s %q, builds %d -> %d\n%s", got.Status, got.Error, builds, se.builder.count(), se.dump(got.ID))
	}
	if !hasEvent(se.events(got.ID), EventLog, "using the images of the last healthy deployment") {
		t.Fatalf("events:\n%s", se.dump(got.ID))
	}
	for _, env := range []string{"gke", "eks"} {
		f, ok := se.topology("shop", env)
		if tp := f.Values.DBTopology; !ok || tp == nil || tp.Primary != "gke" || tp.Peer != "eks" || tp.PeerPresent != nil {
			t.Fatalf("%s dbTopology %+v", env, f.Values.DBTopology)
		}
		if got := se.ctrl[env].open(t, "shop", "app-pg-ca", f.Values.SealedSecrets.PGCA["ca.crt"]); got != row.CACert {
			t.Fatalf("%s CA is not the pair's", env)
		}
	}
}

func TestEnvironmentDeploymentNeedsImages(t *testing.T) {
	te := newTestEnv(t)
	te.register("shop", "gke")
	te.start()
	d := te.environmentDeployment(store.Deployment{App: "shop", Branch: "main", Instance: "shop", Ref: "main", Commit: sha("c1"), Images: []store.DeploymentImage{}}, "gke")
	te.enqueue(d.ID)
	got := te.wait(d.ID)
	if got.Status != store.DeployFailed || !strings.Contains(got.Error, "no images for every service") || te.builder.count() != 0 {
		t.Fatalf("%s %q, builds %d", got.Status, got.Error, te.builder.count())
	}
}

// TestDeployToARemovedEnvironmentWritesNothing: a deployment that still
// lists an environment the app no longer has fails before its commit.
func TestDeployToARemovedEnvironmentWritesNothing(t *testing.T) {
	te := newTestEnv(t)
	te.register("shop", "gke", "eks")
	te.start()
	ctx := context.Background()
	if err := te.store.SetAppEnvironments(ctx, "shop", []string{"gke", "eks"}, []string{"gke"}); err != nil {
		t.Fatal(err)
	}
	d := te.deployment("shop", sha("c1"), "gke", "eks")
	te.enqueue(d.ID)
	got := te.wait(d.ID)
	if got.Status != store.DeployFailed || !strings.Contains(got.Error, "environment eks was removed") {
		t.Fatalf("%s %q", got.Status, got.Error)
	}
	if files, _ := te.git.snapshot(); len(files) != 0 {
		t.Fatalf("committed %v", sortedKeys(files))
	}
}

func TestRemoveEnvironment(t *testing.T) {
	te := newTestEnv(t)
	te.register("shop", "gke", "eks")
	ex := te.start()
	ctx := context.Background()
	branch := te.addBranch("shop", "dev")
	te.github.push("dev", sha("dev1"), shopFiles)
	for _, d := range []store.Deployment{te.deployment("shop", sha("c1"), "gke", "eks"), te.deploymentTo("shop", "dev", branch, sha("dev1"), "gke", "eks")} {
		te.enqueue(d.ID)
		if got := te.wait(d.ID); got.Status != store.DeployHealthy {
			t.Fatalf("%s: %s %q", d.ID, got.Status, got.Error)
		}
	}
	if a, _ := te.gw.app("shop"); a.WriteBackend != "gke" || a.Backends["eks"] == "" {
		t.Fatalf("gateway before %+v", a)
	}

	for name, tc := range map[string]struct {
		env  string
		prep func() func()
		want error
		msg  string
	}{
		"the first environment": {env: "gke", prep: func() func() { return func() {} }, msg: "first environment"},
		"not an environment":    {env: "onprem", prep: func() func() { return func() {} }, msg: "not in environment"},
		"writes there": {env: "eks", want: ErrEnvironmentInUse, prep: func() func() {
			te.gw.mu.Lock()
			old := te.gw.apps[branch]
			a := old
			a.WriteBackend = "eks"
			te.gw.apps[branch] = a
			te.gw.mu.Unlock()
			return func() { te.gw.mu.Lock(); te.gw.apps[branch] = old; te.gw.mu.Unlock() }
		}},
		"reads there": {env: "eks", want: ErrEnvironmentInUse, prep: func() func() {
			te.gw.mu.Lock()
			old := te.gw.apps["shop"]
			a := old
			a.ReadWeights = map[string]int{"gke": 90, "eks": 10}
			te.gw.apps["shop"] = a
			te.gw.mu.Unlock()
			return func() { te.gw.mu.Lock(); te.gw.apps["shop"] = old; te.gw.mu.Unlock() }
		}},
		"writes held": {env: "eks", want: ErrWritesHeld, prep: func() func() {
			te.gw.mu.Lock()
			old := te.gw.apps["shop"]
			a := old
			a.WritesHeld = true
			te.gw.apps["shop"] = a
			te.gw.mu.Unlock()
			return func() { te.gw.mu.Lock(); te.gw.apps["shop"] = old; te.gw.mu.Unlock() }
		}},
		"a deployment in progress": {env: "eks", want: ErrDeploymentInProgress, prep: func() func() {
			d := te.deploymentTo("shop", "dev", branch, sha("dev1"), "gke") // stored, not taken
			return func() {
				now := time.Now().UTC()
				d.Status, d.FinishedAt = store.DeploySuperseded, &now
				if _, err := te.store.UpdateDeployment(ctx, d); err != nil {
					t.Fatal(err)
				}
			}
		}},
	} {
		undo := tc.prep()
		err := ex.RemoveEnvironment(ctx, "shop", tc.env)
		undo()
		if err == nil || (tc.want != nil && !errors.Is(err, tc.want)) || (tc.msg != "" && !strings.Contains(err.Error(), tc.msg)) {
			t.Fatalf("%s: %v, want %v", name, err, tc.want)
		}
		if a, _ := te.store.GetApp(ctx, "shop"); !slices.Equal(a.Environments, []string{"gke", "eks"}) {
			t.Fatalf("%s: environments %v", name, a.Environments)
		}
		if files, _ := te.git.snapshot(); files[FilePath("shop", "eks")] == nil || files[FilePath(branch, "eks")] == nil {
			t.Fatalf("%s: removed files", name)
		}
	}

	if err := ex.RemoveEnvironment(ctx, "shop", "eks"); err != nil {
		t.Fatal(err)
	}
	files, _ := te.git.snapshot()
	if files[FilePath("shop", "eks")] != nil || files[FilePath(branch, "eks")] != nil || files[FilePath("shop", "gke")] == nil || files[FilePath(branch, "gke")] == nil {
		t.Fatalf("files after %v", sortedKeys(files))
	}
	for _, in := range []string{"shop", branch} {
		a, _ := te.gw.app(in)
		if _, ok := a.Backends["eks"]; ok || a.WriteBackend != "gke" || a.Backends["gke"] == "" {
			t.Fatalf("gateway app %s after %+v", in, a)
		}
	}
	if a, _ := te.store.GetApp(ctx, "shop"); !slices.Equal(a.Environments, []string{"gke"}) {
		t.Fatalf("environments %v", a.Environments)
	}
	// Again: the environment is gone, so nothing to do.
	if err := ex.RemoveEnvironment(ctx, "shop", "eks"); err == nil {
		t.Fatal("removed twice")
	}
}

// TestCleanupStorageOfARemovedEnvironment: the instances' buckets in an
// environment taken out of the app go like a deleted instance's, and stay
// when the environment is added back first.
func TestCleanupStorageOfARemovedEnvironment(t *testing.T) {
	se := newStorageEnv(t)
	se.register("shop", "gke", "eks")
	ex := se.start()
	ctx := context.Background()
	if d := se.deploy(sha("s1"), "gke", "eks"); d.Status != store.DeployHealthy {
		t.Fatalf("%s %q", d.Status, d.Error)
	}
	if n, err := ex.CleanupStorage(ctx); err != nil || n != 0 {
		t.Fatalf("cleanup with both environments: %d, %v", n, err)
	}
	if err := se.store.SetAppEnvironments(ctx, "shop", []string{"gke", "eks"}, []string{"gke"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { // the loop runs on its own after start; run it once more here
		_, _ = ex.CleanupStorage(ctx)
		rows, _ := se.store.ListInstanceStorage(ctx, "shop", "")
		return len(rows) == 1 && rows[0].Env == "gke"
	})
	if !slices.Contains(se.drvs["eks"].deleted, "shop") || slices.Contains(se.drvs["gke"].deleted, "shop") {
		t.Fatalf("deleted eks %v, gke %v", se.drvs["eks"].deleted, se.drvs["gke"].deleted)
	}
}

// TestRemoveEnvironmentKeepsAMigratedGatewayOrder: the write backend and
// the other read weights stay as the gateway has them.
func TestRemoveEnvironmentKeepsTheGatewayRouting(t *testing.T) {
	te := newTestEnv(t)
	te.register("shop", "gke", "eks", "onprem")
	ex := te.start()
	ctx := context.Background()
	te.gw.mu.Lock()
	te.gw.apps["shop"] = gateway.AppInfo{Name: "shop", Backends: map[string]string{"gke": "http://g", "eks": "http://e", "onprem": "http://o"},
		WriteBackend: "gke", ReadWeights: map[string]int{"gke": 80, "onprem": 20, "eks": 0}}
	te.gw.mu.Unlock()
	if err := ex.RemoveEnvironment(ctx, "shop", "eks"); err != nil {
		t.Fatal(err)
	}
	a, _ := te.gw.app("shop")
	if len(a.Backends) != 2 || a.WriteBackend != "gke" || a.ReadWeights["gke"] != 80 || a.ReadWeights["onprem"] != 20 {
		t.Fatalf("gateway after %+v", a)
	}
}

// TestEnvironmentDeploymentNeverReplacesANewerCommit: it is superseded like
// a redeploy, so a push that is unfinished keeps it from running and a
// later push replaces it (ADR 0043).
func TestEnvironmentDeploymentNeverReplacesANewerCommit(t *testing.T) {
	te := newTestEnv(t)
	te.register("shop", "gke", "eks")
	ctx := context.Background()
	push := te.deployment("shop", sha("c1"), "gke")
	// Start recovers this queued push too. Hold its build so the intended
	// unfinished-push precondition does not depend on worker scheduling.
	_, release := te.builder.hold(push.ID)
	defer release()
	env := te.environmentDeployment(store.Deployment{App: "shop", Branch: "main", Instance: "shop", Ref: "main", Commit: sha("old"), Images: []store.DeploymentImage{}}, "gke", "eks")
	te.start()
	te.enqueue(env.ID)
	if got := te.wait(env.ID); got.Status != store.DeploySuperseded {
		t.Fatalf("environment deployment beside a push: %s %q", got.Status, got.Error)
	}
	if got, _ := te.store.GetDeployment(ctx, push.ID); store.DeploymentFinished(got.Status) {
		t.Fatalf("the push was finished: %s", got.Status)
	}
}

// pushStoredTo stores a queued push deployment of commit to envs of the
// app's default instance, as the webhook makes it from the app's
// environments at that time.
func (te *testEnv) pushStoredTo(app, commit string, envs ...string) store.Deployment {
	te.t.Helper()
	d := te.deploymentTo(app, "main", app, commit, envs...)
	// deploymentTo stores a manual one; a push is the same with its trigger.
	te.n++
	d.ID, d.Trigger = fmt.Sprintf("dep_p%011d", te.n), "push"
	d.CreatedAt = time.Date(2026, 11, 3, 0, 0, te.n, 0, time.UTC)
	if err := te.store.CreateDeployment(context.Background(), d); err != nil {
		te.t.Fatal(err)
	}
	return d
}

// TestPushFollowsTheAppsEnvironments: a push stored before an environment
// was added (it supersedes the environment's deployment) deploys there too,
// and one stored before an environment was removed leaves it out, instead
// of failing (ADR 0043).
func TestPushFollowsTheAppsEnvironments(t *testing.T) {
	te := newTestEnv(t)
	te.register("shop", "gke")
	te.start()
	ctx := context.Background()

	push := te.pushStoredTo("shop", sha("c1"), "gke")
	if err := te.store.SetAppEnvironments(ctx, "shop", []string{"gke"}, []string{"gke", "eks"}); err != nil {
		t.Fatal(err)
	}
	te.enqueue(push.ID)
	got := te.wait(push.ID)
	if got.Status != store.DeployHealthy || envNames(got.Environments) != "gke, eks" {
		t.Fatalf("push stored before eks was added: %s %q to %s\n%s", got.Status, got.Error, envNames(got.Environments), te.dump(got.ID))
	}
	if files, _ := te.git.snapshot(); files[FilePath("shop", "eks")] == nil {
		t.Fatalf("no eks file: %v", sortedKeys(files))
	}
	if !hasEvent(te.events(got.ID), EventLog, "the app's environments changed since this deployment was stored") {
		t.Fatalf("events:\n%s", te.dump(got.ID))
	}

	te.github.push("main", sha("c2"), shopFiles)
	push = te.pushStoredTo("shop", sha("c2"), "gke", "eks")
	if err := te.store.SetAppEnvironments(ctx, "shop", []string{"gke", "eks"}, []string{"gke"}); err != nil {
		t.Fatal(err)
	}
	te.enqueue(push.ID)
	if got := te.wait(push.ID); got.Status != store.DeployHealthy || envNames(got.Environments) != "gke" {
		t.Fatalf("push stored before eks was removed: %s %q to %s", got.Status, got.Error, envNames(got.Environments))
	}
}

// TestPeerAddedBackGetsANewBackupPath: removing the peer of a database pair
// counts up its generation, so the peer's next database archives under a
// new, empty path in the backup bucket, while the primary's stays (ADR
// 0043).
func TestPeerAddedBackGetsANewBackupPath(t *testing.T) {
	se := newStorageEnv(t)
	se.github.push("main", sha("p1"), map[string]string{"platform.yaml": pgSpec, "Dockerfile": "FROM scratch\n"})
	se.registerAlone("shop", "gke", "eks")
	ex := se.start()
	ctx := context.Background()
	app, err := se.store.GetApp(ctx, "shop")
	if err != nil {
		t.Fatal(err)
	}
	first := se.deploy(sha("p1"), "gke")
	if first.Status != store.DeployHealthy {
		t.Fatalf("first: %s %q", first.Status, first.Error)
	}
	addPeer := func(want string) {
		t.Helper()
		a, _ := se.store.GetApp(ctx, "shop")
		if err := se.store.SetAppEnvironments(ctx, "shop", a.Environments, []string{"gke", "eks"}); err != nil {
			t.Fatal(err)
		}
		stored, _ := se.store.GetDeployment(ctx, first.ID)
		d := se.environmentDeployment(stored, "gke", "eks")
		se.enqueue(d.ID)
		if got := se.wait(d.ID); got.Status != store.DeployHealthy {
			t.Fatalf("add eks: %s %q\n%s", got.Status, got.Error, se.dump(got.ID))
		}
		if b := se.file("shop", "eks").Values.Backup; b == nil || b.Path != want {
			t.Fatalf("eks backup %+v, want path %s", b, want)
		}
		if b := se.file("shop", "gke").Values.Backup; b == nil || b.Path != BackupPath(app.CreatedAt) {
			t.Fatalf("gke backup %+v, want path %s", b, BackupPath(app.CreatedAt))
		}
	}
	addPeer(BackupPath(app.CreatedAt))
	if err := ex.RemoveEnvironment(ctx, "shop", "eks"); err != nil {
		t.Fatal(err)
	}
	if row, _ := se.store.GetAppDatabase(ctx, "shop"); row.PeerGeneration != 1 {
		t.Fatalf("peer generation %d after one removal", row.PeerGeneration)
	}
	addPeer(BackupPath(app.CreatedAt) + "-1")
}

// TestRemoveEnvironmentTakesTheGatewayBackendFirst: the weight 0 backend
// goes before the files, so a gitops failure leaves no backend pointing at
// workloads that are going, and the environment stays for a retry.
func TestRemoveEnvironmentTakesTheGatewayBackendFirst(t *testing.T) {
	te := newTestEnv(t)
	te.register("shop", "gke", "eks")
	ex := te.start()
	ctx := context.Background()
	d := te.deployment("shop", sha("c1"), "gke", "eks")
	te.enqueue(d.ID)
	if got := te.wait(d.ID); got.Status != store.DeployHealthy {
		t.Fatalf("%s %q", got.Status, got.Error)
	}
	te.git.mu.Lock()
	te.git.err = errors.New("github down")
	te.git.mu.Unlock()
	if err := ex.RemoveEnvironment(ctx, "shop", "eks"); !errors.Is(err, ErrGitOps) {
		t.Fatalf("err %v, want ErrGitOps", err)
	}
	if a, _ := te.gw.app("shop"); a.Backends["eks"] != "" || a.Backends["gke"] == "" {
		t.Fatalf("gateway after a gitops failure %+v", a)
	}
	if a, _ := te.store.GetApp(ctx, "shop"); !slices.Equal(a.Environments, []string{"gke", "eks"}) {
		t.Fatalf("environments %v", a.Environments)
	}
	te.git.mu.Lock()
	te.git.err = nil
	te.git.mu.Unlock()
	if err := ex.RemoveEnvironment(ctx, "shop", "eks"); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if files, _ := te.git.snapshot(); files[FilePath("shop", "eks")] != nil {
		t.Fatal("eks file kept")
	}
}

// TestRemoveEnvironmentErrors: what it refuses wraps the errors the API maps
// to its codes.
func TestRemoveEnvironmentErrors(t *testing.T) {
	te := newTestEnv(t)
	te.register("shop", "gke", "eks")
	ex := te.start()
	ctx := context.Background()
	for env, want := range map[string]error{"gke": ErrPrimaryEnvironment, "onprem": ErrNotInEnvironment} {
		if err := ex.RemoveEnvironment(ctx, "shop", env); !errors.Is(err, want) {
			t.Fatalf("%s: %v, want %v", env, err, want)
		}
	}
	if err := ex.RemoveEnvironment(ctx, "nope", "eks"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown app: %v", err)
	}
}

// addMeanwhileStore adds an environment to the app, the way POST
// environments does without the instance locks, right before the first
// SetAppEnvironments of RemoveEnvironment.
type addMeanwhileStore struct {
	store.Store
	once sync.Once
	add  string
}

func (s *addMeanwhileStore) SetAppEnvironments(ctx context.Context, name string, from, to []string) error {
	s.once.Do(func() {
		a, err := s.Store.GetApp(ctx, name)
		if err == nil {
			err = s.Store.SetAppEnvironments(ctx, name, a.Environments, append(slices.Clone(a.Environments), s.add))
		}
		if err != nil {
			panic(err)
		}
	})
	return s.Store.SetAppEnvironments(ctx, name, from, to)
}

// TestRemoveEnvironmentFinishesWhenAnotherIsAddedMeanwhile: an environment
// added while the removal holds its locks makes the store's compare and set
// miss after the gateway and the files are gone already; the removal is
// finished on the list read again, which keeps the added environment.
func TestRemoveEnvironmentFinishesWhenAnotherIsAddedMeanwhile(t *testing.T) {
	te := newTestEnv(t)
	te.register("shop", "gke", "eks")
	race := &addMeanwhileStore{Store: te.store, add: "onprem"}
	te.cfg.Store = race
	ex := te.start()
	ctx := context.Background()
	d := te.deployment("shop", sha("c1"), "gke", "eks")
	te.enqueue(d.ID)
	if got := te.wait(d.ID); got.Status != store.DeployHealthy {
		t.Fatalf("%s %q", got.Status, got.Error)
	}
	if err := ex.RemoveEnvironment(ctx, "shop", "eks"); err != nil {
		t.Fatalf("remove with an environment added meanwhile: %v", err)
	}
	if a, _ := te.store.GetApp(ctx, "shop"); !slices.Equal(a.Environments, []string{"gke", "onprem"}) {
		t.Fatalf("environments %v, want gke and the added onprem", a.Environments)
	}
	if a, _ := te.gw.app("shop"); a.Backends["eks"] != "" {
		t.Fatalf("gateway still has eks: %+v", a)
	}
}
