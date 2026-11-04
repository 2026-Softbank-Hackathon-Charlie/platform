package plans

import (
	"context"
	"errors"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/preflight/pgversion"
)

// VersionGate compares the source's and the target's PostgreSQL major
// versions (pgversion.Checker).
type VersionGate interface {
	Check(ctx context.Context, p *plan.Plan) (pgversion.Result, error)
}

// SetVersionGate makes Reserve refuse plans whose source and target clearly
// run different PostgreSQL major versions. Without a gate nothing is checked.
func (s *Service) SetVersionGate(g VersionGate) { s.version = g }

// Version outcomes recorded on a reservation, besides the pgversion statuses.
const (
	VersionNotConfigured  = "not_configured"
	VersionRollbackExempt = "rollback_exempt"
	VersionError          = "error" // the check itself did not finish; the run starts
)

// VersionCheck is what the gate decided for a reservation, for the audit log.
type VersionCheck struct {
	Outcome string // pgversion.Pass/Unknown or a Version* constant
	Result  *pgversion.Result
}

// ErrVersionMismatch is matched by every VersionMismatch.
var ErrVersionMismatch = errors.New("source and target PostgreSQL major versions differ")

// VersionMismatch is the error Reserve returns when the major versions differ.
type VersionMismatch struct {
	Result pgversion.Result
}

func (e *VersionMismatch) Error() string {
	return ErrVersionMismatch.Error() + ": " + e.Result.Message
}

func (e *VersionMismatch) Is(target error) bool { return target == ErrVersionMismatch }

// checkVersion runs the gate for a reservation. Only a clear mismatch blocks:
// an unknown version is recorded as unknown, never as a pass, and the run
// starts as before. A rollback is exempt like the other gates; its two
// clusters already replicate, so they share a major version.
func (s *Service) checkVersion(ctx context.Context, rollbackOf string, p *plan.Plan) (VersionCheck, error) {
	switch {
	case s.version == nil:
		return VersionCheck{Outcome: VersionNotConfigured}, nil
	case rollbackOf != "":
		return VersionCheck{Outcome: VersionRollbackExempt}, nil
	}
	r, err := s.version.Check(ctx, p)
	if err != nil {
		return VersionCheck{Outcome: VersionError}, nil
	}
	if r.Status == pgversion.Fail {
		return VersionCheck{Outcome: string(r.Status), Result: &r}, &VersionMismatch{Result: r}
	}
	return VersionCheck{Outcome: string(r.Status), Result: &r}, nil
}
