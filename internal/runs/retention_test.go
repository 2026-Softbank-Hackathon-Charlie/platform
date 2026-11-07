package runs

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/retention"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store/storetest"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

type testLease struct{}

func (testLease) Check(context.Context) error { return nil }
func (testLease) Release()                    {}

type retentionBackend struct {
	checks, removes int
	checkErr        error
	done            bool
	removing        func(Job retention.Job)
}

func (b *retentionBackend) Capture(context.Context, *plan.Plan) (retention.Job, error) {
	return retention.Job{SourceUID: "source", TargetUID: "target", Pods: []retention.Pod{{Name: "old-1", UID: "pod"}}}, nil
}
func (b *retentionBackend) Check(context.Context, *plan.Plan, retention.Job, time.Time) error {
	b.checks++
	return b.checkErr
}
func (b *retentionBackend) Remove(_ context.Context, _ *plan.Plan, j retention.Job) (bool, error) {
	b.removes++
	if b.removing != nil {
		b.removing(j)
	}
	return b.done, nil
}
func retentionFixture(t *testing.T) (*Runner, *retentionBackend, *plan.Plan, apiv1.Run) {
	t.Helper()
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	ended := now.Add(-time.Hour)
	yes := true
	p := &plan.Plan{ID: "p", Source: plan.Endpoint{Env: "a", Context: "a", Namespace: "app", Cluster: "old"}, Target: plan.Endpoint{Env: "b", Context: "b", Namespace: "app", Cluster: "new"}}
	b := &retentionBackend{done: true}
	st := storetest.NewMemory()
	r := New(t.Context(), Config{Store: st, Retention: b, RetentionPeriod: 2 * time.Hour, RetentionPlan: func(context.Context, string) (*plan.Plan, error) { return p, nil }, ClusterLease: func(context.Context, []string) (store.ClusterLease, error) { return testLease{}, nil }})
	r.now = func() time.Time { return now }
	doc := apiv1.Run{RunId: "run-1", PlanRef: "plan-1", PlanId: "p", Status: apiv1.RunStatusSucceeded, StartedAt: ended.Add(-time.Hour), EndedAt: &ended, UpdatedAt: ended, Source: apiv1.PlanEndpoint{Env: "a", Namespace: "app", Cluster: "old"}, Target: apiv1.PlanEndpoint{Env: "b", Namespace: "app", Cluster: "new"}, Result: apiv1.RunResult{Verified: &yes}}
	putRun(t, st, doc)
	j, _ := b.Capture(t.Context(), p)
	j.RunID = doc.RunId
	j.Status = "captured"
	j.KeepFor = 2 * time.Hour
	if err := r.saveRetirement(t.Context(), j); err != nil {
		t.Fatal(err)
	}
	return r, b, p, doc
}
func putRun(t *testing.T, st store.Store, doc apiv1.Run) {
	t.Helper()
	row, err := runRow(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateRun(t.Context(), row); err != nil {
		t.Fatal(err)
	}
}
func latestJob(t *testing.T, r *Runner) *retention.Job {
	t.Helper()
	j, err := r.retirementJob(t.Context(), "run-1")
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func TestRetentionDeadlineSurvivesRestart(t *testing.T) {
	r, b, _, doc := retentionFixture(t)
	if err := r.RetireSources(t.Context()); err != nil {
		t.Fatal(err)
	}
	j := latestJob(t, r)
	want := doc.EndedAt.Add(2 * time.Hour)
	if j.Status != "scheduled" || !j.DueAt.Equal(want) || b.removes != 0 {
		t.Fatalf("job=%+v removes=%d", j, b.removes)
	}
	restarted := New(t.Context(), r.cfg)
	restarted.now = func() time.Time { return want }
	b.removing = func(j retention.Job) {
		if latestJob(t, restarted).Status != "deleting" || j.Status != "deleting" {
			t.Fatal("delete ran before persistent intent")
		}
	}
	if err := restarted.RetireSources(t.Context()); err != nil {
		t.Fatal(err)
	}
	if latestJob(t, restarted).Status != "deleted" || b.removes != 1 {
		t.Fatal("due retirement did not complete")
	}
	if err := restarted.RetireSources(t.Context()); err != nil {
		t.Fatal(err)
	}
	if b.removes != 1 {
		t.Fatal("completed deletion repeated")
	}
}
func TestRetentionDoesNotDeleteOnMissingEvidence(t *testing.T) {
	r, b, _, _ := retentionFixture(t)
	r.now = func() time.Time { return time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC) }
	b.checkErr = errors.New("private credentials must not be exposed")
	if err := r.RetireSources(t.Context()); err != nil {
		t.Fatal(err)
	}
	j := latestJob(t, r)
	if j.Status != "blocked" || j.Reason != "safety_check_failed" || b.removes != 0 {
		t.Fatalf("job=%+v removes=%d", j, b.removes)
	}
	b.checkErr = nil
	if err := r.RetireSources(t.Context()); err != nil {
		t.Fatal(err)
	}
	if b.removes != 1 {
		t.Fatal("resolved safety block did not retry")
	}
}
func TestNewAttemptCancelsOldRetirement(t *testing.T) {
	r, b, p, _ := retentionFixture(t)
	r.mu.Lock()
	err := r.retirementGuard(t.Context(), p, "")
	r.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if latestJob(t, r).Status != "cancelled" {
		t.Fatal("old retirement left enabled")
	}
	r.now = func() time.Time { return time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC) }
	if err := r.RetireSources(t.Context()); err != nil {
		t.Fatal(err)
	}
	if b.removes != 0 {
		t.Fatal("superseded source deleted")
	}
}
func TestDeletingSourceCannotBeRolledBackOrReused(t *testing.T) {
	r, _, p, _ := retentionFixture(t)
	j := latestJob(t, r)
	j.Status = "deleting"
	if err := r.saveRetirement(t.Context(), *j); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	err := r.retirementGuard(t.Context(), p, "")
	r.mu.Unlock()
	if !errors.Is(err, ErrWrongStatus) {
		t.Fatalf("source reuse=%v", err)
	}
	p.Source, p.Target = p.Target, p.Source
	r.mu.Lock()
	err = r.retirementGuard(t.Context(), p, "")
	r.mu.Unlock()
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("rollback race=%v", err)
	}
}
func TestNoRetroactiveEnrollmentAndIncompleteObserve(t *testing.T) {
	r, b, _, doc := retentionFixture(t)
	doc.RunId = "historical"
	putRun(t, r.cfg.Store, doc)
	doc.RunId = "run-1"
	doc.Observe = &apiv1.ObserveResult{}
	row, _ := runRow(doc)
	if err := r.cfg.Store.UpdateRun(t.Context(), row); err != nil {
		t.Fatal(err)
	}
	r.now = func() time.Time { return time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC) }
	if err := r.RetireSources(t.Context()); err != nil {
		t.Fatal(err)
	}
	if b.removes != 0 || latestJob(t, r).Status != "blocked" {
		t.Fatal("unverified run deleted")
	}
	j, err := r.retirementJob(t.Context(), "historical")
	if err != nil || j != nil {
		t.Fatal("historical run enrolled")
	}
}

func TestRestoredEndpointCanMigrateAgain(t *testing.T) {
	r, _, p, retired := retentionFixture(t)
	j := latestJob(t, r)
	j.Status = "deleted"
	if err := r.saveRetirement(t.Context(), *j); err != nil {
		t.Fatal(err)
	}
	finished := retired.EndedAt.Add(2 * time.Hour)
	restored := apiv1.Run{RunId: "restore", PlanRef: "restore-plan", Status: apiv1.RunStatusSucceeded, StartedAt: retired.EndedAt.Add(time.Hour), EndedAt: &finished, Source: retired.Target, Target: retired.Source}
	putRun(t, r.cfg.Store, restored)
	r.mu.Lock()
	err := r.retirementGuard(t.Context(), p, "")
	r.mu.Unlock()
	if err != nil {
		t.Fatalf("restored incarnation blocked: %v", err)
	}
}

func TestDeletedSourceRejectsImmediateRollback(t *testing.T) {
	r, _, p, _ := retentionFixture(t)
	j := latestJob(t, r)
	j.Status = "deleted"
	if err := r.saveRetirement(t.Context(), *j); err != nil {
		t.Fatal(err)
	}
	p.Source, p.Target = p.Target, p.Source
	_, err := r.Start(t.Context(), StartOptions{Plan: p, PlanRef: "rollback", RollbackOf: "run-1"})
	if !errors.Is(err, ErrWrongStatus) {
		t.Fatalf("rollback after deletion=%v", err)
	}
}

type failIntentStore struct{ store.Store }

func (s failIntentStore) AppendEvent(ctx context.Context, e store.Event) (store.Event, error) {
	var j retention.Job
	_ = json.Unmarshal(e.Data, &j)
	if e.Kind == retention.Event && j.Status == "deleting" {
		return store.Event{}, errors.New("disk full")
	}
	return s.Store.AppendEvent(ctx, e)
}
func TestIntentPersistenceFailurePreventsDeletion(t *testing.T) {
	r, b, _, _ := retentionFixture(t)
	r.cfg.Store = failIntentStore{r.cfg.Store}
	r.now = func() time.Time { return time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC) }
	if err := r.RetireSources(t.Context()); err == nil {
		t.Fatal("persistence failure ignored")
	}
	if b.removes != 0 {
		t.Fatal("delete without durable intent")
	}
}
