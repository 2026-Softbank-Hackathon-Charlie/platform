package api

import (
	"context"
	"strings"
	"testing"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/preflight/pgversion"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/runs"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

// fixedVersions compares fixed versions with the real Compare; only the
// database and manifest reads are replaced.
type fixedVersions struct {
	src, dst *pgversion.Version
	code     string
}

func (f fixedVersions) Check(context.Context, *plan.Plan) (pgversion.Result, error) {
	return pgversion.Compare(f.src, f.dst, f.code), nil
}

func withVersions(f fixedVersions) option {
	return func(_ *harness, _ *runs.Config, d *Deps) { d.Plans.SetVersionGate(f) }
}

func TestStartRefusedForVersionMismatch(t *testing.T) {
	t.Setenv("SRC_PGPASSWORD", "TOPSECRET")
	h := newHarness(t, withVersions(fixedVersions{
		src: &pgversion.Version{Major: 17, Full: "17.11", From: "server"},
		dst: &pgversion.Version{Major: 18, Full: "18.6", From: "manifest image ghcr.io/cloudnative-pg/postgresql:18.6"},
	}))
	ref := h.approved(planJSON("p1", "a", "b", false))

	code, b := h.do("POST", "/v1/migrations", opToken, apiv1.StartMigrationRequest{PlanRef: ref})
	if code != 409 || errCode(t, b) != "version_mismatch" || !strings.Contains(string(b), "source PostgreSQL 17 (17.11, from server) and target 18") {
		t.Fatalf("start: %d %s", code, b)
	}
	if strings.Contains(string(b), "TOPSECRET") || strings.Contains(string(b), "ts.net") {
		t.Fatalf("response leaks the DSN: %s", b)
	}
	_, b = h.do("GET", "/v1/plans/"+ref, viewerToken, nil)
	if p := decode[apiv1.PlanRecord](t, b); p.Status != apiv1.PlanStatusApproved {
		t.Fatalf("plan status = %s", p.Status)
	}
	if d := h.auditDetails("POST /v1/migrations"); len(d) == 0 || !strings.Contains(d[0], "version_mismatch") ||
		!strings.Contains(d[0], `"code":"major_mismatch"`) || !strings.Contains(d[0], `"major":18`) {
		t.Fatalf("audit: %v", d)
	}
}

func TestStartRecordsUnknownVersion(t *testing.T) {
	h := newHarness(t, withVersions(fixedVersions{src: &pgversion.Version{Major: 17, Full: "17.11", From: "server"}, code: pgversion.CodeImageDefault}))
	h.start(apiv1.StartMigrationRequest{PlanRef: h.approved(planJSON("p1", "a", "b", false))})
	if d := h.auditDetails("POST /v1/migrations"); len(d) == 0 || !strings.Contains(d[0], `"status":"unknown"`) || !strings.Contains(d[0], `"code":"image_default"`) {
		t.Fatalf("audit: %v", d)
	}
}
