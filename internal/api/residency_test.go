package api

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/preflight"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/preflight/gate"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/preflight/inventory"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/preflight/residency"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/runs"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

// gke is in Korea and eks in Japan; db-gke holds one kr-only table when
// customersLabel is kr-only.
const residencyPolicy = `
version: 1
environments: [{id: gke, country: KR}, {id: eks, country: JP}]
apacCountries: [KR, JP]
catalog:
  - scope: {environment: gke, namespace: shop, cluster: db-gke}
    datasets:
      - {dataset: {database: app, schema: public, table: products}, label: global}
      - {dataset: {database: app, schema: public, table: customers}, label: LABEL}
  - scope: {environment: eks, namespace: shop, cluster: db-eks}
    datasets:
      - {dataset: {database: app, schema: public, table: products}, label: global}
      - {dataset: {database: app, schema: public, table: customers}, label: LABEL}
`

// shopInventory stands in for the live collector: both clusters hold the
// same two tables.
type shopInventory struct{}

func (shopInventory) Collect(_ context.Context, _ string, scope preflight.ClusterScope) (preflight.Inventory, inventory.Result) {
	return preflight.Inventory{Scope: scope, Complete: true, Datasets: []preflight.Dataset{
		{Database: "app", Schema: "public", Table: "customers"},
		{Database: "app", Schema: "public", Table: "products"},
	}}, inventory.Result{}
}

func residencyGate(t *testing.T, label string) *gate.Gate {
	t.Helper()
	cfg, err := residency.ParseConfig([]byte(strings.ReplaceAll(residencyPolicy, "LABEL", label)))
	if err != nil {
		t.Fatal(err)
	}
	g, err := gate.New(context.Background(), cfg, shopInventory{})
	if err != nil {
		t.Fatal(err)
	}
	return g
}

// swappableGate lets a test change the policy after the server started.
type swappableGate struct{ cur atomic.Pointer[gate.Gate] }

func (s *swappableGate) Check(ctx context.Context, p *plan.Plan) (preflight.ResidencyVerdict, error) {
	return s.cur.Load().Check(ctx, p)
}

func withResidency(g *swappableGate) option {
	return func(_ *harness, _ *runs.Config, d *Deps) { d.Plans.SetResidencyGate(g) }
}

func (h *harness) auditDetails(action string) []string {
	h.t.Helper()
	audit, err := h.store.ListAudit(context.Background(), 0, 100)
	if err != nil {
		h.t.Fatal(err)
	}
	var out []string
	for _, a := range audit {
		if strings.Contains(a.Action, action) {
			out = append(out, string(a.Detail))
		}
	}
	return out
}

func TestStartDeniedByResidency(t *testing.T) {
	t.Setenv("SRC_PGPASSWORD", "TOPSECRET")
	g := &swappableGate{}
	g.cur.Store(residencyGate(t, "kr-only"))
	h := newHarness(t, withResidency(g))
	ref := h.approved(planJSON("p1", "a", "b", false))

	code, b := h.do("POST", "/v1/migrations", opToken, apiv1.StartMigrationRequest{PlanRef: ref})
	if code != 409 || errCode(t, b) != "residency_denied" || !strings.Contains(string(b), "country_not_allowed app.public.customers") {
		t.Fatalf("start: %d %s", code, b)
	}
	if strings.Contains(string(b), "TOPSECRET") || strings.Contains(string(b), "ts.net") {
		t.Fatalf("response leaks the DSN: %s", b)
	}
	_, b = h.do("GET", "/v1/plans/"+ref, viewerToken, nil)
	if p := decode[apiv1.PlanRecord](t, b); p.Status != apiv1.PlanStatusApproved {
		t.Fatalf("plan status = %s", p.Status)
	}
	h.idle()
	if list, _ := h.runner.List(context.Background()); len(list) != 0 {
		t.Fatalf("a denied plan started %d runs", len(list))
	}
	details := h.auditDetails("POST /v1/migrations")
	if len(details) == 0 || !strings.Contains(details[0], "residency_denied") || !strings.Contains(details[0], "country_not_allowed app.public.customers") ||
		!strings.Contains(details[0], g.cur.Load().PolicyDigest()) {
		t.Fatalf("audit: %v", details)
	}

	// The same plan starts once the policy allows it.
	g.cur.Store(residencyGate(t, "apac"))
	h.start(apiv1.StartMigrationRequest{PlanRef: ref})
	if d := h.auditDetails("POST /v1/migrations"); !strings.Contains(d[0], `"residency":"allowed"`) {
		t.Fatalf("audit after allow: %v", d)
	}
}

// A rollback returns the data to where it came from; a policy that now
// forbids the original direction must not trap the application.
func TestAutomaticRollbackIsExemptFromResidency(t *testing.T) {
	g := &swappableGate{}
	g.cur.Store(residencyGate(t, "global"))
	h := newHarness(t, withResidency(g))
	h.setTarget(apiv1.BackendStats{Requests: 400, ErrorRate: 0, P95Millis: 2500})
	id := h.start(observeReq(h.approved(planJSON("p1", "a", "b", true)), true, "1s"))
	g.cur.Store(nil) // from now on every check is denied (a nil gate is not ready)

	run := h.waitRun(id, func(r apiv1.Run) bool { return r.Observe != nil && r.Observe.RollbackRunId != nil })
	h.waitRun(*run.Observe.RollbackRunId, ended(apiv1.RunStatusSucceeded))
	found := false
	for _, d := range h.auditDetails("/rollback") {
		if strings.Contains(d, `"residency":"rollback_exempt"`) {
			found = true
		}
	}
	if !found {
		t.Fatalf("rollback audit: %v", h.auditDetails("/rollback"))
	}
}
