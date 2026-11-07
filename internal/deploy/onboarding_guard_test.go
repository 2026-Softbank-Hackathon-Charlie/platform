package deploy

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	onboardjob "github.com/2026-Softbank-Hackathon-Charlie/platform/internal/onboard/job"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
)

// TestCuttingOverGuard: with the onboarding runner's guard, a deployment of
// an app whose onboarding cuts over fails, and once the run succeeded
// (postRun "run") the platform's own redeploy goes through (ADR 0042).
func TestCuttingOverGuard(t *testing.T) {
	se := newStorageEnv(t)
	se.github.push("main", sha("p1"), map[string]string{"platform.yaml": pgSpec, "Dockerfile": "FROM scratch\n"})
	se.register("shop", "gke")
	ctx := context.Background()
	now := time.Now().UTC()
	doc := func(postRun ...string) []byte {
		b, _ := json.Marshal(map[string]any{"id": "onb-0123456789ab", "internal": map[string]any{"postRun": postRun}})
		return b
	}
	row := store.Onboarding{ID: "onb-0123456789ab", App: "shop", Repo: "org/shop", State: store.OnboardingCuttingOver, Doc: doc(), CreatedAt: now, UpdatedAt: now}
	if err := se.store.CreateOnboarding(ctx, row); err != nil {
		t.Fatal(err)
	}
	se.cfg.CuttingOver = onboardjob.CuttingOver(se.store)
	se.start()
	d := se.deploy(sha("p1"), "gke")
	if d.Status != store.DeployFailed || !strings.Contains(d.Error, "cutting over") {
		t.Fatalf("deployment while cutting over: %s %q", d.Status, d.Error)
	}
	got, err := se.store.GetOnboarding(ctx, row.ID)
	if err != nil {
		t.Fatal(err)
	}
	got.Doc = doc("run")
	if _, err := se.store.UpdateOnboarding(ctx, got, []string{store.OnboardingCuttingOver}); err != nil {
		t.Fatal(err)
	}
	if d := se.deploy(sha("p1"), "gke"); d.Status != store.DeployHealthy {
		t.Fatalf("deployment after the run succeeded: %s %q", d.Status, d.Error)
	}
}
