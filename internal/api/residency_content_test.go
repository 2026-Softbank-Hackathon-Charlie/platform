package api

import (
	"context"
	"strings"
	"testing"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/preflight"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/runs"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

// This test isolates audit plumbing, not scanner correctness or a live run.
type contentAuditGate struct{ allowed bool }

func (g contentAuditGate) Check(context.Context, *plan.Plan) (preflight.ResidencyVerdict, error) {
	v := preflight.ResidencyVerdict{Allowed: g.allowed, PolicyDigest: strings.Repeat("d", 64), Content: &preflight.FileInspection{Rules: "content-v1", Objects: 2, Bytes: 32, ManifestDigest: strings.Repeat("e", 64), Labels: []string{"global", "kr-only"}, Findings: map[string]int{"email": 1}}}
	if !g.allowed {
		v.Reasons = []preflight.Reason{{Code: "bucket_country_not_allowed"}}
	}
	return v, nil
}

func TestContentEvidenceAuditedOnAllowAndDeny(t *testing.T) {
	for _, allowed := range []bool{false, true} {
		t.Run(map[bool]string{true: "allow", false: "deny"}[allowed], func(t *testing.T) {
			h := newHarness(t, func(_ *harness, _ *runs.Config, d *Deps) {
				d.Plans.SetResidencyGate(contentAuditGate{allowed: allowed})
			})
			ref := h.approved(planJSON("content-audit", "a", "b", false))
			if allowed {
				h.start(apiv1.StartMigrationRequest{PlanRef: ref})
			} else {
				code, b := h.do("POST", "/v1/migrations", opToken, apiv1.StartMigrationRequest{PlanRef: ref})
				if code != 409 || errCode(t, b) != "residency_denied" {
					t.Fatalf("%d %s", code, b)
				}
			}
			details := h.auditDetails("POST /v1/migrations")
			if len(details) == 0 || !strings.Contains(details[0], `"residencyContent"`) || !strings.Contains(details[0], strings.Repeat("e", 64)) || !strings.Contains(details[0], `"objects":2`) {
				t.Fatalf("missing content evidence: %v", details)
			}
		})
	}
}
