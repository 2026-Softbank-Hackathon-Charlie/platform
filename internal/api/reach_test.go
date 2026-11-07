package api

import (
	"context"
	"strings"
	"testing"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/preflight/reach"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/runs"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

// fixedReach answers every check with one result; the real Checker's lookups
// and dials are replaced.
type fixedReach struct{ r reach.Result }

func (f fixedReach) Check(context.Context, *plan.Plan) (reach.Result, error) { return f.r, nil }

func withReach(f fixedReach) option {
	return func(_ *harness, _ *runs.Config, d *Deps) { d.Plans.SetReachGate(f) }
}

func TestApproveRefusedForUnreachableSource(t *testing.T) {
	t.Setenv("SRC_PGPASSWORD", "TOPSECRET")
	h := newHarness(t, withReach(fixedReach{reach.Result{
		Status: reach.Fail, Code: reach.CodeDNS,
		Message: "the source database name does not resolve from the platform API (dns): the run would stop at its precheck",
		Source:  reach.Side{Status: reach.Fail, Code: reach.CodeDNS},
		Target:  reach.Side{Status: reach.Unknown, Code: reach.CodeInconclusive},
	}}))
	body := planJSON("p1", "a", "b", false)
	rec := h.submit(body)
	rev := h.rehearsed(rec.Ref, body, "")

	code, b := h.do("POST", "/v1/plans/"+rec.Ref+"/approve", opToken, map[string]any{"revision": rev})
	if code != 409 || errCode(t, b) != "db_unreachable" || !strings.Contains(string(b), "does not resolve from the platform API") {
		t.Fatalf("approve: %d %s", code, b)
	}
	if strings.Contains(string(b), "TOPSECRET") || strings.Contains(string(b), "ts.net") {
		t.Fatalf("response leaks the DSN: %s", b)
	}
	// nothing was approved, and no run exists to fail
	_, b = h.do("GET", "/v1/plans/"+rec.Ref, viewerToken, nil)
	if p := decode[apiv1.PlanRecord](t, b); p.Status != apiv1.PlanStatusPendingApproval {
		t.Fatalf("plan status = %s", p.Status)
	}
	if d := h.auditDetails("/approve"); len(d) == 0 || !strings.Contains(d[0], "db_unreachable") ||
		!strings.Contains(d[0], `"code":"dns"`) || strings.Contains(d[0], "TOPSECRET") || strings.Contains(d[0], "ts.net") {
		t.Fatalf("audit: %v", d)
	}
}

func TestApproveWhenSourceIsReachable(t *testing.T) {
	t.Setenv("SRC_PGPASSWORD", "TOPSECRET")
	h := newHarness(t, withReach(fixedReach{reach.Result{Status: reach.Pass, Code: reach.CodeReachable}}))
	h.approved(planJSON("p1", "a", "b", false)) // fails the test on anything but 200
}

// What the check cannot establish is not a refusal.
func TestApproveWhenReachIsUnknown(t *testing.T) {
	t.Setenv("SRC_PGPASSWORD", "TOPSECRET")
	h := newHarness(t, withReach(fixedReach{reach.Result{Status: reach.Unknown, Code: reach.CodeInconclusive}}))
	h.approved(planJSON("p1", "a", "b", false))
}
