package plans

import (
	"context"
	"errors"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/preflight/parity"
)

// ParityGate checks the target's identity and monitoring against what the
// app needs (paritygate.Gate). Its report is recorded, never enforced.
type ParityGate interface {
	Check(ctx context.Context, p *plan.Plan) (parity.Report, error)
}

// SetParityGate makes Reserve record a parity report for every new run.
// The report never stops a run (audit mode). Without a gate nothing runs.
func (s *Service) SetParityGate(g ParityGate) { s.parity = g }

// Parity outcomes recorded on a reservation.
const (
	ParityNotConfigured  = "not_configured"
	ParityRollbackExempt = "rollback_exempt"
	// ParityRecorded: the probes ran; Report says what they found.
	ParityRecorded = "recorded"
	// ParityNoApp: the parity file has no app for the target namespace.
	ParityNoApp = "no_app"
	// ParityError: the check itself could not run (invalid request, cancelled).
	ParityError = "error"
)

// ParityCheck is what was recorded for a reservation, for the audit log.
type ParityCheck struct {
	Outcome string
	Report  *parity.Report
}

// checkParity records parity for a reservation. It never returns an error:
// in audit mode nothing it finds may stop or delay a run beyond its timeout.
// A rollback is exempt, as for the other gates.
func (s *Service) checkParity(ctx context.Context, rollbackOf string, p *plan.Plan) ParityCheck {
	switch {
	case s.parity == nil:
		return ParityCheck{Outcome: ParityNotConfigured}
	case rollbackOf != "":
		return ParityCheck{Outcome: ParityRollbackExempt}
	}
	r, err := s.parity.Check(ctx, p)
	switch {
	case errors.Is(err, parity.ErrNotConfigured):
		return ParityCheck{Outcome: ParityNoApp}
	case err != nil:
		return ParityCheck{Outcome: ParityError}
	}
	return ParityCheck{Outcome: ParityRecorded, Report: &r}
}
