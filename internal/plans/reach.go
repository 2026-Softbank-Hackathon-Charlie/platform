package plans

import (
	"context"
	"errors"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/preflight/reach"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
)

// ReachGate checks that the platform API can reach the plan's source database
// (reach.Checker).
type ReachGate interface {
	Check(ctx context.Context, p *plan.Plan) (reach.Result, error)
}

// SetReachGate makes Approve refuse a plan whose source database name does not
// resolve, or whose port does not accept connections, from where the API runs:
// the engine would stop at its precheck right after the start (ADR 0035).
// Without a gate nothing is checked.
func (s *Service) SetReachGate(g ReachGate) { s.reach = g }

// ErrDBUnreachable is matched by every DBUnreachable.
var ErrDBUnreachable = errors.New("the source database is not reachable from the platform API")

// DBUnreachable is the error Approve returns when the source database cannot
// be reached from the API.
type DBUnreachable struct {
	Result reach.Result
}

func (e *DBUnreachable) Error() string {
	return ErrDBUnreachable.Error() + ": " + e.Result.Message
}

func (e *DBUnreachable) Is(target error) bool { return target == ErrDBUnreachable }

// requireReach runs the gate for an approval. Only a clear failure of the
// source blocks, like the other gates: whatever the check cannot establish is
// unknown and the plan is approved as before. A plan that does not parse here
// was parsed when it was submitted, and Reserve reports it again at the start,
// so a parse problem is not this gate's to raise. Approving an approved plan
// again is a no-op and is not checked again; a rollback plan is created
// approved (Rollback), so it never reaches this gate.
func (s *Service) requireReach(ctx context.Context, row store.Plan) error {
	if s.reach == nil || row.Status != store.PlanPending {
		return nil
	}
	p, _, err := s.parse(ctx, row.Raw)
	if err != nil {
		return nil
	}
	r, err := s.reach.Check(ctx, p)
	if err != nil { // the check itself did not finish: the plan is approved
		return nil
	}
	if r.Status == reach.Fail {
		return &DBUnreachable{Result: r}
	}
	return nil
}
