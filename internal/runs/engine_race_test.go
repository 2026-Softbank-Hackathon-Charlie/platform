package runs

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/cnpg"
	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/engine"
	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/gateway"
	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store/storetest"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

// slowFailingCluster keeps precheck busy long enough for the writer goroutine
// to persist the run while the engine is inside the step.
type slowFailingCluster struct{}

func (slowFailingCluster) Get(ctx context.Context) (*cnpg.Status, error) {
	time.Sleep(30 * time.Millisecond)
	return nil, errors.New("cluster unreachable")
}
func (slowFailingCluster) ApplyManifest(context.Context, string) error { return nil }
func (slowFailingCluster) SetPrimary(context.Context, string, string) error {
	return nil
}
func (slowFailingCluster) WaitFor(context.Context, time.Duration, func(*cnpg.Status) bool) (*cnpg.Status, error) {
	return nil, errors.New("unreachable")
}

// TestRealEngineHasNoRace drives the real engine (whose State shares step
// records with the running step) under -race.
func TestRealEngineHasNoRace(t *testing.T) {
	p, err := plan.Parse([]byte(`{"id":"race","source":{"env":"gke","namespace":"n","cluster":"a","dsn":"postgres://x@a/db"},"target":{"env":"eks","namespace":"n","cluster":"b","dsn":"postgres://x@b/db"}}`))
	if err != nil {
		t.Fatal(err)
	}
	factory := func(p *plan.Plan, o engine.Options) (Engine, error) {
		o.SourceCluster, o.TargetCluster, o.Gateway = slowFailingCluster{}, slowFailingCluster{}, gateway.None{}
		return engine.New(p, o)
	}
	r := New(context.Background(), Config{StateDir: t.TempDir(), Store: storetest.NewMemory(), Factory: factory})
	run, err := r.Start(context.Background(), StartOptions{PlanRef: "pln_x", Plan: p, Actor: "op"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.Wait(ctx); err != nil {
		t.Fatal("run did not finish")
	}
	got, err := r.Get(context.Background(), run.RunId)
	if err != nil || got.Status != apiv1.RunStatusFailed || got.Engine.Steps["precheck"].Status != apiv1.StepStatusFailed {
		t.Fatalf("run = %v %v %+v", got.Status, err, got.Engine.Steps["precheck"])
	}
}
