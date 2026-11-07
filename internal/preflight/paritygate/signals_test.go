package paritygate

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/preflight/parity"
)

type signalReaderFunc func(context.Context, parity.SignalScope, time.Time, time.Time) ([]parity.SignalCount, error)

func (f signalReaderFunc) SignalCounts(ctx context.Context, scope parity.SignalScope, from, to time.Time) ([]parity.SignalCount, error) {
	return f(ctx, scope, from, to)
}

func signalGate(t *testing.T, enforce bool) *Gate {
	t.Helper()
	now := time.Now().UTC()
	cfg := Config{Version: 1, Mode: ModeAudit, Environments: map[string]EnvProbes{"gke": {Telemetry: &Telemetry{ClusterName: "gke-cluster", DeploymentEnvironment: "dev", MaxAge: "1m"}}, "eks": {Telemetry: &Telemetry{ClusterName: "eks-cluster", DeploymentEnvironment: "dev", MaxAge: "1m"}}}, Apps: []App{{Name: "shop", Namespace: "shop", Observability: &ObservationRequirement{RequiredSignals: []string{"logs", "traces"}, ServiceNames: []string{"shop-web"}, Enforce: enforce}, Baselines: map[string]Baseline{"gke": {Revision: "r1", ReviewedAt: now.Add(-time.Hour), ExpiresAt: now.Add(time.Hour)}}}}}
	raw, _ := json.Marshal(cfg)
	checked, err := ParseConfig(raw, []string{"gke", "eks"})
	if err != nil {
		t.Fatal(err)
	}
	return New(checked, nil, nil)
}

func TestObservabilityChecksSourceAndTargetFreshSignalsWithoutClaimingAlertProof(t *testing.T) {
	g := signalGate(t, true)
	var mu sync.Mutex
	seen := map[string]bool{}
	g.SetSignalReader(signalReaderFunc(func(_ context.Context, scope parity.SignalScope, from, to time.Time) ([]parity.SignalCount, error) {
		if scope.Target.Namespace != "shop" || scope.Target.Application != "shop" || len(scope.Services) != 1 || scope.Services[0] != "shop-web" || to.Sub(from) != time.Minute {
			t.Error("signal scope or window escaped")
		}
		mu.Lock()
		seen[scope.Target.Environment] = true
		mu.Unlock()
		return []parity.SignalCount{{Signal: "logs", Samples: 1, LastSeen: to.Add(-time.Second), Code: "signal_counted"}, {Signal: "traces", Samples: 1, LastSeen: to.Add(-time.Second), Code: "signal_counted"}}, nil
	}))
	p := shopPlan()
	r, err := g.Check(context.Background(), p)
	if err != nil || !seen["gke"] || !seen["eks"] {
		t.Fatalf("%+v %v", r, err)
	}
	obs := result(r, parity.Observability)
	if obs.Status != parity.Unknown || r.Passed {
		t.Fatal("receiving samples claimed untested alert/other controls")
	}
	if enforced, allowed := g.ObservabilityDecision(p, &r); !enforced || !allowed {
		t.Fatal("explicit required signals did not pass their narrow gate")
	}
	if len(obs.Checks) != 5 {
		t.Fatalf("source/target signal evidence missing: %+v", obs)
	}
}

func TestRequiredSignalsUnknownStaleErrorAndPartialAlwaysBlockOnlyOptIn(t *testing.T) {
	for _, fault := range []string{"missing", "stale", "future", "unavailable", "partial", "duplicate", "not configured"} {
		t.Run(fault, func(t *testing.T) {
			g := signalGate(t, true)
			if fault != "not configured" {
				g.SetSignalReader(signalReaderFunc(func(_ context.Context, scope parity.SignalScope, from, to time.Time) ([]parity.SignalCount, error) {
					out := []parity.SignalCount{{Signal: "logs", Samples: 1, LastSeen: to.Add(-time.Second), Code: "signal_counted"}, {Signal: "traces", Samples: 1, LastSeen: to.Add(-time.Second), Code: "signal_counted"}}
					if scope.Target.Environment == "eks" {
						switch fault {
						case "missing":
							out[0].Samples = 0
						case "stale":
							out[0].LastSeen = from.Add(-time.Second)
						case "future":
							out[0].LastSeen = to.Add(time.Minute)
						case "unavailable":
							return nil, errors.New("TOPSECRET_PROVIDER_ERROR")
						case "partial":
							out = out[:1]
						case "duplicate":
							out = append(out, out[0])
						}
					}
					return out, nil
				}))
			}
			r, err := g.Check(context.Background(), shopPlan())
			if err != nil {
				t.Fatal(err)
			}
			if enforced, allowed := g.ObservabilityDecision(shopPlan(), &r); !enforced || allowed {
				t.Fatalf("%s allowed: %+v", fault, r)
			}
			if strings.Contains(strings.Join(Lines(r), "\n"), "TOPSECRET_PROVIDER_ERROR") {
				t.Fatal("provider error leaked")
			}
			audit := signalGate(t, false)
			if enforced, _ := audit.ObservabilityDecision(shopPlan(), nil); enforced {
				t.Fatal("existing audit mode unexpectedly enforced")
			}
		})
	}
}

func TestSignalConfigurationAndReportBinding(t *testing.T) {
	g := signalGate(t, true)
	for _, mutate := range []func(*Config){
		func(c *Config) { c.Environments["eks"].Telemetry.ClusterName = "gke-cluster" },
		func(c *Config) { c.Environments["eks"].Telemetry.DeploymentEnvironment = "" },
		func(c *Config) { c.Apps[0].Observability.RequiredSignals = []string{"logs", "logs"} },
		func(c *Config) { c.Apps[0].Observability.ServiceNames = nil },
	} {
		raw, _ := json.Marshal(g.cfg)
		var candidate Config
		_ = json.Unmarshal(raw, &candidate)
		mutate(&candidate)
		raw, _ = json.Marshal(candidate)
		if _, err := ParseConfig(raw, []string{"gke", "eks"}); err == nil {
			t.Fatal("unsafe signal configuration accepted")
		}
	}
	g.SetSignalReader(signalReaderFunc(func(_ context.Context, _ parity.SignalScope, _, to time.Time) ([]parity.SignalCount, error) {
		return []parity.SignalCount{{Signal: "logs", Samples: 1, LastSeen: to, Code: "signal_counted"}, {Signal: "traces", Samples: 1, LastSeen: to, Code: "signal_counted"}}, nil
	}))
	for _, mutate := range []func(*parity.Report){
		func(r *parity.Report) { r.ConfigDigest = "different-policy" },
		func(r *parity.Report) { r.BaselineStatus = "expired_or_future" },
		func(r *parity.Report) { r.ExpiresAt = time.Now().Add(-time.Second) },
		func(r *parity.Report) { r.ObservedAt = time.Now().Add(-2 * time.Minute) },
		func(r *parity.Report) {
			for i := range r.Results {
				if r.Results[i].Capability == parity.Observability {
					r.Results[i].Target.Environment = "gke"
				}
			}
		},
	} {
		r, err := g.Check(context.Background(), shopPlan())
		if err != nil {
			t.Fatal(err)
		}
		mutate(&r)
		if enforced, allowed := g.ObservabilityDecision(shopPlan(), &r); !enforced || allowed {
			t.Fatal("foreign, stale or unreviewed report accepted")
		}
	}
}

func TestStatelessSignalScopeUsesExactRegisteredAppRoute(t *testing.T) {
	g := signalGate(t, true)
	p := &plan.Plan{Source: plan.Endpoint{Kind: "none", Env: "gke"}, Target: plan.Endpoint{Kind: "none", Env: "eks"}, Gateway: plan.GatewaySpec{URL: "https://gateway/apps/shop"}}
	if !g.ObservabilityConfigured(p) {
		t.Fatal("database-free app escaped required signals")
	}
	r, err := g.Check(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if enforce, allow := g.ObservabilityDecision(p, &r); !enforce || allow {
		t.Fatal("missing stateless evidence accepted")
	}
	if p.Target.Namespace != "" {
		t.Fatal("scope resolution mutated caller plan")
	}
	p.Gateway.URL += "-other"
	if g.ObservabilityConfigured(p) {
		t.Fatal("app-prefix route crossed namespaces")
	}
}
