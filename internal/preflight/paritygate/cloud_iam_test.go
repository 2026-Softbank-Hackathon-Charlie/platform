package paritygate

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/preflight/parity"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/preflight/probes"
)

func TestCloudPolicyConfigAndPlanScope(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "examples", "parity-cloud-iam.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := ParseConfig(raw, []string{"eks", "gke", "onprem"})
	if err != nil {
		t.Fatal(err)
	}
	g := New(cfg, nil, nil)
	calls := 0
	g.SetCloudPolicyFetcher(func(_ context.Context, target parity.Target, c probes.CloudIAM, q probes.CloudRule) (probes.CloudDecision, error) {
		calls++
		if target.Environment != "eks" || target.Namespace != "shop" || c.Principal != "arn:aws:iam::123456789012:role/replace-me-app-role" {
			t.Fatal("plan changed cloud policy scope")
		}
		allowed := *q.Allowed
		return probes.CloudDecision{Allowed: &allowed, Complete: false}, nil
	})
	p := shopPlan()
	p.Target.Context = "untrusted"
	p.Target.Kubeconfig = "untrusted"
	r, err := g.Check(context.Background(), p)
	if err != nil || calls != 2 || result(r, parity.Identity).Status != parity.Unknown {
		t.Fatalf("calls=%d %+v %v", calls, r, err)
	}
	p.Target.Env = "onprem"
	calls = 0
	if _, err := g.Check(context.Background(), p); err != nil || calls != 0 {
		t.Fatal("inherited cloud identity into on-prem")
	}
}
