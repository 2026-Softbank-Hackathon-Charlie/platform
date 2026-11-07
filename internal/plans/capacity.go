package plans

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/preflight/capacity"
)

// CapacityGate measures whether a plan would fail for capacity reasons
// (capacity.Gate).
type CapacityGate interface {
	Check(ctx context.Context, p *plan.Plan) (capacity.Report, error)
}

// SetCapacityGate makes Reserve refuse plans whose measured capacity clearly
// fails. Without a gate nothing is measured, as before.
func (s *Service) SetCapacityGate(g CapacityGate) { s.capacity = g }

// Capacity outcomes recorded on a reservation.
const (
	CapacityNotConfigured  = "not_configured"
	CapacityPassed         = "passed"
	CapacityRollbackExempt = "rollback_exempt"
	// CapacityError: the measurement itself failed; the run starts and the
	// engine's own lag gate still applies.
	CapacityError = "error"
	// CapacityNoDatabase: the plan moves an app without a database (ADR
	// 0049); there is nothing to measure ("DB 없음").
	CapacityNoDatabase = "no_database"
)

// CapacityCheck is what the gate decided for a reservation, for the audit log.
type CapacityCheck struct {
	Outcome string
	Report  *capacity.Report
}

// ErrCapacityInsufficient is matched by every CapacityInsufficient.
var ErrCapacityInsufficient = errors.New("measured capacity is insufficient for this migration")

// CapacityInsufficient is the error Reserve returns when a check failed.
type CapacityInsufficient struct {
	Report capacity.Report
}

func (e *CapacityInsufficient) Error() string {
	var parts []string
	for _, c := range e.Report.Failed() {
		parts = append(parts, c.ID+": "+c.Message)
	}
	return fmt.Sprintf("%v: %s", ErrCapacityInsufficient, strings.Join(parts, "; "))
}

func (e *CapacityInsufficient) Is(target error) bool { return target == ErrCapacityInsufficient }

// checkCapacity runs the gate for a reservation. A rollback is exempt for the
// same reason as residency: blocking it could keep an application in a
// failing environment. A measurement that could not run does not block.
func (s *Service) checkCapacity(ctx context.Context, rollbackOf string, p *plan.Plan) (CapacityCheck, error) {
	switch {
	case s.capacity == nil:
		return CapacityCheck{Outcome: CapacityNotConfigured}, nil
	case NoDatabase(p):
		return CapacityCheck{Outcome: CapacityNoDatabase}, nil
	case rollbackOf != "":
		return CapacityCheck{Outcome: CapacityRollbackExempt}, nil
	}
	r, err := s.capacity.Check(ctx, p)
	if err != nil {
		return CapacityCheck{Outcome: CapacityError}, nil
	}
	if !r.Allowed {
		return CapacityCheck{Report: &r}, &CapacityInsufficient{Report: r}
	}
	return CapacityCheck{Outcome: CapacityPassed, Report: &r}, nil
}
