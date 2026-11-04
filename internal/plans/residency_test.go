package plans

import (
	"reflect"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/preflight"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store/storetest"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

// fakeGate records the plans it saw and answers with a fixed verdict.
type fakeGate struct {
	verdict preflight.ResidencyVerdict
	err     error
	seen    []*plan.Plan
}

func (g *fakeGate) Check(_ context.Context, p *plan.Plan) (preflight.ResidencyVerdict, error) {
	g.seen = append(g.seen, p)
	return g.verdict, g.err
}

func approved(t *testing.T, s *Service) string {
	t.Helper()
	t.Setenv("SRC_PGPASSWORD", "src-secret")
	rec, err := s.Submit(ctx, planJSON(""), "alice")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Approve(ctx, rec.Ref, "bob", "", nil); err != nil {
		t.Fatal(err)
	}
	return rec.Ref
}

func TestReserveWithoutGateIsUnchanged(t *testing.T) {
	s := newService(DefaultPolicy())
	res, err := s.Reserve(ctx, approved(t, s))
	if err != nil || res.Residency.Outcome != ResidencyNotConfigured {
		t.Fatalf("%+v %v", res.Residency, err)
	}
}

func TestReserveAllowedByGate(t *testing.T) {
	s := newService(DefaultPolicy())
	g := &fakeGate{verdict: preflight.ResidencyVerdict{Allowed: true, PolicyDigest: "d1", Reasons: []preflight.Reason{}}}
	s.SetResidencyGate(g)
	res, err := s.Reserve(ctx, approved(t, s))
	if err != nil || !reflect.DeepEqual(res.Residency, ResidencyCheck{Outcome: ResidencyAllowed, PolicyDigest: "d1"}) {
		t.Fatalf("%+v %v", res.Residency, err)
	}
	// The gate sees the expanded plan: the collector needs the real DSN.
	if len(g.seen) != 1 || !strings.Contains(g.seen[0].Source.DSN, "src-secret") {
		t.Fatalf("gate saw %d plans", len(g.seen))
	}
}

func TestReserveDeniedReturnsPlanToApproved(t *testing.T) {
	for name, g := range map[string]*fakeGate{
		"policy denies": {verdict: preflight.ResidencyVerdict{PolicyDigest: "d1", Reasons: []preflight.Reason{
			{Code: "country_not_allowed", MovementID: "shop-gke-to-eks", Dataset: &preflight.Dataset{Database: "app", Schema: "public", Table: "customers"}},
		}}},
		"gate error":            {err: errors.New("boom"), verdict: preflight.ResidencyVerdict{Reasons: []preflight.Reason{{Code: "evaluation_failed"}}}},
		"error without reasons": {err: errors.New("boom")},
		// A gate must never be trusted on an error, whatever Allowed says.
		"allowed with an error": {err: errors.New("boom"), verdict: preflight.ResidencyVerdict{Allowed: true}},
	} {
		t.Run(name, func(t *testing.T) {
			s := newService(DefaultPolicy())
			s.SetResidencyGate(g)
			ref := approved(t, s)
			_, err := s.Reserve(ctx, ref)
			var denied *ResidencyDenied
			if !errors.Is(err, ErrResidencyDenied) || !errors.As(err, &denied) || denied.Verdict.Allowed || len(denied.Verdict.Reasons) == 0 {
				t.Fatalf("reserve: %v", err)
			}
			if strings.Contains(err.Error(), "src-secret") || strings.Contains(err.Error(), "boom") {
				t.Fatalf("error leaks: %v", err)
			}
			if got, _ := s.Get(ctx, ref); got.Status != apiv1.PlanStatusApproved {
				t.Fatalf("plan is %s after a denial", got.Status)
			}
		})
	}
}

func TestDeniedReasonText(t *testing.T) {
	err := &ResidencyDenied{Verdict: preflight.ResidencyVerdict{Reasons: []preflight.Reason{
		{Code: "country_not_allowed", Dataset: &preflight.Dataset{Database: "app", Schema: "public", Table: "customers"}},
		{Code: "inventory_incomplete"},
	}}}
	if got := err.Error(); !strings.HasSuffix(got, "country_not_allowed app.public.customers; inventory_incomplete") {
		t.Fatalf("message %q", got)
	}
}

func TestRollbackIsExemptFromGate(t *testing.T) {
	t.Setenv("DST_PGPASSWORD", "dst-secret")
	st := storetest.NewMemory()
	s := New(DefaultPolicy(), st)
	g := &fakeGate{verdict: preflight.ResidencyVerdict{Reasons: []preflight.Reason{{Code: "country_not_allowed"}}}}
	s.SetResidencyGate(g)
	orig := approved(t, s)
	rec, err := s.Rollback(ctx, "r1", orig, "carol", "slo violated")
	if err != nil {
		t.Fatal(err)
	}
	res, err := s.Reserve(ctx, rec.Ref)
	if err != nil || res.Residency.Outcome != ResidencyRollbackExempt || res.RollbackOf != "r1" || len(g.seen) != 0 {
		t.Fatalf("%+v %v (gate saw %d)", res.Residency, err, len(g.seen))
	}
	// The original direction is still checked.
	if _, err := s.Reserve(ctx, orig); !errors.Is(err, ErrResidencyDenied) {
		t.Fatalf("original plan: %v", err)
	}
}

// Only the server sets RollbackOf: a submitted plan cannot claim to be a
// rollback to skip the gate.
func TestSubmittedPlanCannotClaimRollback(t *testing.T) {
	st := storetest.NewMemory()
	s := New(DefaultPolicy(), st)
	s.SetResidencyGate(&fakeGate{verdict: preflight.ResidencyVerdict{Reasons: []preflight.Reason{{Code: "country_not_allowed"}}}})
	t.Setenv("SRC_PGPASSWORD", "x")
	if _, err := s.Submit(ctx, planJSON(`"rollbackOf": "r1"`), "mallory"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("submit with rollbackOf: %v", err)
	}
	ref := approved(t, s)
	if row, _ := st.GetPlan(ctx, ref); row.RollbackOf != "" || row.Status != store.PlanApproved {
		t.Fatalf("row %+v", row)
	}
	if _, err := s.Reserve(ctx, ref); !errors.Is(err, ErrResidencyDenied) {
		t.Fatalf("reserve: %v", err)
	}
}
