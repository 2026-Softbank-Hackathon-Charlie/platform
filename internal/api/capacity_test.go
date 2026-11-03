package api

import (
	"context"
	"strings"
	"testing"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/preflight/capacity"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/runs"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

// measuredCapacity judges fixed measurements with the real Evaluate; only the
// database reads are replaced.
type measuredCapacity struct{ m capacity.Measurements }

func (c measuredCapacity) Check(_ context.Context, p *plan.Plan) (capacity.Report, error) {
	return capacity.Evaluate(c.m, capacity.Thresholds{MaxLagBytes: p.Thresholds.MaxLagBytes, CatchupTimeout: p.Thresholds.CatchupTimeout.Duration}), nil
}

func withCapacity(m capacity.Measurements) option {
	return func(_ *harness, _ *runs.Config, d *Deps) { d.Plans.SetCapacityGate(measuredCapacity{m}) }
}

func n(v int) *int { return &v }

func TestStartRefusedForInsufficientCapacity(t *testing.T) {
	t.Setenv("SRC_PGPASSWORD", "TOPSECRET")
	h := newHarness(t, withCapacity(capacity.Measurements{SourceSessions: n(340), TargetMaxConnections: n(200), TargetMaxConnectionsFrom: "target"}))
	ref := h.approved(planJSON("p1", "a", "b", false))

	code, b := h.do("POST", "/v1/migrations", opToken, apiv1.StartMigrationRequest{PlanRef: ref})
	if code != 409 || errCode(t, b) != "capacity_insufficient" || !strings.Contains(string(b), "required 408") {
		t.Fatalf("start: %d %s", code, b)
	}
	if strings.Contains(string(b), "TOPSECRET") || strings.Contains(string(b), "ts.net") {
		t.Fatalf("response leaks the DSN: %s", b)
	}
	_, b = h.do("GET", "/v1/plans/"+ref, viewerToken, nil)
	if p := decode[apiv1.PlanRecord](t, b); p.Status != apiv1.PlanStatusApproved {
		t.Fatalf("plan status = %s", p.Status)
	}
	if d := h.auditDetails("POST /v1/migrations"); len(d) == 0 || !strings.Contains(d[0], "capacity_insufficient") ||
		!strings.Contains(d[0], "target_connections_insufficient") || !strings.Contains(d[0], `"targetMaxConnections":200`) {
		t.Fatalf("audit: %v", d)
	}
}

func TestStartRecordsPassingCapacity(t *testing.T) {
	h := newHarness(t, withCapacity(capacity.Measurements{SourceSessions: n(10), TargetMaxConnections: n(100), TargetMaxConnectionsFrom: "target"}))
	h.start(apiv1.StartMigrationRequest{PlanRef: h.approved(planJSON("p1", "a", "b", false))})
	if d := h.auditDetails("POST /v1/migrations"); len(d) == 0 || !strings.Contains(d[0], `"allowed":true`) || !strings.Contains(d[0], "connections pass ok") {
		t.Fatalf("audit: %v", d)
	}
}
