package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/envs"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/preflight/parity"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/preflight/paritygate"
)

func exampleEnvs(t *testing.T) *envs.Service {
	t.Helper()
	cfg, err := envs.LoadConfig(filepath.Join("..", "..", "examples", "envs.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := envs.New(cfg, envs.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestExampleParityFileLoads(t *testing.T) {
	if _, err := parityGate(filepath.Join("..", "..", "examples", "parity.yaml"), exampleEnvs(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := parityGate(filepath.Join(t.TempDir(), "missing.yaml"), exampleEnvs(t)); err == nil {
		t.Fatal("missing file accepted")
	}
}

// TestParityLive runs the real wiring (parityGate: kubeconfig context ->
// SubjectAccessReview, Prometheus HTTP API) against a cluster and a
// Prometheus. PARITY_LIVE_KUBECONFIG and PARITY_LIVE_CONTEXT name the
// cluster, which has namespace shop with service account shop-api allowed to
// get configmaps there only; PARITY_LIVE_PROMETHEUS scrapes job shop-api (up)
// and job shop-down (down) with label namespace=shop.
func TestParityLive(t *testing.T) {
	kc, kctx, prom := os.Getenv("PARITY_LIVE_KUBECONFIG"), os.Getenv("PARITY_LIVE_CONTEXT"), os.Getenv("PARITY_LIVE_PROMETHEUS")
	if kc == "" || kctx == "" || prom == "" {
		t.Skip("PARITY_LIVE_* not set")
	}
	envSvc, err := envs.New(envs.Config{Environments: []envs.EnvConfig{
		{Name: "src", Provider: "onprem", KubeContext: kctx, Kubeconfig: kc},
		{Name: "dst", Provider: "onprem", KubeContext: kctx, Kubeconfig: kc},
	}}, envs.Options{})
	if err != nil {
		t.Fatal(err)
	}
	p := &plan.Plan{ID: "shop-move", Source: plan.Endpoint{Env: "src", Namespace: "shop"}, Target: plan.Endpoint{Env: "dst", Namespace: "shop"}}
	for _, tc := range []struct {
		name, job, rules string
		identity         parity.Status
		metrics          string
	}{
		{"matching target", "shop-api", "", parity.Pass, "scrape_verified"},
		// The target grants less than the app needs and its scrape target is down.
		{"drifted target", "shop-down", "\n      - {id: list-pods, verb: list, resource: pods, allowed: true}", parity.Fail, "target_not_up"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "parity.yaml")
			doc := `version: 1
mode: audit
environments:
  dst: {prometheus: {url: ` + prom + `, maxAge: 1m}}
apps:
  - name: shop
    namespace: shop
    serviceAccount: shop-api
    prometheusJob: ` + tc.job + `
    rbac:
      - {id: read-own-config, verb: get, resource: configmaps, allowed: true}
      - {id: no-secret-list, verb: list, resource: secrets, allowed: false}
      - {id: no-other-namespace, verb: get, resource: configmaps, namespace: kube-system, allowed: false}` + tc.rules + "\n"
			if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
				t.Fatal(err)
			}
			g, err := parityGate(path, envSvc)
			if err != nil {
				t.Fatal(err)
			}
			r, err := g.Check(context.Background(), p)
			lines := paritygate.Lines(r)
			t.Logf("passed=%v\n%s", r.Passed, strings.Join(lines, "\n"))
			if err != nil || r.Passed || r.Results[0].Capability != parity.Identity || r.Results[0].Status != tc.identity {
				t.Fatalf("identity: %+v %v", r.Results, err)
			}
			if !strings.Contains(strings.Join(lines, "\n"), "metrics-target="+map[bool]string{true: "pass", false: "fail"}[tc.metrics == "scrape_verified"]+"("+tc.metrics+")") {
				t.Fatalf("metrics: %v", lines)
			}
		})
	}
}
