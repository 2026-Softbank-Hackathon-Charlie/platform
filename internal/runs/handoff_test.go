package runs

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/engine"
	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store/storetest"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

// stateDir plays the engine's state directory on the volume both API
// processes mount: a resumed engine continues from the saved state.
type stateDir struct {
	mu     sync.Mutex
	states map[string]*engine.State
	ran    map[string]int           // RunStep calls per step, over all engines
	block  map[string]chan struct{} // a step waits for its channel to close
	inStep map[string]chan struct{} // closed when that step starts
	next   int
}

func newStateDir() *stateDir {
	return &stateDir{states: map[string]*engine.State{}, ran: map[string]int{}, block: map[string]chan struct{}{}, inStep: map[string]chan struct{}{}}
}

func (d *stateDir) hold(step string) (started <-chan struct{}, release func()) {
	b, s := make(chan struct{}), make(chan struct{})
	d.mu.Lock()
	d.block[step], d.inStep[step] = b, s
	d.mu.Unlock()
	return s, func() { close(b) }
}

func (d *stateDir) factory(_ *plan.Plan, o engine.Options) (Engine, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	id := o.RunID
	if id == "" {
		d.next++
		id = fmt.Sprintf("run_%d", d.next)
		st := &engine.State{RunID: id, Outcome: engine.OutcomeRunning, Steps: map[string]*engine.StepRecord{}}
		for _, s := range engine.Steps {
			st.Steps[s] = &engine.StepRecord{Status: engine.StatusPending}
		}
		d.states[id] = st
	}
	if d.states[id] == nil {
		return nil, fmt.Errorf("no state for %s", id)
	}
	return &dirEngine{id: id, d: d}, nil
}

type dirEngine struct {
	id string
	d  *stateDir
}

func (e *dirEngine) RunID() string                      { return e.id }
func (e *dirEngine) ResumeHalted(context.Context) error { return nil }
func (e *dirEngine) Close()                             {}

func (e *dirEngine) State() engine.State {
	e.d.mu.Lock()
	defer e.d.mu.Unlock()
	st := *e.d.states[e.id]
	st.Steps = map[string]*engine.StepRecord{}
	for k, v := range e.d.states[e.id].Steps {
		c := *v
		st.Steps[k] = &c
	}
	return st
}

func (e *dirEngine) RunStep(ctx context.Context, step string) error {
	e.d.mu.Lock()
	st := e.d.states[e.id]
	st.Steps[step].Status = engine.StatusRunning
	e.d.ran[step]++
	block, started := e.d.block[step], e.d.inStep[step]
	if step == engine.StepFence {
		st.WritesHeld = true
	}
	e.d.mu.Unlock()
	if block != nil {
		close(started)
		<-block // a real step does not stop for a handoff either
	}
	e.d.mu.Lock()
	defer e.d.mu.Unlock()
	switch step {
	case engine.StepSwitchover:
		st.PromoteRequested, st.SourceDemoted = true, true
	case engine.StepCutover:
		st.WritesHeld, st.TrafficOnTarget = false, true
	case engine.StepFinalize:
		st.Outcome = engine.OutcomeSucceeded
	}
	st.Steps[step].Status = engine.StatusSucceeded
	return nil
}

func handoffPlan(t *testing.T) *plan.Plan {
	t.Helper()
	p, err := plan.Parse([]byte(`{"id":"handoff","source":{"env":"gke","namespace":"n","cluster":"a","dsn":"postgres://x@a/db"},"target":{"env":"eks","namespace":"n","cluster":"b","dsn":"postgres://x@b/db"}}`))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func waitStatus(t *testing.T, r *Runner, id string, want apiv1.RunStatus) apiv1.Run {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		run, err := r.Get(context.Background(), id)
		if err == nil && run.Status == want {
			return run
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %s is %s (%v), want %s", id, run.Status, err, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func waitDone(t *testing.T, r *Runner) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.Wait(ctx); err != nil {
		t.Fatal("runs did not stop")
	}
}

// Two servers on one store and one state directory: the old one hands a
// run waiting for switch approval over, the new one resumes it where it was.
func TestHandOffAwaitingSwitchResumesOnNewLeader(t *testing.T) {
	st, dir, p := storetest.NewMemory(), newStateDir(), handoffPlan(t)
	load := func(context.Context, string) (*plan.Plan, error) { return p, nil }
	old := New(context.Background(), Config{Store: st, Factory: dir.factory})
	run, err := old.Start(context.Background(), StartOptions{PlanRef: "pln_1", Plan: p, Actor: "op"})
	if err != nil {
		t.Fatal(err)
	}
	waitStatus(t, old, run.RunId, apiv1.RunStatusAwaitingSwitch)

	if n := old.HandOff(); n != 1 {
		t.Fatalf("waiting runs = %d", n)
	}
	waitDone(t, old)
	got := waitStatus(t, old, run.RunId, apiv1.RunStatusInterrupted)
	if got.StatusReason == nil || *got.StatusReason != HandoffReason {
		t.Fatalf("reason = %v", got.StatusReason)
	}

	next := New(context.Background(), Config{Store: st, Factory: dir.factory})
	if n, err := next.Recover(context.Background()); err != nil || n != 0 {
		t.Fatalf("Recover = %d, %v", n, err)
	}
	if n, err := next.ResumeHandedOff(context.Background(), load); err != nil || n != 1 {
		t.Fatalf("ResumeHandedOff = %d, %v", n, err)
	}
	waitStatus(t, next, run.RunId, apiv1.RunStatusAwaitingSwitch)
	if _, err := next.Switch(context.Background(), run.RunId, "op"); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, next, run.RunId, apiv1.RunStatusSucceeded)
	for _, s := range engine.Steps {
		if dir.ran[s] != 1 {
			t.Errorf("step %s ran %d times", s, dir.ran[s])
		}
	}
	// Resumed once: a second call finds nothing.
	if n, err := next.ResumeHandedOff(context.Background(), load); err != nil || n != 0 {
		t.Fatalf("again = %d, %v", n, err)
	}
}

// A run inside the write hold is not stopped: the old server finishes it
// before it lets go, and nothing is left for the new one.
func TestHandOffFinishesRunInsideWriteHold(t *testing.T) {
	st, dir, p := storetest.NewMemory(), newStateDir(), handoffPlan(t)
	started, release := dir.hold(engine.StepSwitchover)
	old := New(context.Background(), Config{Store: st, Factory: dir.factory})
	run, err := old.Start(context.Background(), StartOptions{PlanRef: "pln_1", Plan: p, Actor: "op", AutoSwitch: true})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	old.HandOff()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	if err := old.Wait(ctx); err == nil {
		t.Fatal("handoff did not wait for the run inside the hold")
	}
	cancel()
	release()
	waitDone(t, old)
	waitStatus(t, old, run.RunId, apiv1.RunStatusSucceeded)

	next := New(context.Background(), Config{Store: st, Factory: dir.factory})
	if n, err := next.ResumeHandedOff(context.Background(), func(context.Context, string) (*plan.Plan, error) { return p, nil }); err != nil || n != 0 {
		t.Fatalf("ResumeHandedOff = %d, %v", n, err)
	}
}

// A run copying data finishes the step it is in, stops before the next
// one, and the new server goes on from there.
func TestHandOffStopsBetweenSyncSteps(t *testing.T) {
	st, dir, p := storetest.NewMemory(), newStateDir(), handoffPlan(t)
	started, release := dir.hold(engine.StepPresync)
	old := New(context.Background(), Config{Store: st, Factory: dir.factory})
	run, err := old.Start(context.Background(), StartOptions{PlanRef: "pln_1", Plan: p, Actor: "op", AutoSwitch: true})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	old.HandOff()
	release()
	waitDone(t, old)
	waitStatus(t, old, run.RunId, apiv1.RunStatusInterrupted)
	if dir.ran[engine.StepPresync] != 1 || dir.ran[engine.StepDelta] != 0 {
		t.Fatalf("ran = %v", dir.ran)
	}

	next := New(context.Background(), Config{Store: st, Factory: dir.factory})
	if n, err := next.ResumeHandedOff(context.Background(), func(context.Context, string) (*plan.Plan, error) { return p, nil }); err != nil || n != 1 {
		t.Fatalf("ResumeHandedOff = %d, %v", n, err)
	}
	waitStatus(t, next, run.RunId, apiv1.RunStatusSucceeded)
	if dir.ran[engine.StepPresync] != 1 || dir.ran[engine.StepDelta] != 1 {
		t.Fatalf("ran = %v", dir.ran)
	}
}

// Only runs stopped for a handoff resume by themselves; one a crash left
// behind (Recover) waits for an operator.
func TestResumeHandedOffSkipsOtherInterruptedRuns(t *testing.T) {
	st := storetest.NewMemory()
	reason := "the platform restarted while this run was active; resume it to continue"
	doc := apiv1.Run{RunId: "run_x", PlanRef: "pln_1", Status: apiv1.RunStatusInterrupted, StatusReason: &reason}
	row, err := runRow(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateRun(context.Background(), row); err != nil {
		t.Fatal(err)
	}
	r := New(context.Background(), Config{Store: st, Factory: newStateDir().factory})
	n, err := r.ResumeHandedOff(context.Background(), func(context.Context, string) (*plan.Plan, error) {
		t.Fatal("loaded a plan")
		return nil, nil
	})
	if err != nil || n != 0 {
		t.Fatalf("= %d, %v", n, err)
	}
}
