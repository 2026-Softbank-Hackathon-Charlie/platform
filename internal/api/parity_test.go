package api

import (
	"context"
	"strings"
	"testing"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/preflight/parity"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/runs"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

type recordedParity struct{}

func (recordedParity) Check(context.Context, *plan.Plan) (parity.Report, error) {
	return parity.Report{Passed: false, Results: []parity.Result{
		{Capability: parity.Identity, Status: parity.Fail, Code: "rbac_mismatch", Checks: []parity.Check{{ID: "no-secrets", Status: parity.Fail, Code: "authorization_mismatch"}}},
		{Capability: parity.Secret, Status: parity.Unknown, Code: "probe_not_configured", Checks: []parity.Check{}},
	}}, nil
}

// A failing parity report is written to the audit log and the run starts.
func TestStartRecordsParityWithoutBlocking(t *testing.T) {
	h := newHarness(t, func(_ *harness, _ *runs.Config, d *Deps) { d.Plans.SetParityGate(recordedParity{}) })
	h.start(apiv1.StartMigrationRequest{PlanRef: h.approved(planJSON("p1", "a", "b", false))})
	d := h.auditDetails("POST /v1/migrations")
	if len(d) == 0 || !strings.Contains(d[0], `"parity"`) || !strings.Contains(d[0], `"mode":"audit"`) || !strings.Contains(d[0], `"passed":false`) ||
		!strings.Contains(d[0], "identity fail rbac_mismatch: no-secrets=fail(authorization_mismatch)") || !strings.Contains(d[0], "secret unknown probe_not_configured") {
		t.Fatalf("audit: %v", d)
	}
	for _, field := range []string{`"binding"`, `"planDigest"`, `"revision"`, `"reviewedAt"`} {
		if !strings.Contains(d[0], field) {
			t.Fatalf("missing %s: %v", field, d)
		}
	}
}
