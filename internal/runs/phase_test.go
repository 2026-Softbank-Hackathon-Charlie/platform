package runs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/engine"
	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/gateway"
	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

// Every engine step has a run phase; start-target (ADR 0012 of the engine)
// runs while writes are held, between switchover and cutover.
func TestEveryStepHasAPhase(t *testing.T) {
	for _, s := range engine.Steps {
		if _, ok := stepPhase[s]; !ok {
			t.Errorf("step %s has no phase", s)
		}
	}
	if stepPhase[engine.StepStartTarget] != apiv1.RunPhaseCutover {
		t.Fatalf("start-target phase = %s", stepPhase[engine.StepStartTarget])
	}
	r := &Runner{}
	l := &live{doc: apiv1.Run{Phase: apiv1.RunPhaseAwaitingSwitch, Engine: apiv1.EngineState{Steps: map[string]apiv1.StepRecord{}}}}
	step := engine.StepStartTarget
	r.apply(l, apiv1.RunEvent{Kind: engine.EventStepStarted, Step: &step})
	if l.doc.Phase != apiv1.RunPhaseCutover {
		t.Fatalf("phase after start-target started = %s", l.doc.Phase)
	}
}

// A state file written before start-target existed resumes: the step is
// pending, so the runner runs it (a warm plan skips it) before cutover.
func TestStateFromBeforeStartTargetResumes(t *testing.T) {
	dir := t.TempDir()
	steps := map[string]any{}
	for _, s := range []string{"precheck", "prepare", "presync", "delta"} {
		steps[s] = map[string]any{"status": "succeeded"}
	}
	for _, s := range []string{"fence", "switchover", "cutover", "verify", "finalize"} {
		steps[s] = map[string]any{"status": "pending"}
	}
	old, _ := json.Marshal(map[string]any{"runId": "old-1", "planId": "old", "outcome": "failed", "steps": steps})
	if err := os.WriteFile(filepath.Join(dir, "old-1.json"), old, 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := plan.Parse([]byte(`{"id":"old","source":{"env":"gke","namespace":"n","cluster":"a","dsn":"postgres://x@a/db"},"target":{"env":"eks","namespace":"n","cluster":"b","dsn":"postgres://x@b/db"}}`))
	if err != nil {
		t.Fatal(err)
	}
	e, err := engine.New(p, engine.Options{StateDir: dir, RunID: "old-1", SourceCluster: slowFailingCluster{}, TargetCluster: slowFailingCluster{}, Gateway: gateway.None{}})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if stepSucceeded(e, engine.StepStartTarget) {
		t.Fatal("start-target counts as done")
	}
	if rec := e.State().Steps[engine.StepStartTarget]; rec == nil || rec.Status != engine.StatusPending {
		t.Fatalf("start-target record = %+v", rec)
	}
	if st := engineState(e.State()); st.Steps[engine.StepStartTarget].Status != apiv1.StepStatusPending {
		t.Fatalf("run document start-target = %+v", st.Steps[engine.StepStartTarget])
	}
}
