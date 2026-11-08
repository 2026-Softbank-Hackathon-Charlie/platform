package deploy

import (
	"strings"
	"testing"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
)

// TestCheckAppDeploysTheRealAppsSpec: an onboarding's check app
// (onb-<hex>, ADR 0042) deploys a platform.yaml that names the real app,
// under its own name: its files and namespace are the check app's. Before,
// it failed with "platform.yaml at ... names the app "shop", but it is
// registered as "onb-...""; any other app still does.
func TestCheckAppDeploysTheRealAppsSpec(t *testing.T) {
	te := newTestEnv(t)
	const check = "onb-37802837f0c3"
	te.register(check, "gke")
	te.register("store", "gke")
	te.start()
	te.github.push("main", sha("pr"), map[string]string{"platform.yaml": shopSpec, "Dockerfile": "x", "worker/Dockerfile": "x"})

	d := te.deployment(check, sha("pr"), "gke")
	te.enqueue(d.ID)
	if got := te.wait(d.ID); got.Status != store.DeployHealthy {
		t.Fatalf("check app: %s %q", got.Status, got.Error)
	}
	files, _ := te.git.snapshot()
	b, ok := files[FilePath(check, "gke")]
	if !ok || !strings.Contains(string(b), `namespace: "`+check+`"`) || strings.Contains(string(b), `app: "shop"`) {
		t.Fatalf("check app file %v:\n%s", ok, b)
	}
	if _, ok := files[FilePath("shop", "gke")]; ok {
		t.Fatal("the check app wrote the real app's file")
	}

	d = te.deployment("store", sha("pr"), "gke")
	te.enqueue(d.ID)
	if got := te.wait(d.ID); !strings.Contains(got.Error, `platform.yaml at `+short(sha("pr"))+` names the app "shop", but it is registered as "store"`) {
		t.Fatalf("another app: %s %q", got.Status, got.Error)
	}
}
