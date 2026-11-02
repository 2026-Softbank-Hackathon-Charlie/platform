package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/runs"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

func observeReq(ref string, auto bool, duration string) apiv1.StartMigrationRequest {
	return apiv1.StartMigrationRequest{PlanRef: ref, AutoSwitch: ptr(true),
		Observe: &apiv1.ObserveOptions{Duration: ptr(duration), StepDuration: ptr("20ms"), AutoRollback: ptr(auto)}}
}

func (h *harness) setTarget(s apiv1.BackendStats) {
	h.traffic.mu.Lock()
	h.traffic.stats["eks"] = s
	h.traffic.mu.Unlock()
}

// rollbacksOf lists the runs that roll back id.
func (h *harness) rollbacksOf(id string) []apiv1.Run {
	h.t.Helper()
	list, err := h.runner.List(context.Background())
	if err != nil {
		h.t.Fatal(err)
	}
	var out []apiv1.Run
	for _, r := range list {
		if deref(r.RollbackOf) == id {
			out = append(out, r)
		}
	}
	return out
}

func TestObservePasses(t *testing.T) {
	h := newHarness(t)
	h.setTarget(apiv1.BackendStats{Requests: 400, ErrorRate: 0.001, P95Millis: 90})
	id := h.start(observeReq(h.approved(planJSON("p1", "a", "b", true)), true, "60ms"))

	run := h.waitRun(id, ended(apiv1.RunStatusSucceeded))
	o := run.Observe
	if o == nil || o.Passed == nil || !*o.Passed || o.Violation != nil || o.StoppedReason != nil || len(o.Steps) < 2 {
		t.Fatalf("observe: %+v", o)
	}
	for _, s := range o.Steps {
		if s.Verdict != apiv1.ObserveStepVerdictOk {
			t.Fatalf("step %+v", s)
		}
	}
	if run.StatusReason != nil {
		t.Errorf("status reason %q on a clean run", *run.StatusReason)
	}
	h.validateSchema("Run", mustJSON(t, run))
	h.idle()
	if len(h.rollbacksOf(id)) != 0 {
		t.Fatal("a passing observation must not roll back")
	}
}

// Without autoRollback a breach is recorded and the operator decides.
func TestObserveViolationLeavesTheDecisionToAnOperator(t *testing.T) {
	h := newHarness(t)
	h.setTarget(apiv1.BackendStats{Requests: 400, ErrorRate: 0.2, P95Millis: 90})
	id := h.start(observeReq(h.approved(planJSON("p1", "a", "b", true)), false, "1s"))

	run := h.waitRun(id, ended(apiv1.RunStatusSucceeded))
	o := run.Observe
	if o.Passed == nil || *o.Passed || !strings.Contains(deref(o.Violation), "error rate 20.00%") || len(o.Steps) != 1 ||
		o.Steps[0].Verdict != apiv1.ObserveStepVerdictViolated {
		t.Fatalf("observe: %+v", o)
	}
	if !strings.Contains(deref(run.StatusReason), "SLO violated after cutover") || !strings.Contains(deref(run.StatusReason), "/rollback") {
		t.Fatalf("status reason: %q", deref(run.StatusReason))
	}
	h.idle()
	if len(h.rollbacksOf(id)) != 0 || o.RollbackRunId != nil {
		t.Fatal("rolled back without autoRollback")
	}
	if code, b := h.do("POST", "/v1/migrations/"+id+"/rollback", opToken, map[string]any{"reason": "SLO"}); code != 202 {
		t.Fatalf("one-click rollback after a violation: %d %s", code, b)
	}
}

func TestObserveViolationRollsBackAutomatically(t *testing.T) {
	h := newHarness(t)
	h.setTarget(apiv1.BackendStats{Requests: 400, ErrorRate: 0, P95Millis: 2500})
	id := h.start(observeReq(h.approved(planJSON("p1", "a", "b", true)), true, "1s"))

	run := h.waitRun(id, func(r apiv1.Run) bool { return r.Observe != nil && r.Observe.RollbackRunId != nil })
	if run.Status != apiv1.RunStatusSucceeded || !strings.Contains(deref(run.Observe.Violation), "p95 2500ms") ||
		!strings.Contains(deref(run.StatusReason), "starting the automatic rollback") {
		t.Fatalf("run: status %s reason %q observe %+v", run.Status, deref(run.StatusReason), run.Observe)
	}
	rb := h.waitRun(*run.Observe.RollbackRunId, ended(apiv1.RunStatusSucceeded))
	if deref(rb.RollbackOf) != id || rb.StartedBy != runs.AutoRollbackActor || rb.Source.Env != "eks" || rb.Target.Env != "gke" {
		t.Fatalf("rollback run: %+v", rb)
	}
	h.validateSchema("Run", mustJSON(t, run))

	audit, err := h.store.ListAudit(context.Background(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range audit {
		if a.Actor == runs.AutoRollbackActor && a.Status == 202 && strings.Contains(string(a.Detail), *run.Observe.RollbackRunId) {
			found = true
		}
	}
	if !found {
		t.Fatal("automatic rollback missing from the audit log")
	}
	events, err := h.runner.Events(context.Background(), id, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !hasKind(events, runs.EventObserveRollback) || !hasKind(events, runs.EventObserveStep) {
		t.Fatal("observe events missing")
	}
}

// Too little traffic or a gateway outage is not a breach: no rollback.
func TestObserveInconclusiveDoesNotRollBack(t *testing.T) {
	h := newHarness(t)
	h.setTarget(apiv1.BackendStats{Requests: 3, ErrorRate: 1, P95Millis: 5000})
	id := h.start(observeReq(h.approved(planJSON("p1", "a", "b", true)), true, "60ms"))

	run := h.waitRun(id, ended(apiv1.RunStatusSucceeded))
	o := run.Observe
	if o.Passed != nil || o.Violation != nil || !strings.Contains(deref(o.StoppedReason), "enough requests") {
		t.Fatalf("observe: %+v", o)
	}
	for _, s := range o.Steps {
		if s.Verdict != apiv1.ObserveStepVerdictInconclusive {
			t.Fatalf("step %+v", s)
		}
	}
	h.idle()
	if len(h.rollbacksOf(id)) != 0 {
		t.Fatal("rolled back on inconclusive data")
	}
}

func TestAbortDuringObserveKeepsTheMigration(t *testing.T) {
	h := newHarness(t)
	h.setTarget(apiv1.BackendStats{Requests: 400, ErrorRate: 0, P95Millis: 50})
	id := h.start(observeReq(h.approved(planJSON("p1", "a", "b", true)), true, "30s"))
	h.waitRun(id, func(r apiv1.Run) bool { return r.Phase == apiv1.RunPhaseObserve })

	if code, b := h.do("POST", "/v1/migrations/"+id+"/abort", opToken, map[string]string{"reason": "seen enough"}); code != 200 {
		t.Fatalf("abort: %d %s", code, b)
	}
	run := h.waitRun(id, ended(apiv1.RunStatusSucceeded))
	if !strings.Contains(deref(run.Observe.StoppedReason), "stopped by an operator") || run.Observe.Passed != nil {
		t.Fatalf("observe: %+v", run.Observe)
	}
}

func TestObserveOptionsValidation(t *testing.T) {
	h := newHarness(t)
	noGw := h.approved(planJSON("p1", "a", "b", false))
	if code, b := h.do("POST", "/v1/migrations", opToken, observeReq(noGw, false, "1m")); code != 400 || errCode(t, b) != "invalid_request" {
		t.Fatalf("observe without gateway: %d %s", code, b)
	}
	ref := h.approved(planJSON("p2", "c", "d", true))
	for _, d := range []string{"2h", "x"} {
		if code, b := h.do("POST", "/v1/migrations", opToken, observeReq(ref, false, d)); code != 400 {
			t.Fatalf("duration %s: %d %s", d, code, b)
		}
	}
}

func hasKind(events []apiv1.RunEvent, kind string) bool {
	for _, e := range events {
		if e.Kind == kind {
			return true
		}
	}
	return false
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
