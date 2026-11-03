package api

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/runs"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

// flakyStore makes some store calls fail, as a database that is down or a
// request that lost a race would.
type flakyStore struct {
	store.Store
	mu               sync.Mutex
	createDeployment error            // CreateDeployment fails with it
	branchesErr      map[string]error // ListAppBranches fails with it, by app
	hideBranches     bool             // ListAppBranches finds none
}

func (s *flakyStore) set(f func(*flakyStore)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f(s)
}

func (s *flakyStore) CreateDeployment(ctx context.Context, d store.Deployment) error {
	s.mu.Lock()
	err := s.createDeployment
	s.mu.Unlock()
	if err != nil {
		return err
	}
	return s.Store.CreateDeployment(ctx, d)
}

func (s *flakyStore) ListAppBranches(ctx context.Context, app string) ([]store.AppBranch, error) {
	s.mu.Lock()
	err, hide := s.branchesErr[app], s.hideBranches
	s.mu.Unlock()
	switch {
	case err != nil:
		return nil, err
	case hide:
		return nil, nil
	}
	return s.Store.ListAppBranches(ctx, app)
}

// withStore serves the API from fs over the harness's store.
func withStore(fs *flakyStore) option {
	return func(h *harness, _ *runs.Config, d *Deps) {
		fs.Store = h.store
		d.Store = fs
	}
}

func TestCreateAppInstanceWithoutItsFirstDeployment(t *testing.T) {
	fs := &flakyStore{}
	h := newHarness(t, withApps(""), withGitHub(instanceGitHub(), &fakeDeployer{}), withStore(fs))
	h.registerShop()
	ctx := t.Context()

	// The instance or the app was deleted between the branch and its first
	// deployment.
	for _, tc := range []struct {
		err  error
		code string
	}{
		{fmt.Errorf("deployment: %w", store.ErrInstanceNotFound), "instance_not_found"},
		{fmt.Errorf("deployment: %w", store.ErrNotFound), "app_not_found"},
	} {
		fs.set(func(s *flakyStore) { s.createDeployment = tc.err })
		if code, b := h.do("POST", "/v1/apps/shop/instances", opToken, map[string]any{"branch": "dev"}); code != 404 || errorCode(t, b) != tc.code {
			t.Errorf("%v: %d %s, want 404 %s", tc.err, code, b, tc.code)
		}
		_ = h.store.DeleteAppBranch(ctx, "shop", "dev") // what the concurrent delete did
	}

	// Any other failure forgets the branch, so a retry adds it.
	fs.set(func(s *flakyStore) { s.createDeployment = errors.New("connection reset") })
	if code, b := h.do("POST", "/v1/apps/shop/instances", opToken, map[string]any{"branch": "dev"}); code != 500 || errorCode(t, b) != "internal" {
		t.Fatalf("%d %s, want 500", code, b)
	}
	if rows, _ := h.store.ListAppBranches(ctx, "shop"); len(rows) != 0 {
		t.Fatalf("a branch without its first deployment stayed: %+v", rows)
	}
	fs.set(func(s *flakyStore) { s.createDeployment = nil })
	if got := h.addInstance(map[string]any{"branch": "dev"}); got.Instance.Name != "shop--dev" || got.Deployment.Instance != "shop--dev" {
		t.Fatalf("retry %s", mustJSON(t, got))
	}

	// The store still refuses what the checks before GitHub missed.
	fs.set(func(s *flakyStore) { s.hideBranches = true })
	if code, b := h.do("POST", "/v1/apps/shop/instances", opToken, map[string]any{"branch": "dev"}); code != 409 || errorCode(t, b) != "branch_exists" {
		t.Fatalf("%d %s, want 409 branch_exists from the store", code, b)
	}
}

func TestAppStatusSurvivesABusyBranch(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	created := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)
	if err := h.store.CreateApp(ctx, store.App{Name: "shop", Repo: shopRepo, Branch: "main", Environments: []string{"gke"}, AutoDeploy: true,
		Spec: []byte(`{"name":"shop","services":[]}`), SpecCommit: headSHA, CreatedBy: "op", CreatedAt: created}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.CreateAppBranch(ctx, store.AppBranch{App: "shop", Branch: "dev", Instance: "shop--dev", Spec: []byte(`{}`), CreatedAt: created}, 2); err != nil {
		t.Fatal(err)
	}
	// main deployed once; dev deployed more often since than any one window holds.
	deps := []store.Deployment{{ID: "dep_main", App: "shop", Branch: "main", Instance: "shop", Ref: "main", Commit: headSHA, CreatedAt: created,
		Environments: []store.DeploymentEnvironment{{Env: "gke", Status: "healthy"}}}}
	for i := range 3 * appStatusDeployments {
		deps = append(deps, store.Deployment{ID: fmt.Sprintf("dep_dev%03d", i), App: "shop", Branch: "dev", Instance: "shop--dev", Ref: "dev", Commit: devSHA,
			CreatedAt: created.Add(time.Duration(i+1) * time.Minute), Environments: []store.DeploymentEnvironment{{Env: "gke", Status: "healthy"}}})
	}
	for _, d := range deps {
		d.Trigger, d.TriggeredBy, d.Status = "push", "github:dev", store.DeployHealthy
		if err := h.store.CreateDeployment(ctx, d); err != nil {
			t.Fatal(err)
		}
	}

	code, b := h.do("GET", "/v1/apps", viewerToken, nil)
	if code != 200 {
		t.Fatalf("%d %s", code, b)
	}
	dep := decode[apiv1.AppList](t, b).Apps[0].Deploy
	if dep.LastDeployment == nil || dep.LastDeployment.Id != "dep_main" || dep.Status[0].Status != "healthy" || *dep.Status[0].Commit != headSHA {
		t.Fatalf("the default instance lost its deployments to dev's: %s", mustJSON(t, dep))
	}
	if dev := dep.Instances[1]; dev.LastDeployment == nil || dev.LastDeployment.Id != fmt.Sprintf("dep_dev%03d", 3*appStatusDeployments-1) || dev.Status[0].Status != "healthy" {
		t.Fatalf("dev %s", mustJSON(t, dev))
	}
}

func TestWebhookDeploysPastAFailingApp(t *testing.T) {
	fs := &flakyStore{}
	h, dep := webhookHarness(t, withStore(fs))
	ctx := t.Context()
	for _, b := range []store.AppBranch{
		{App: "shop", Branch: "dev", Instance: "shop--dev", AutoDeploy: true},
		{App: "shop-manual", Branch: "dev", Instance: "shop-manual--dev", AutoDeploy: true},
	} {
		b.Spec, b.SpecCommit, b.CreatedAt = []byte(`{}`), headSHA, time.Now().UTC()
		if err := h.store.CreateAppBranch(ctx, b, 2); err != nil {
			t.Fatal(err)
		}
	}
	push := func(delivery string) (int, []byte) {
		body := pushPayload(shopRepo, "refs/heads/dev", pushSHA, false)
		return h.webhook("push", delivery, body, sign(hookToken, []byte(body)))
	}

	// shop-manual's branches cannot be read after shop's deployment was
	// queued: shop's still counts.
	fs.set(func(s *flakyStore) { s.branchesErr = map[string]error{"shop-manual": errors.New("connection reset")} })
	code, b := push("d-1")
	if list := decode[apiv1.DeploymentList](t, b).Deployments; code != 202 || len(list) != 1 || list[0].Instance != "shop--dev" {
		t.Fatalf("%d %s", code, b)
	}
	if ids := dep.enqueued(); len(ids) != 1 {
		t.Fatalf("enqueued %v", ids)
	}

	// Nothing deployed and an app failed: 500.
	fs.set(func(s *flakyStore) {
		s.branchesErr = map[string]error{"shop": errors.New("connection reset"), "shop-manual": errors.New("connection reset")}
	})
	if code, b := push("d-2"); code != 500 || errorCode(t, b) != "internal" {
		t.Fatalf("%d %s", code, b)
	}
}
