package plans

import (
	"errors"
	"testing"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/preflight/parity"
)

type narrowObservationGate struct {
	fakeParity
	enforced, allowed bool
}

func (g *narrowObservationGate) ObservabilityDecision(*plan.Plan, *parity.Report) (bool, bool) {
	return g.enforced, g.allowed
}
func (g *narrowObservationGate) ObservabilityConfigured(*plan.Plan) bool { return true }

func TestExplicitSignalGateBlocksAndReleasesReservationButAuditStillStarts(t *testing.T) {
	s := newService(DefaultPolicy())
	g := &narrowObservationGate{fakeParity: fakeParity{report: failedParity()}, enforced: true}
	s.SetParityGate(g)
	ref := approved(t, s)
	if _, err := s.Reserve(ctx, ref); !errors.Is(err, ErrObservabilityUnverified) {
		t.Fatalf("missing receiving evidence did not block: %v", err)
	}
	// A blocked check must not strand the plan in starting. Current signal
	// evidence can later pass without a new plan or an override parameter.
	g.allowed = true
	res, err := s.Reserve(ctx, ref)
	if err != nil || !res.Parity.ObservabilityEnforced {
		t.Fatalf("reservation was not released: %v", err)
	}
	s = newService(DefaultPolicy())
	g = &narrowObservationGate{fakeParity: fakeParity{report: failedParity()}, enforced: false}
	s.SetParityGate(g)
	if _, err := s.Reserve(ctx, approved(t, s)); err != nil {
		t.Fatal("audit mode unexpectedly blocked")
	}
}
