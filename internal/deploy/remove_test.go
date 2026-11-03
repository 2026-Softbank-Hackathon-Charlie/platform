package deploy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/appspec"
)

// forgetHookStore runs hook once, right before the store forgets an app or
// a branch: the last moment of a removal.
type forgetHookStore struct {
	store.Store
	hook func()
}

func (s *forgetHookStore) run() {
	if h := s.hook; h != nil {
		s.hook = nil
		h()
	}
}

func (s *forgetHookStore) DeleteApp(ctx context.Context, name string) error {
	s.run()
	return s.Store.DeleteApp(ctx, name)
}

func (s *forgetHookStore) DeleteAppBranch(ctx context.Context, app, branch string) error {
	s.run()
	return s.Store.DeleteAppBranch(ctx, app, branch)
}

// lateDeployment creates and queues a deployment of an instance, as a push
// that lands while the instance is being removed, and gives it the time to
// commit: until it is past building, then up to 300ms for its gitops file.
func (te *testEnv) lateDeployment(app, branch, instance string) string {
	te.t.Helper()
	d := te.deploymentTo(app, branch, instance, sha("late"), "eks")
	te.enqueue(d.ID)
	deadline := time.Now().Add(5 * time.Second)
	for st := te.get(d.ID).Status; st == store.DeployQueued || st == store.DeployBuilding; st = te.get(d.ID).Status {
		if time.Now().After(deadline) {
			te.t.Fatalf("the late deployment is still %s", st)
		}
		time.Sleep(time.Millisecond)
	}
	for end := time.Now().Add(300 * time.Millisecond); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
		if files, _ := te.git.snapshot(); strings.Contains(string(files[FilePath(instance, "eks")]), d.ID) {
			break
		}
	}
	return d.ID
}

// removed checks that nothing of the instance outlived its removal, the
// late deployment included.
func (te *testEnv) removed(instance, late string) {
	te.t.Helper()
	files, msgs := te.git.snapshot()
	if b := files[FilePath(instance, "eks")]; b != nil {
		te.t.Errorf("%s was written after the removal:\n%s", FilePath(instance, "eks"), b)
	}
	for _, m := range msgs {
		if strings.Contains(m, late) {
			te.t.Errorf("the late deployment committed: %q", msgs)
		}
	}
	if _, ok := te.gw.app(instance); ok {
		te.t.Errorf("the gateway has %s again", instance)
	}
	if _, err := te.store.GetDeployment(context.Background(), late); !errors.Is(err, store.ErrNotFound) {
		te.t.Errorf("the late deployment is still stored: %v", err)
	}
}

func TestRemoveInstanceForgetsUnderTheLock(t *testing.T) {
	te := newTestEnv(t)
	hs := &forgetHookStore{Store: te.store}
	te.cfg.Store = hs
	te.register("shop", "eks")
	dev := te.addBranch("shop", "dev")
	te.github.push("dev", sha("late"), shopFiles)
	te.start()
	d := te.deploymentTo("shop", "dev", dev, sha("late"), "eks")
	te.enqueue(d.ID)
	te.wait(d.ID)

	// A deployment of dev created after its deployments were superseded
	// waits for the lock, then finds itself deleted with the branch.
	var late string
	hs.hook = func() { late = te.lateDeployment("shop", "dev", dev) }
	if err := te.ex.RemoveInstance(context.Background(), "shop", "dev"); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, te.ex)
	te.removed(dev, late)
	if list, _ := te.store.ListAppBranches(context.Background(), "shop"); len(list) != 0 {
		t.Errorf("branches %+v", list)
	}
}

func TestRemoveAppForgetsUnderTheLock(t *testing.T) {
	te := newTestEnv(t)
	hs := &forgetHookStore{Store: te.store}
	te.cfg.Store = hs
	te.register("shop", "eks")
	te.github.push("main", sha("late"), shopFiles)
	te.start()
	d := te.deployment("shop", sha("late"), "eks")
	te.enqueue(d.ID)
	te.wait(d.ID)

	var late string
	hs.hook = func() { late = te.lateDeployment("shop", "main", "shop") }
	if err := te.ex.RemoveApp(context.Background(), "shop"); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, te.ex)
	te.removed("shop", late)
	if _, err := te.store.GetApp(context.Background(), "shop"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("app still stored: %v", err)
	}
}

// hookGitOps runs beforeDelete before every Delete.
type hookGitOps struct {
	*fakeGitOps
	beforeDelete func()
}

func (g *hookGitOps) Delete(ctx context.Context, dirs []string, message string) (string, bool, error) {
	if g.beforeDelete != nil {
		g.beforeDelete()
	}
	return g.fakeGitOps.Delete(ctx, dirs, message)
}

func TestRemoveAppStartsOverForABranchAddedMeanwhile(t *testing.T) {
	te := newTestEnv(t)
	hg := &hookGitOps{fakeGitOps: te.git}
	te.cfg.GitOps = hg
	te.register("shop", "eks")
	te.start()
	ctx := context.Background()

	// While the app's directories are removed, branch qa is added and its
	// first deployment commits: RemoveApp removes it too.
	deletes := 0
	hg.beforeDelete = func() {
		if deletes++; deletes == 1 {
			qa := te.addBranch("shop", "qa")
			if _, _, err := te.git.Write(ctx, map[string][]byte{FilePath(qa, "eks"): []byte("qa\n")}, "deploy "+qa); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := te.ex.RemoveApp(ctx, "shop"); err != nil {
		t.Fatal(err)
	}
	if files, msgs := te.git.snapshot(); len(files) != 0 || deletes != 2 {
		t.Errorf("gitops files %v after %d deletes, commits %q", sortedKeys(files), deletes, msgs)
	}
	if _, err := te.store.GetApp(ctx, "shop"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("app still stored: %v", err)
	}

	// Branches that keep coming make it give up, keeping the app to retry.
	te.register("cart", "eks")
	deletes = 0
	hg.beforeDelete = func() {
		deletes++
		b := fmt.Sprintf("qa%d", deletes)
		in, _ := appspec.InstanceName("cart", b)
		if err := te.store.CreateAppBranch(ctx, store.AppBranch{App: "cart", Branch: b, Instance: in, Spec: []byte(`{}`), CreatedAt: time.Now().UTC()}, 10); err != nil {
			t.Fatal(err)
		}
	}
	if err := te.ex.RemoveApp(ctx, "cart"); err == nil || !strings.Contains(err.Error(), "kept changing") || deletes != removeAppAttempts {
		t.Fatalf("remove while branches keep coming: %v after %d attempts", err, deletes)
	}
	if _, err := te.store.GetApp(ctx, "cart"); err != nil {
		t.Fatalf("the app went: %v", err)
	}
	hg.beforeDelete = nil
	if err := te.ex.RemoveApp(ctx, "cart"); err != nil {
		t.Fatal(err)
	}
}

func TestRemoveInstancesLocksEachInstanceOnce(t *testing.T) {
	te := newTestEnv(t)
	te.register("shop", "eks")
	te.start()
	done := make(chan error, 1)
	go func() {
		done <- te.ex.removeInstances(context.Background(), []string{"shop", "shop--dev", "shop"}, "delete", func() error { return nil })
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("removeInstances deadlocked on an instance listed twice")
	}
}
