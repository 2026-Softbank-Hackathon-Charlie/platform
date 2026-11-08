package api

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/runs"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

// finishDeployment records a deployment as built and finished with status.
func (h *harness) finishDeployment(id, status string) store.Deployment {
	h.t.Helper()
	ctx := context.Background()
	d, err := h.store.GetDeployment(ctx, id)
	if err != nil {
		h.t.Fatal(err)
	}
	now := storeTime()
	d.Status, d.FinishedAt = status, &now
	d.Images = []store.DeploymentImage{{Service: "web", Image: "r/web@sha256:aa"}, {Service: "worker", Image: "r/worker@sha256:bb"}}
	d.Spec = []byte(`{"name":"shop","services":[]}`)
	out, err := h.store.UpdateDeployment(ctx, d)
	if err != nil {
		h.t.Fatal(err)
	}
	return out
}

func TestRedeploy(t *testing.T) {
	gh, dep := shopGitHub(), &fakeDeployer{}
	h := newHarness(t, withApps(""), withGitHub(gh, dep))
	first := h.registerShop()

	redeploy := map[string]any{"redeploy": true}
	// The first deployment is still queued.
	code, b := h.do("POST", "/v1/apps/shop/deployments", opToken, redeploy)
	if code != 409 || errCode(t, b) != "deployment_in_progress" || !strings.Contains(string(b), first.Id) {
		t.Fatalf("while deploying: %d %s", code, b)
	}
	h.finishDeployment(first.Id, store.DeployFailed)
	code, b = h.do("POST", "/v1/apps/shop/deployments", opToken, redeploy)
	if code != 409 || errCode(t, b) != "nothing_to_redeploy" {
		t.Fatalf("no healthy deployment: %d %s", code, b)
	}

	// A healthy one, then a failed one: the healthy one is repeated.
	code, b = h.do("POST", "/v1/apps/shop/deployments", opToken, nil)
	if code != 202 {
		t.Fatalf("deploy: %d %s", code, b)
	}
	healthy := h.finishDeployment(decode[apiv1.Deployment](t, b).Id, store.DeployHealthy)
	code, b = h.do("POST", "/v1/apps/shop/deployments", opToken, nil)
	if code != 202 {
		t.Fatalf("deploy: %d %s", code, b)
	}
	h.finishDeployment(decode[apiv1.Deployment](t, b).Id, store.DeployFailed)

	reads := gh.readCount()
	code, b = h.do("POST", "/v1/apps/shop/deployments", opToken, map[string]any{"redeploy": true, "environments": []string{"eks"}})
	if code != 202 {
		t.Fatalf("redeploy: %d %s", code, b)
	}
	d := decode[apiv1.Deployment](t, b)
	if d.Trigger != apiv1.DeploymentTriggerRedeploy || d.Commit != healthy.Commit || d.Ref != healthy.Ref || d.Instance != "shop" ||
		len(d.Images) != 2 || d.Images[0].Image != "r/web@sha256:aa" || len(d.Environments) != 1 || d.Environments[0].Env != "eks" {
		t.Fatalf("redeploy %+v", d)
	}
	if gh.readCount() != reads {
		t.Fatal("a redeploy read GitHub")
	}
	stored, _ := h.store.GetDeployment(context.Background(), d.Id)
	if string(stored.Spec) != string(healthy.Spec) {
		t.Fatalf("redeploy spec %s, want %s", stored.Spec, healthy.Spec)
	}
	if ids := dep.enqueued(); ids[len(ids)-1] != d.Id {
		t.Fatalf("enqueued %v", ids)
	}
	if a := h.lastAudit(); !strings.Contains(string(a.Detail), `"redeploy":true`) || !strings.Contains(string(a.Detail), healthy.ID) {
		t.Fatalf("audit %s", a.Detail)
	}

	if code, b := h.do("POST", "/v1/apps/shop/deployments", opToken, map[string]any{"redeploy": true, "ref": "main"}); code != 400 || errCode(t, b) != "invalid_request" {
		t.Fatalf("ref with redeploy: %d %s", code, b)
	}
	if code, b := h.do("POST", "/v1/apps/shop/deployments", opToken, map[string]any{"redeploy": true, "branch": "nope"}); code != 400 {
		t.Fatalf("unknown branch: %d %s", code, b)
	}
}

func TestRedeployEnvironments(t *testing.T) {
	gh, dep := shopGitHub(), &fakeDeployer{}
	h := newHarness(t, withApps(""), withGitHub(gh, dep))
	first := h.registerShop() // to gke and eks
	h.finishDeployment(first.Id, store.DeployHealthy)
	// This test needs a strictly later deployment. On coarse system clocks
	// two HTTP requests can share created_at, whose tie-breaker is random ID.
	deadline := time.Now().Add(5 * time.Second)
	for !storeTime().After(first.CreatedAt) {
		if time.Now().After(deadline) {
			t.Fatal("clock did not advance past first deployment")
		}
		time.Sleep(time.Millisecond)
	}
	code, b := h.do("POST", "/v1/apps/shop/deployments", opToken, map[string]any{"environments": []string{"gke"}})
	if code != 202 {
		t.Fatalf("deploy to gke: %d %s", code, b)
	}
	gkeOnly := h.finishDeployment(decode[apiv1.Deployment](t, b).Id, store.DeployHealthy)

	// By default only the environments the last healthy deployment went
	// to: eks may run another version.
	code, b = h.do("POST", "/v1/apps/shop/deployments", opToken, map[string]any{"redeploy": true})
	if code != 202 {
		t.Fatalf("redeploy: %d %s", code, b)
	}
	d := decode[apiv1.Deployment](t, b)
	if len(d.Environments) != 1 || d.Environments[0].Env != "gke" || d.Commit != gkeOnly.Commit {
		t.Fatalf("redeploy went to %+v (first=%s, later=%s)", d.Environments, first.CreatedAt.Format(time.RFC3339Nano), gkeOnly.CreatedAt.Format(time.RFC3339Nano))
	}
	if a := h.lastAudit(); !strings.Contains(string(a.Detail), `"environments":["gke"]`) {
		t.Fatalf("audit %s", a.Detail)
	}
	h.finishDeployment(d.Id, store.DeployHealthy)

	// Asking for an environment it did not go to is refused.
	for _, envs := range [][]string{{"eks"}, {"gke", "eks"}} {
		code, b = h.do("POST", "/v1/apps/shop/deployments", opToken, map[string]any{"redeploy": true, "environments": envs})
		if code != 409 || errCode(t, b) != "nothing_to_redeploy" || !strings.Contains(string(b), "did not go to eks") {
			t.Fatalf("redeploy to %v: %d %s", envs, code, b)
		}
	}

	// The last healthy deployment went only to an environment the app no
	// longer deploys to.
	old, err := h.store.GetDeployment(context.Background(), d.Id)
	if err != nil {
		t.Fatal(err)
	}
	old.ID, old.Status, old.FinishedAt, old.CreatedAt = "dep_onprem", store.DeployQueued, nil, storeTime().Add(time.Second)
	old.Environments = []store.DeploymentEnvironment{{Env: "onprem", Status: "pending"}}
	if err := h.store.CreateDeployment(context.Background(), old); err != nil {
		t.Fatal(err)
	}
	h.finishDeployment(old.ID, store.DeployHealthy)
	code, b = h.do("POST", "/v1/apps/shop/deployments", opToken, map[string]any{"redeploy": true})
	if code != 409 || errCode(t, b) != "nothing_to_redeploy" || !strings.Contains(string(b), "no longer deploys to") {
		t.Fatalf("redeploy of a dropped environment: %d %s", code, b)
	}
}

func TestRedeployWithoutGitHub(t *testing.T) {
	gh := shopGitHub()
	h := newHarness(t, withApps(""), withGitHub(gh, nil))
	first := h.registerShop()
	h.finishDeployment(first.Id, store.DeployHealthy)
	h.api.d.GitHub = nil // a redeploy needs no GitHub
	code, b := h.do("POST", "/v1/apps/shop/deployments", opToken, map[string]any{"redeploy": true})
	if code != 202 || decode[apiv1.Deployment](t, b).Trigger != apiv1.DeploymentTriggerRedeploy {
		t.Fatalf("redeploy without GitHub: %d %s", code, b)
	}
}

const s3ShopSpec = shopSpec + "  - {name: photos, type: s3}\n"

func TestRegisterS3AppNeedsStorage(t *testing.T) {
	var configured map[string]bool
	gh := shopGitHub()
	gh.repo(shopRepo, "main", headSHA, s3ShopSpec, "Dockerfile", "worker/Dockerfile")
	gh.repo(shopRepo, "dev", devSHA, s3ShopSpec, "Dockerfile", "worker/Dockerfile")
	h := newHarness(t, withApps(""), withGitHub(gh, nil), func(_ *harness, _ *runs.Config, d *Deps) {
		d.StorageConfigured = func(env string) bool { return configured[env] }
	})
	register := func() (int, []byte) {
		return h.do("POST", "/v1/apps", opToken, map[string]any{"repo": shopRepo, "environments": []string{"gke", "eks"}})
	}
	configured = map[string]bool{"gke": true}
	if code, b := register(); code != 503 || errCode(t, b) != "storage_not_configured" || !strings.Contains(string(b), "eks") {
		t.Fatalf("eks without storage: %d %s", code, b)
	}
	configured = map[string]bool{"gke": true, "eks": true}
	if code, b := register(); code != 201 {
		t.Fatalf("with storage: %d %s", code, b)
	}
	configured = map[string]bool{"gke": true}
	if code, b := h.do("POST", "/v1/apps/shop/instances", opToken, map[string]any{"branch": "dev"}); code != 503 || errCode(t, b) != "storage_not_configured" {
		t.Fatalf("instance without storage: %d %s", code, b)
	}
}

func TestHeadlampIsReserved(t *testing.T) {
	gh := newFakeGitHub()
	gh.repo("acme/headlamp", "main", headSHA, strings.Replace(shopSpec, "name: shop", "name: headlamp", 1), "Dockerfile", "worker/Dockerfile")
	h := newHarness(t, withApps(""), withGitHub(gh, nil))
	code, b := h.do("POST", "/v1/apps", opToken, map[string]any{"repo": "acme/headlamp", "environments": []string{"gke"}})
	if code != 409 || errCode(t, b) != "app_exists" {
		t.Fatalf("%d %s", code, b)
	}
}

func (f *fakeGitHub) readCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads
}
