package runs

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/engine"
	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/retention"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store/storetest"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

// heldEngine stays in its first step until the run is cancelled, so the run
// stays active while the test looks at it.
type heldEngine struct{ id string }

func (e heldEngine) RunID() string { return e.id }
func (e heldEngine) RunStep(ctx context.Context, _ string) error {
	<-ctx.Done()
	return ctx.Err()
}
func (heldEngine) ResumeHalted(context.Context) error { return nil }
func (e heldEngine) State() engine.State {
	return engine.State{RunID: e.id, Steps: map[string]*engine.StepRecord{}}
}
func (heldEngine) Close() {}

type quietTraffic struct{}

func (quietTraffic) State(context.Context, time.Duration) (apiv1.TrafficState, error) {
	return apiv1.TrafficState{}, nil
}
func (quietTraffic) SetReadWeights(context.Context, map[string]int) error { return nil }

// capturing fails the test if a source database is captured for retirement.
type capturing struct {
	retentionBackend
	t *testing.T
}

func (c *capturing) Capture(context.Context, *plan.Plan) (retention.Job, error) {
	c.t.Error("retention captured the source of a plan without a database")
	return retention.Job{}, errors.New("no source database")
}

// statelessPlan is a plan without a database as the engine decodes it
// (ADR 0049): endpoints with only an environment, the app's gateway scope.
func statelessPlan(app string) *plan.Plan {
	return &plan.Plan{ID: app + "-eks-to-gke", Source: plan.Endpoint{Kind: plan.KindNone, Env: "eks"}, Target: plan.Endpoint{Kind: plan.KindNone, Env: "gke"},
		Gateway: plan.GatewaySpec{Mode: "http", URL: "http://gw:9901/apps/" + app, SourceBackend: "eks", TargetBackend: "gke"}}
}

// TestStatelessRunWithCanaryAndObserve: a run without a database accepts
// canary and observe like any run through the gateway, records its app,
// captures nothing for retention, and holds its gateway scope, not a
// cluster, against another run of the same app.
func TestStatelessRunWithCanaryAndObserve(t *testing.T) {
	var leased [][]string
	n := 0
	ctx, stop := context.WithCancel(t.Context())
	r := New(ctx, Config{StateDir: t.TempDir(), Store: storetest.NewMemory(), Traffic: quietTraffic{}, MinCanaryStep: time.Millisecond,
		Retention: &capturing{t: t}, RetentionPeriod: time.Hour, RetentionPlan: func(context.Context, string) (*plan.Plan, error) { return nil, nil },
		ClusterLease: func(_ context.Context, keys []string) (store.ClusterLease, error) {
			leased = append(leased, keys)
			return testLease{}, nil
		},
		Factory: func(p *plan.Plan, _ engine.Options) (Engine, error) {
			n++
			return heldEngine{id: p.ID + "-run" + string(rune('0'+n))}, nil
		}})
	app, step := "docker-doom", "5m"
	opts := StartOptions{PlanRef: "pln_1", Plan: statelessPlan(app), App: &app, Actor: "op",
		Canary:  &apiv1.CanaryOptions{Weights: []int{10, 50}, StepDuration: &step},
		Observe: &apiv1.ObserveOptions{}}
	run, err := r.Start(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if run.App == nil || *run.App != app || run.Canary == nil {
		t.Fatalf("run %+v", run)
	}
	if len(leased) != 1 || !slices.Equal(leased[0], []string{"gateway:http://gw:9901/apps/docker-doom"}) {
		t.Fatalf("leased %v", leased)
	}
	if _, err := r.Start(t.Context(), StartOptions{PlanRef: "pln_2", Plan: statelessPlan(app), App: &app}); !errors.Is(err, ErrBusy) {
		t.Fatalf("a second run of the same app: %v, want ErrBusy", err)
	}
	other := "album"
	if _, err := r.Start(t.Context(), StartOptions{PlanRef: "pln_3", Plan: statelessPlan(other), App: &other}); err != nil {
		t.Fatalf("a run of another app: %v", err)
	}
	stop() // the held engines return; the runs end
	wait, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.Wait(wait); err != nil {
		t.Fatal("runs did not end")
	}
}
