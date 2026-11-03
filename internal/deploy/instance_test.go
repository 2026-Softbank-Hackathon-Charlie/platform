package deploy

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
)

func TestDeployAddedBranchInstance(t *testing.T) {
	te := newTestEnv(t)
	te.register("shop", "eks")
	inst := te.addBranch("shop", "feature/x")
	if inst != "shop--feature-x" {
		t.Fatalf("instance %q", inst)
	}
	te.github.push("feature/x", sha("x1"), shopFiles)
	te.start()
	d := te.deploymentTo("shop", "feature/x", inst, sha("x1"), "eks")
	te.enqueue(d.ID)
	got := te.wait(d.ID)
	if got.Status != store.DeployHealthy {
		t.Fatalf("deployment %s %q\n%s", got.Status, got.Error, te.dump(d.ID))
	}

	// The gitops file is the instance's, in its namespace, for the app's chart.
	files, msgs := te.git.snapshot()
	b := string(files["apps/shop--feature-x/eks.yaml"])
	for _, want := range []string{"\napp: \"shop\"\n", "\nnamespace: \"shop--feature-x\"\n", "\nvalues:\n  app: \"shop\"\n", "registry.test/hopcharlie/apps/shop/web@sha256:"} {
		if !strings.Contains(b, want) {
			t.Errorf("apps/shop--feature-x/eks.yaml lacks %q:\n%s", want, b)
		}
	}
	if _, ok := files["apps/shop/eks.yaml"]; ok || len(files) != 1 {
		t.Errorf("gitops files %v", sortedKeys(files))
	}
	if last := msgs[len(msgs)-1]; !strings.HasPrefix(last, "deploy shop--feature-x ") || !strings.Contains(last, d.ID) {
		t.Errorf("commit message %q", last)
	}
	if r := te.argo.refreshed(); !slices.Contains(r, "shop--feature-x-eks") || slices.Contains(r, "shop-eks") {
		t.Errorf("refreshes %v", r)
	}

	// The gateway app and URL are the instance's; the Service keeps the app's name.
	app, ok := te.gw.app("shop--feature-x")
	if !ok || app.Backends["eks"] != "http://shop-web.shop--feature-x.eks.test:8080" || app.WriteBackend != "eks" {
		t.Errorf("gateway app %+v", app)
	}
	if _, ok := te.gw.app("shop"); ok {
		t.Error("the default instance was registered with the gateway")
	}
	if got.Environments[0].URL != "http://shop-web.shop--feature-x.eks.test:8080" {
		t.Errorf("environment url %q", got.Environments[0].URL)
	}
	evs := te.events(d.ID)
	if !hasEvent(evs, EventDone, "https://shop--feature-x.hopcharlie.test") || !hasEvent(evs, EventPhase, "writing apps/shop--feature-x for eks") {
		t.Errorf("events:\n%s", te.dump(d.ID))
	}

	// The spec went to the branch, not to the app.
	if br := te.branch("shop", "feature/x"); br.SpecCommit != sha("x1") || !strings.Contains(string(br.Spec), `"web"`) {
		t.Errorf("branch spec %s at %q", br.Spec, br.SpecCommit)
	}
	if a, _ := te.store.GetApp(context.Background(), "shop"); a.SpecCommit != "" {
		t.Errorf("the app's spec commit became %q", a.SpecCommit)
	}
	if !hasEvent(evs, EventLog, "is now branch feature/x's spec") {
		t.Errorf("events:\n%s", te.dump(d.ID))
	}
}

func (te *testEnv) branch(app, branch string) store.AppBranch {
	te.t.Helper()
	list, err := te.store.ListAppBranches(context.Background(), app)
	if err != nil {
		te.t.Fatal(err)
	}
	for _, b := range list {
		if b.Branch == branch {
			return b
		}
	}
	te.t.Fatalf("app %s has no branch %s", app, branch)
	return store.AppBranch{}
}

func TestSupersedeStaysInItsInstance(t *testing.T) {
	te := newTestEnv(t)
	te.register("shop", "eks")
	dev := te.addBranch("shop", "dev")
	te.github.push("dev", sha("d1"), shopFiles)
	te.start()

	// A newer deployment of dev leaves a running one of main alone.
	main := te.deployment("shop", sha("c1"), "eks")
	started, release := te.builder.hold(main.ID)
	te.enqueue(main.ID)
	<-started
	d := te.deploymentTo("shop", "dev", dev, sha("d1"), "eks")
	te.enqueue(d.ID)
	if got := te.get(main.ID); store.DeploymentFinished(got.Status) {
		t.Fatalf("a deployment of dev finished one of main: %s", got.Status)
	}
	if got := te.wait(d.ID); got.Status != store.DeployHealthy {
		t.Fatalf("dev deployment %s %q", got.Status, got.Error)
	}
	release()
	if got := te.wait(main.ID); got.Status != store.DeployHealthy {
		t.Fatalf("main deployment %s %q", got.Status, got.Error)
	}

	// And the other way round.
	d = te.deploymentTo("shop", "dev", dev, sha("d1"), "eks")
	started, release = te.builder.hold(d.ID)
	te.enqueue(d.ID)
	<-started
	main = te.deployment("shop", sha("c1"), "eks")
	te.enqueue(main.ID)
	if got := te.get(d.ID); store.DeploymentFinished(got.Status) {
		t.Fatalf("a deployment of main finished one of dev: %s", got.Status)
	}
	te.wait(main.ID)
	// A newer deployment of dev still supersedes it.
	newer := te.deploymentTo("shop", "dev", dev, sha("d1"), "eks")
	te.enqueue(newer.ID)
	if got := te.get(d.ID); got.Status != store.DeploySuperseded {
		t.Fatalf("an older deployment of dev is %s, want superseded", got.Status)
	}
	release()
	if got := te.wait(newer.ID); got.Status != store.DeployHealthy {
		t.Fatalf("newer dev deployment %s %q", got.Status, got.Error)
	}
	waitIdle(t, te.ex)
	for _, name := range []string{"shop", "shop--dev"} {
		if _, ok := te.gw.app(name); !ok {
			t.Errorf("the gateway lacks %s", name)
		}
	}
	files, _ := te.git.snapshot()
	if len(files) != 2 || files["apps/shop/eks.yaml"] == nil || files["apps/shop--dev/eks.yaml"] == nil {
		t.Errorf("gitops files %d", len(files))
	}
}

func TestUpdateSpecPerInstance(t *testing.T) {
	te := newTestEnv(t)
	te.register("shop", "eks")
	dev := te.addBranch("shop", "dev")
	te.github.push("dev", sha("d1"), shopFiles)
	te.start()

	// A deployment of dev is the newest of its instance even when one of
	// main came after it, so it stores dev's spec, and main's stores the
	// app's.
	d := te.deploymentTo("shop", "dev", dev, sha("d1"), "eks")
	m := te.deployment("shop", sha("c1"), "eks")
	te.enqueue(d.ID)
	te.wait(d.ID)
	te.enqueue(m.ID)
	te.wait(m.ID)
	if br := te.branch("shop", "dev"); br.SpecCommit != sha("d1") {
		t.Errorf("dev's spec commit %q", br.SpecCommit)
	}
	if a, _ := te.store.GetApp(context.Background(), "shop"); a.SpecCommit != sha("c1") || a.Branch != "main" {
		t.Errorf("app spec commit %q, branch %q", a.SpecCommit, a.Branch)
	}
}

// goneBranchStore loses the added branches: ListAppBranches finds none, or
// UpdateAppBranch finds none.
type goneBranchStore struct {
	store.Store
	list bool
}

func (s goneBranchStore) ListAppBranches(ctx context.Context, app string) ([]store.AppBranch, error) {
	if s.list {
		return nil, nil
	}
	return s.Store.ListAppBranches(ctx, app)
}

func (s goneBranchStore) UpdateAppBranch(context.Context, store.AppBranch) error {
	return fmt.Errorf("app branch: %w", store.ErrNotFound)
}

func TestDeployFailsWhenTheBranchIsGone(t *testing.T) {
	for _, list := range []bool{true, false} {
		te := newTestEnv(t)
		te.cfg.Store = goneBranchStore{Store: te.store, list: list}
		te.register("shop", "eks")
		dev := te.addBranch("shop", "dev")
		te.github.push("dev", sha("d1"), shopFiles)
		te.start()
		d := te.deploymentTo("shop", "dev", dev, sha("d1"), "eks")
		te.enqueue(d.ID)
		got := te.wait(d.ID)
		if got.Status != store.DeployFailed || got.Error != "branch dev is no longer deployed by app shop" {
			t.Errorf("gone from the list %v: deployment %s %q", list, got.Status, got.Error)
		}
		if _, msgs := te.git.snapshot(); len(msgs) != 1 || te.builder.count() != 0 {
			t.Errorf("gone from the list %v: commits %q, builds %d", list, msgs, te.builder.count())
		}
	}
}

func TestRemoveAppWithInstances(t *testing.T) {
	te := newTestEnv(t)
	te.register("shop", "eks")
	dev := te.addBranch("shop", "dev")
	te.github.push("dev", sha("d1"), shopFiles)
	te.start()
	for _, d := range []store.Deployment{te.deployment("shop", sha("c1"), "eks"), te.deploymentTo("shop", "dev", dev, sha("d1"), "eks")} {
		te.enqueue(d.ID)
		te.wait(d.ID)
	}
	running := te.deploymentTo("shop", "dev", dev, sha("d1"), "eks")
	started, release := te.builder.hold(running.ID)
	defer release()
	te.enqueue(running.ID)
	<-started

	// The dev instance's writes are held: the app is kept, to retry.
	info, _ := te.gw.app(dev)
	info.WritesHeld = true
	te.gw.apps[dev] = info
	if err := te.ex.RemoveApp(context.Background(), "shop"); !errors.Is(err, ErrWritesHeld) {
		t.Fatalf("remove while dev's writes are held: %v", err)
	}
	if got := te.get(running.ID); got.Status != store.DeploySuperseded {
		t.Errorf("the running deployment of dev is %s", got.Status)
	}
	te.branch("shop", "dev")
	info.WritesHeld = false
	te.gw.apps[dev] = info

	_, before := te.git.snapshot()
	if err := te.ex.RemoveApp(context.Background(), "shop"); err != nil {
		t.Fatal(err)
	}
	files, msgs := te.git.snapshot()
	// The first attempt removed both directories in one commit already.
	if len(files) != 0 || len(msgs) != len(before) || msgs[len(msgs)-1] != "delete app shop" || strings.Count(strings.Join(msgs, "\n"), "delete app shop") != 1 {
		t.Errorf("gitops files %v, commits %q", files, msgs)
	}
	for _, name := range []string{"shop", dev} {
		if _, ok := te.gw.app(name); ok {
			t.Errorf("the gateway still has %s", name)
		}
	}
	if _, err := te.store.GetApp(context.Background(), "shop"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("app still stored: %v", err)
	}
	if list, _ := te.store.ListAppBranches(context.Background(), "shop"); len(list) != 0 {
		t.Errorf("branches still stored: %+v", list)
	}
}

func TestRemoveInstance(t *testing.T) {
	te := newTestEnv(t)
	te.register("shop", "eks")
	dev := te.addBranch("shop", "dev")
	te.github.push("dev", sha("d1"), shopFiles)
	te.start()
	ctx := context.Background()
	for _, d := range []store.Deployment{te.deployment("shop", sha("c1"), "eks"), te.deploymentTo("shop", "dev", dev, sha("d1"), "eks")} {
		te.enqueue(d.ID)
		te.wait(d.ID)
	}
	// One deployment of each instance is running.
	runDev := te.deploymentTo("shop", "dev", dev, sha("d1"), "eks")
	startedDev, releaseDev := te.builder.hold(runDev.ID)
	defer releaseDev()
	te.enqueue(runDev.ID)
	<-startedDev
	runMain := te.deployment("shop", sha("c1"), "eks")
	startedMain, releaseMain := te.builder.hold(runMain.ID)
	te.enqueue(runMain.ID)
	<-startedMain

	// Not found: the app, a branch it never added, its own branch.
	for _, tc := range []struct{ app, branch string }{{"ghost", "dev"}, {"shop", "nope"}, {"shop", "main"}} {
		if err := te.ex.RemoveInstance(ctx, tc.app, tc.branch); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("RemoveInstance(%s, %s): %v", tc.app, tc.branch, err)
		}
	}
	if err := te.ex.RemoveInstance(ctx, "shop", "main"); !errors.Is(err, store.ErrInstanceNotFound) {
		t.Errorf("RemoveInstance of the app's own branch: %v", err)
	}

	// The gitops repository unreachable: ErrGitOps, and the branch stays.
	te.git.mu.Lock()
	te.git.err = errors.New("connection refused")
	te.git.mu.Unlock()
	if err := te.ex.RemoveInstance(ctx, "shop", "dev"); !errors.Is(err, ErrGitOps) {
		t.Fatalf("remove with gitops down: %v", err)
	}
	if got := te.get(runDev.ID); got.Status != store.DeploySuperseded {
		t.Errorf("the running deployment of dev is %s", got.Status)
	}
	te.branch("shop", "dev")
	te.git.mu.Lock()
	te.git.err = nil
	te.git.mu.Unlock()

	// Writes held at the gateway: ErrWritesHeld, and the branch stays.
	info, _ := te.gw.app(dev)
	info.WritesHeld = true
	te.gw.apps[dev] = info
	if err := te.ex.RemoveInstance(ctx, "shop", "dev"); !errors.Is(err, ErrWritesHeld) {
		t.Fatalf("remove while writes are held: %v", err)
	}
	te.branch("shop", "dev")
	info.WritesHeld = false
	te.gw.apps[dev] = info

	te.argo.mu.Lock()
	refreshes := te.argo.appSetRefreshes
	te.argo.mu.Unlock()
	if err := te.ex.RemoveInstance(ctx, "shop", "dev"); err != nil {
		t.Fatal(err)
	}
	files, msgs := te.git.snapshot()
	if files["apps/shop--dev/eks.yaml"] != nil || files["apps/shop/eks.yaml"] == nil || !slices.Contains(msgs, "delete instance shop--dev") {
		t.Errorf("gitops files %d, commits %q", len(files), msgs)
	}
	te.argo.mu.Lock()
	if te.argo.appSetRefreshes != refreshes+1 {
		t.Errorf("ApplicationSet refreshes %d, want %d", te.argo.appSetRefreshes, refreshes+1)
	}
	te.argo.mu.Unlock()
	if _, ok := te.gw.app(dev); ok {
		t.Error("the gateway still has shop--dev")
	}
	if _, ok := te.gw.app("shop"); !ok {
		t.Error("the gateway lost shop")
	}
	if list, _ := te.store.ListAppBranches(ctx, "shop"); len(list) != 0 {
		t.Errorf("branches %+v", list)
	}
	if _, err := te.store.GetDeployment(ctx, runDev.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("dev's deployment is still stored: %v", err)
	}
	if err := te.ex.RemoveInstance(ctx, "shop", "dev"); !errors.Is(err, store.ErrInstanceNotFound) {
		t.Errorf("second removal: %v", err)
	}

	// The default instance's deployment went on.
	if got := te.get(runMain.ID); store.DeploymentFinished(got.Status) {
		t.Errorf("main's running deployment is %s", got.Status)
	}
	releaseMain()
	if got := te.wait(runMain.ID); got.Status != store.DeployHealthy {
		t.Errorf("main's deployment %s %q", got.Status, got.Error)
	}
}
