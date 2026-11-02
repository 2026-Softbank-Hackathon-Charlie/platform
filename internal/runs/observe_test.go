package runs

import (
	"context"
	"testing"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store/storetest"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

// A restart during observe keeps the migration: the data already moved.
func TestRecoverDuringObserve(t *testing.T) {
	ctx := context.Background()
	st := storetest.NewMemory()
	now := time.Now().UTC()
	mk := func(id string, outcome apiv1.EngineOutcome, phase apiv1.RunPhase) {
		doc := apiv1.Run{RunId: id, PlanRef: "pln_1", PlanId: "p", Status: apiv1.RunStatusRunning, Phase: phase,
			StartedBy: "op", StartedAt: now, UpdatedAt: now,
			Engine:  apiv1.EngineState{Outcome: outcome, Steps: map[string]apiv1.StepRecord{}},
			Observe: &apiv1.ObserveResult{Steps: []apiv1.ObserveStep{}}}
		row, err := runRow(doc)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.CreateRun(ctx, row); err != nil {
			t.Fatal(err)
		}
	}
	mk("observing", apiv1.EngineOutcomeSucceeded, apiv1.RunPhaseObserve)
	mk("syncing", apiv1.EngineOutcomeRunning, apiv1.RunPhaseSync)

	r := New(ctx, Config{StateDir: t.TempDir(), Store: st})
	if n, err := r.Recover(ctx); err != nil || n != 2 {
		t.Fatalf("recover = %d, %v", n, err)
	}
	obs, err := r.Get(ctx, "observing")
	if err != nil {
		t.Fatal(err)
	}
	if obs.Status != apiv1.RunStatusSucceeded || obs.Observe.StoppedReason == nil {
		t.Fatalf("observing run after restart: %s, observe %+v", obs.Status, obs.Observe)
	}
	syn, err := r.Get(ctx, "syncing")
	if err != nil {
		t.Fatal(err)
	}
	if syn.Status != apiv1.RunStatusInterrupted {
		t.Fatalf("syncing run after restart: %s", syn.Status)
	}
}
