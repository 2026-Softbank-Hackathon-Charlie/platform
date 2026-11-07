package runs

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/engine"
	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/pg"
	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/retention"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store/storetest"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

type fakeExternal struct{}

func (fakeExternal) Status(context.Context) (engine.ExternalStatus, error) {
	return engine.ExternalStatus{}, nil
}
func (fakeExternal) Fence(context.Context) (pg.Fence, error) { return pg.Fence{}, nil }
func (fakeExternal) Unfence(context.Context) error           { return nil }

type capturing struct {
	*retentionBackend
	captures int
}

func (c *capturing) Capture(ctx context.Context, p *plan.Plan) (retention.Job, error) {
	c.captures++
	return c.retentionBackend.Capture(ctx, p)
}

// TestExternalSourceWiring: a plan with an external source (ADR 0042) gets
// Config.External as the engine's source adapter, captures no retention
// (there is no source Cluster) and is keyed by its target app.
func TestExternalSourceWiring(t *testing.T) {
	p := &plan.Plan{ID: "onb-3f9a1c2b7d4e-1", Source: plan.Endpoint{Kind: plan.SourceExternal, Name: "legacy", Env: "legacy"},
		Target: plan.Endpoint{Env: "eks", Context: "eks", Namespace: "shop", Cluster: "shop-db"}}
	ext := fakeExternal{}
	var got engine.ExternalSource
	b := &capturing{retentionBackend: &retentionBackend{done: true}}
	called, external := false, false
	r := New(context.Background(), Config{Store: storetest.NewMemory(), Retention: b, RetentionPeriod: time.Hour,
		RetentionPlan: func(context.Context, string) (*plan.Plan, error) { return p, nil },
		External: func(q *plan.Plan) engine.ExternalSource {
			if q != p {
				t.Fatal("wrong plan")
			}
			external = true
			return ext
		},
		Factory: func(_ *plan.Plan, opts engine.Options) (Engine, error) {
			called, got = true, opts.External
			return nil, errors.New("stop before the engine runs")
		}})
	if _, err := r.launch(context.Background(), p, "", apiv1.Run{}, false); !errors.Is(err, ErrEngineInit) || !called {
		t.Fatalf("launch: %v", err)
	}
	if !external || got != engine.ExternalSource(ext) {
		t.Fatalf("the engine got external source %#v", got)
	}
	if b.captures != 0 {
		t.Fatalf("retention captured an external source %d times", b.captures)
	}
	if k := clusterKeys(p); k[0] != "external/legacy/shop" || k[1] != "eks/shop/shop-db" {
		t.Fatalf("keys %q", k)
	}
}

// TestCNPGPlanIsNotExternal: every plan the pinned engine parses keeps its
// source cluster key and retention.
func TestCNPGPlanIsNotExternal(t *testing.T) {
	p := &plan.Plan{Source: plan.Endpoint{Context: "a", Namespace: "n", Cluster: "c"}, Target: plan.Endpoint{Context: "b", Namespace: "n", Cluster: "d"}}
	if p.Source.External() || clusterKeys(p)[0] != "a/n/c" {
		t.Fatal("a CNPG source was taken for an external one")
	}
}
