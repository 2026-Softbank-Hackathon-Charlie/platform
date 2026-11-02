package api

import (
	"strings"
	"testing"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

// jsonEsc is the start of a JSON unicode escape (backslash, "u"), built at
// run time so this file holds no escape that a tool could decode.
var jsonEsc = string(rune(92)) + "u"

// A JSON escape hides "$" from a check of the raw body; the server must
// still refuse the reference, and never answer with the variable's value.
func TestEscapedEnvReferenceIsRejected(t *testing.T) {
	const secret = "TOPSECRET-platform-db"
	t.Setenv("PLATFORM_DATABASE_URL", "postgres://platform:"+secret+"@platform-db.internal:5432/platform")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "TOPSECRET-aws")
	h := newHarness(t)
	dollar := jsonEsc + "0024"
	for name, dsn := range map[string]string{
		"escaped ${VAR} as the host":     "postgres://app:${SRC_PGPASSWORD}@" + dollar + "{AWS_SECRET_ACCESS_KEY}/app",
		"escaped $VAR as the host":       "postgres://app:${SRC_PGPASSWORD}@" + dollar + "AWS_SECRET_ACCESS_KEY/app",
		"escaped ${VAR} as the dsn":      dollar + "{PLATFORM_DATABASE_URL}",
		"escaped ${VAR} as the password": "postgres://app:" + dollar + "{PLATFORM_DATABASE_URL}@db-gke.shop.ts.net/app",
		"reference in the query":         "postgres://app:${SRC_PGPASSWORD}@db-gke.shop.ts.net/app?application_name=${DST_PGPASSWORD}",
	} {
		body := strings.Replace(planJSON("p1", "a", "b", false), "postgres://app:${SRC_PGPASSWORD}@db-gke.shop.ts.net/app", dsn, 1)
		code, b := h.do("POST", "/v1/plans", agentToken, body)
		if code != 400 || errCode(t, b) != "invalid_plan" || strings.Contains(string(b), "TOPSECRET") || strings.Contains(string(b), "platform-db") {
			t.Errorf("%s: %d %s", name, code, b)
		}
	}
	_, b := h.do("GET", "/v1/plans", viewerToken, nil)
	if l := decode[apiv1.PlanList](t, b); len(l.Plans) != 0 {
		t.Fatalf("refused plans were stored: %s", b)
	}
}

func rollbackPlans(t *testing.T, h *harness, runID string) []apiv1.PlanRecord {
	t.Helper()
	_, b := h.do("GET", "/v1/plans", viewerToken, nil)
	var out []apiv1.PlanRecord
	for _, p := range decode[apiv1.PlanList](t, b).Plans {
		if deref(p.RollbackOf) == runID {
			out = append(out, p)
		}
	}
	return out
}

// succeededWithBlocker returns a succeeded run and a run that holds the same
// clusters while it waits for /switch, so rollback starts fail with cluster_busy.
func succeededWithBlocker(t *testing.T, h *harness) (id, blocker string) {
	t.Helper()
	id = h.start(apiv1.StartMigrationRequest{PlanRef: h.approved(moimroomPlan("p1", "gke-ctx", "eks-ctx", "")), AutoSwitch: ptr(true)})
	h.waitRun(id, ended(apiv1.RunStatusSucceeded))
	h.idle()
	blocker = h.start(apiv1.StartMigrationRequest{PlanRef: h.approved(moimroomPlan("blk", "gke-ctx", "eks-ctx", ""))})
	h.waitRun(blocker, status(apiv1.RunStatusAwaitingSwitch))
	return id, blocker
}

func (h *harness) abortAndIdle(id string) {
	h.t.Helper()
	if code, b := h.do("POST", "/v1/migrations/"+id+"/abort", opToken, nil); code != 200 {
		h.t.Fatalf("abort: %d %s", code, b)
	}
	h.waitRun(id, ended(apiv1.RunStatusAborted))
	h.idle()
}

// The reviewer's bypass: two rollbacks fail to start, the operator starts one
// of their plans by hand, and a third rollback started another run.
func TestRollbackAtMostOnceAfterFailedStarts(t *testing.T) {
	h := newHarness(t)
	id, blocker := succeededWithBlocker(t, h)
	for i := range 2 {
		if code, b := h.do("POST", "/v1/migrations/"+id+"/rollback", opToken, nil); code != 409 || errCode(t, b) != "cluster_busy" {
			t.Fatalf("rollback %d while busy: %d %s", i+1, code, b)
		}
	}
	plans := rollbackPlans(t, h, id)
	if len(plans) != 1 || plans[0].Status != apiv1.PlanStatusApproved {
		t.Fatalf("rollback plans after two failed starts: %+v", plans)
	}

	h.abortAndIdle(blocker)
	manual := h.start(apiv1.StartMigrationRequest{PlanRef: plans[0].Ref})
	if run := h.waitRun(manual, status(apiv1.RunStatusAwaitingSwitch)); deref(run.RollbackOf) != id {
		t.Fatalf("a rollback plan started by hand: rollbackOf = %q", deref(run.RollbackOf))
	}

	code, b := h.do("POST", "/v1/migrations/"+id+"/rollback", opToken, nil)
	if code != 409 || errCode(t, b) != "run_state_conflict" || !strings.Contains(string(b), manual) {
		t.Fatalf("rollback after the manual start: %d %s", code, b)
	}
	if plans := rollbackPlans(t, h, id); len(plans) != 1 {
		t.Fatalf("rollback plans: %+v", plans)
	}
	_, b = h.do("GET", "/v1/migrations", viewerToken, nil)
	n := 0
	for _, r := range decode[apiv1.RunList](t, b).Runs {
		if deref(r.RollbackOf) == id {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d runs roll back %s", n, id)
	}
}

func TestRollbackRetryStartsTheSamePlan(t *testing.T) {
	h := newHarness(t)
	id, blocker := succeededWithBlocker(t, h)
	if code, b := h.do("POST", "/v1/migrations/"+id+"/rollback", opToken, map[string]any{"reason": "first"}); code != 409 || errCode(t, b) != "cluster_busy" {
		t.Fatalf("rollback while busy: %d %s", code, b)
	}
	first := rollbackPlans(t, h, id)
	h.abortAndIdle(blocker)

	code, b := h.do("POST", "/v1/migrations/"+id+"/rollback", opToken, map[string]any{"reason": "second", "autoSwitch": false})
	if code != 202 {
		t.Fatalf("retry: %d %s", code, b)
	}
	resp := decode[apiv1.StartMigrationResponse](t, b)
	if len(first) != 1 || resp.PlanRef != first[0].Ref {
		t.Fatalf("retry started plan %s, want the first rollback plan %+v", resp.PlanRef, first)
	}
	run := h.waitRun(resp.RunId, status(apiv1.RunStatusAwaitingSwitch))
	if deref(run.RollbackOf) != id || run.Source.Env != "eks" {
		t.Fatalf("rollback run: %+v", run)
	}
	if plans := rollbackPlans(t, h, id); len(plans) != 1 || plans[0].Status != apiv1.PlanStatusStarted {
		t.Fatalf("rollback plans: %+v", plans)
	}
}

func TestWrittenRecordsInput(t *testing.T) {
	h := newHarness(t)
	path := "/v1/written-records/moimroom-demo"
	for name, rec := range map[string]string{
		"NUL in pk":      `{"table":"app.reservations","pk":"4` + jsonEsc + `00002"}`,
		"newline in pk":  `{"table":"app.reservations","pk":"4` + jsonEsc + `000a2"}`,
		"DEL in pk":      `{"table":"app.reservations","pk":"4` + jsonEsc + `007f2"}`,
		"NUL in table":   `{"table":"app.reservations` + jsonEsc + `0000","pk":"1"}`,
		"tab in table":   `{"table":"app.` + jsonEsc + `0009reservations","pk":"1"}`,
		"C1 control pk":  `{"table":"app.reservations","pk":"` + jsonEsc + `0085"}`,
		"line end table": `{"table":"app.reservations` + jsonEsc + `000a","pk":"1"}`,
	} {
		if code, b := h.do("POST", path, agentToken, `{"records":[`+rec+`]}`); code != 400 || errCode(t, b) != "invalid_request" {
			t.Errorf("%s: %d %s", name, code, b)
		}
	}
	// Unquoted identifiers are lower case in PostgreSQL, which is what verify looks up.
	if code, b := h.do("POST", path, agentToken, `{"records":[{"table":"App.Reservations","pk":"Ab-42"},{"table":"Reviews","pk":"é"}]}`); code != 202 {
		t.Fatalf("append: %d %s", code, b)
	}
	recs, err := h.store.ListWrittenRecords(t.Context(), "moimroom-demo", 0, 0)
	if err != nil || len(recs) != 2 || recs[0].Table != "app.reservations" || recs[0].PK != "Ab-42" || recs[1].Table != "reviews" || recs[1].PK != "é" {
		t.Fatalf("stored = %+v, %v", recs, err)
	}
}

// Verification must never pass on a stream nobody wrote to.
func TestStartNeedsWrittenRecords(t *testing.T) {
	h := newHarness(t)
	ref := h.approved(moimroomPlan("p1", "a", "b", `"verify":{"writtenRecords":"stream:moimroom-demo"}`))
	code, b := h.do("POST", "/v1/migrations", opToken, apiv1.StartMigrationRequest{PlanRef: ref})
	if code != 400 || errCode(t, b) != "invalid_request" || !strings.Contains(string(b), "start the load generator before the migration") {
		t.Fatalf("start with an empty stream: %d %s", code, b)
	}
	_, b = h.do("GET", "/v1/plans/"+ref, viewerToken, nil)
	if p := decode[apiv1.PlanRecord](t, b); p.Status != apiv1.PlanStatusApproved {
		t.Fatalf("the plan was not released: %s", b)
	}
	if code, b := h.do("POST", "/v1/written-records/moimroom-demo", agentToken, `{"records":[{"table":"app.reservations","pk":"1"}]}`); code != 202 {
		t.Fatalf("append: %d %s", code, b)
	}
	h.waitRun(h.start(apiv1.StartMigrationRequest{PlanRef: ref}), status(apiv1.RunStatusAwaitingSwitch))
}
