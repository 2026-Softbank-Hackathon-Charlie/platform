package runs

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/engine"
	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store/storetest"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

type securityStepEngine struct {
	heldEngine
	state  engine.State
	before func(string)
}

func (e *securityStepEngine) State() engine.State { return e.state }
func (e *securityStepEngine) RunStep(_ context.Context, step string) error {
	if e.before != nil {
		e.before(step)
	}
	now := time.Now()
	e.state.Steps[step] = &engine.StepRecord{Status: engine.StatusSucceeded, StartedAt: &now}
	if step == engine.StepFinalize {
		e.state.Outcome = engine.OutcomeSucceeded
	}
	return nil
}

func TestMigrationSecurityPersistsBeforePrecheckAndCutover(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	st := storetest.NewMemory()
	e := &securityStepEngine{heldEngine: heldEngine{id: "record-security"}, state: engine.State{Steps: map[string]*engine.StepRecord{}}}
	e.before = func(step string) {
		if step != engine.StepPrecheck && step != engine.StepCutover {
			return
		}
		row, err := st.GetRun(ctx, e.RunID())
		var doc apiv1.Run
		if err != nil || json.Unmarshal(row.Doc, &doc) != nil || doc.SecurityComparison == nil {
			t.Errorf("no persisted security comparison before %s (read error %v)", step, err)
			return
		}
		comp := doc.SecurityComparison
		if comp.ControlId != "HC-POL-02" || comp.Source == nil || comp.Source.Result != apiv1.SecurityControlResultPass || comp.Source.AssessedAt.IsZero() {
			t.Errorf("source checkpoint before %s = %+v", step, comp.Source)
		}
		if step == engine.StepPrecheck && comp.Target != nil {
			t.Error("target was assessed before startup")
		}
		if step == engine.StepCutover && (comp.Target == nil || comp.Target.Result != apiv1.SecurityControlResultFail || comp.Target.AssessedAt.IsZero()) {
			t.Errorf("target checkpoint before cutover = %+v", comp.Target)
		}
	}
	r := New(ctx, Config{
		Store:   st,
		Factory: func(*plan.Plan, engine.Options) (Engine, error) { return e, nil },
		MigrationSecurity: func(_ context.Context, _ *plan.Plan, _ apiv1.Run) SecurityAssessor {
			return func(_ context.Context, endpoint apiv1.PlanEndpoint) apiv1.RunSecurityAssessment {
				a := apiv1.RunSecurityAssessment{PolicyVersion: "test-v1", Workload: "shop / Deployment/shop-web", Result: apiv1.SecurityControlResultPass, Reason: "clear"}
				if endpoint.Env == "gke" {
					if step := e.state.Steps[engine.StepStartTarget]; step == nil || step.Status != engine.StatusSucceeded {
						t.Error("target assessed before start-target finished")
					}
					a.Result, a.Reason = apiv1.SecurityControlResultFail, "privileged"
				}
				return a
			}
		},
	})
	p := &plan.Plan{ID: "shop", Source: plan.Endpoint{Env: "eks", Namespace: "shop", Cluster: "src"}, Target: plan.Endpoint{Env: "gke", Namespace: "shop", Cluster: "dst"}}
	run, err := r.Start(ctx, StartOptions{Plan: p, PlanRef: "plan-shop", AutoSwitch: true, Source: apiv1.PlanEndpoint{Env: "eks"}, Target: apiv1.PlanEndpoint{Env: "gke"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := r.Get(ctx, run.RunId)
	if err != nil || got.Status != apiv1.RunStatusSucceeded || got.SecurityComparison == nil || got.SecurityComparison.Target == nil {
		t.Fatalf("completed run = %+v, err = %v", got, err)
	}
}

func TestMigrationSecurityDoesNotRecreateAnAttemptedCheckpoint(t *testing.T) {
	now := time.Now()
	e := &securityStepEngine{state: engine.State{Steps: map[string]*engine.StepRecord{
		engine.StepPrecheck: {Status: engine.StatusSucceeded, StartedAt: &now},
		engine.StepCutover:  {Status: engine.StatusFailed, StartedAt: &now},
	}}}
	l := &live{eng: e, securityAssessor: func(context.Context, apiv1.PlanEndpoint) apiv1.RunSecurityAssessment {
		t.Error("attempted checkpoint was collected again")
		return apiv1.RunSecurityAssessment{}
	}}
	l.q.init()
	r := &Runner{}
	if err := r.runSteps(t.Context(), l, []string{engine.StepPrecheck, engine.StepCutover}); err != nil {
		t.Fatal(err)
	}
	if l.doc.SecurityComparison != nil {
		t.Fatal("historic evidence was fabricated for an attempted run")
	}
}

type failCheckpointOnce struct {
	store.Store
	failed bool
}

func (s *failCheckpointOnce) UpdateRun(ctx context.Context, row store.Run) error {
	if !s.failed {
		s.failed = true
		return errors.New("temporary run store failure")
	}
	return s.Store.UpdateRun(ctx, row)
}

func TestMigrationSecurityRetriesTransientCheckpointWrite(t *testing.T) {
	st := &failCheckpointOnce{Store: storetest.NewMemory()}
	r := New(t.Context(), Config{Store: st})
	doc := apiv1.Run{RunId: "retry-checkpoint"}
	row, err := runRow(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateRun(t.Context(), row); err != nil {
		t.Fatal(err)
	}
	l := &live{
		id: doc.RunId, doc: doc, eng: heldEngine{id: doc.RunId},
		securityAssessor: func(context.Context, apiv1.PlanEndpoint) apiv1.RunSecurityAssessment {
			return apiv1.RunSecurityAssessment{
				Result: apiv1.SecurityControlResultUnknown, Reason: apiv1.RunSecurityAssessmentReasonForbidden,
				PolicyVersion: "test-v1", Workload: "shop / App/shop",
			}
		},
	}
	l.q.init()
	go r.writer(l)
	defer func() { l.q.close(); <-l.q.drained }()
	r.recordMigrationSecurity(t.Context(), l, engine.StepPrecheck)
	saved, err := st.GetRun(t.Context(), doc.RunId)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(saved.Doc, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.SecurityComparison == nil || doc.SecurityComparison.Source == nil ||
		doc.SecurityComparison.Source.Reason != apiv1.RunSecurityAssessmentReasonForbidden {
		t.Fatalf("the failed write was not retried before continuing: %+v", doc.SecurityComparison)
	}
}

func TestMigrationSecurityRefreshesBeforeFirstCutoverAttempt(t *testing.T) {
	st := storetest.NewMemory()
	r := New(t.Context(), Config{Store: st})
	old := time.Now().Add(-time.Hour)
	doc := apiv1.Run{RunId: "pending-cutover", SecurityComparison: &apiv1.RunSecurityComparison{
		ControlId: "HC-POL-02",
		Source:    &apiv1.RunSecurityAssessment{AssessedAt: old, Result: apiv1.SecurityControlResultPass},
		Target:    &apiv1.RunSecurityAssessment{AssessedAt: old, Result: apiv1.SecurityControlResultPass},
	}}
	row, err := runRow(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateRun(t.Context(), row); err != nil {
		t.Fatal(err)
	}
	l := &live{id: doc.RunId, doc: doc, eng: heldEngine{id: doc.RunId}, securityAssessor: func(context.Context, apiv1.PlanEndpoint) apiv1.RunSecurityAssessment {
		return apiv1.RunSecurityAssessment{Result: apiv1.SecurityControlResultFail, Reason: apiv1.RunSecurityAssessmentReasonPrivileged}
	}}
	l.q.init()
	go r.writer(l)
	defer func() { l.q.close(); <-l.q.drained }()
	r.recordMigrationSecurity(t.Context(), l, engine.StepCutover)
	comparison := l.snapshot().SecurityComparison
	if comparison.Target.Result != apiv1.SecurityControlResultFail || !comparison.Target.AssessedAt.After(old) || !comparison.Source.AssessedAt.Equal(old) {
		t.Fatalf("unexpected refreshed comparison: %+v / %+v", comparison.Source, comparison.Target)
	}
}

type cancellationSensitiveLease struct{ store.ClusterLease }

func (cancellationSensitiveLease) Check(ctx context.Context) error { return ctx.Err() }

func TestMigrationSecurityLeavesCancellationToEngine(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	called := false
	e := &securityStepEngine{state: engine.State{Steps: map[string]*engine.StepRecord{}}, before: func(string) { called = true }}
	l := &live{eng: e, lease: cancellationSensitiveLease{}, securityAssessor: func(context.Context, apiv1.PlanEndpoint) apiv1.RunSecurityAssessment {
		cancel()
		return apiv1.RunSecurityAssessment{Result: apiv1.SecurityControlResultUnknown}
	}}
	l.q.init()
	r := New(t.Context(), Config{Store: storetest.NewMemory()})
	if err := r.runSteps(ctx, l, []string{engine.StepCutover}); err != nil || !called {
		t.Fatalf("cancelled capture must reach engine boundary: called=%v err=%v", called, err)
	}
}
