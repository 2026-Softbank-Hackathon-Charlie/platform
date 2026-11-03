package plans

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/preflight/capacity"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store/storetest"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

type fakeCapacity struct {
	report capacity.Report
	err    error
	seen   int
}

func (f *fakeCapacity) Check(context.Context, *plan.Plan) (capacity.Report, error) {
	f.seen++
	return f.report, f.err
}

func short() capacity.Report {
	return capacity.Evaluate(capacity.Measurements{SourceSessions: ptrInt(340), TargetMaxConnections: ptrInt(200), TargetMaxConnectionsFrom: "target"},
		capacity.Thresholds{MaxLagBytes: 16 << 20})
}

func ptrInt(n int) *int { return &n }

func TestReserveRefusesInsufficientCapacity(t *testing.T) {
	s := newService(DefaultPolicy())
	s.SetCapacityGate(&fakeCapacity{report: short()})
	ref := approved(t, s)
	_, err := s.Reserve(ctx, ref)
	var c *CapacityInsufficient
	if !errors.Is(err, ErrCapacityInsufficient) || !errors.As(err, &c) || c.Report.Allowed {
		t.Fatalf("reserve: %v", err)
	}
	if !strings.Contains(err.Error(), "connections: target max_connections 200 (from target), required 408") || strings.Contains(err.Error(), "src-secret") {
		t.Fatalf("message %q", err)
	}
	if got, _ := s.Get(ctx, ref); got.Status != apiv1.PlanStatusApproved {
		t.Fatalf("plan is %s after a refusal", got.Status)
	}
}

func TestReserveCapacityOutcomes(t *testing.T) {
	passed := capacity.Evaluate(capacity.Measurements{}, capacity.Thresholds{})
	for name, tc := range map[string]struct {
		gate *fakeCapacity
		want string
	}{
		"passes":                            {&fakeCapacity{report: passed}, CapacityPassed},
		"a failed measurement never blocks": {&fakeCapacity{report: short(), err: errors.New("boom")}, CapacityError},
	} {
		t.Run(name, func(t *testing.T) {
			s := newService(DefaultPolicy())
			s.SetCapacityGate(tc.gate)
			res, err := s.Reserve(ctx, approved(t, s))
			if err != nil || res.Capacity.Outcome != tc.want || tc.gate.seen != 1 {
				t.Fatalf("%+v %v", res.Capacity, err)
			}
		})
	}
	s := newService(DefaultPolicy())
	if res, err := s.Reserve(ctx, approved(t, s)); err != nil || res.Capacity.Outcome != CapacityNotConfigured {
		t.Fatalf("without a gate: %+v %v", res.Capacity, err)
	}
}

func TestRollbackIsExemptFromCapacity(t *testing.T) {
	t.Setenv("DST_PGPASSWORD", "dst-secret")
	s := New(DefaultPolicy(), storetest.NewMemory())
	g := &fakeCapacity{report: short()}
	s.SetCapacityGate(g)
	orig := approved(t, s)
	rec, err := s.Rollback(ctx, "r1", orig, "carol", "slo violated")
	if err != nil {
		t.Fatal(err)
	}
	res, err := s.Reserve(ctx, rec.Ref)
	if err != nil || res.Capacity.Outcome != CapacityRollbackExempt || g.seen != 0 {
		t.Fatalf("%+v %v (measured %d times)", res.Capacity, err, g.seen)
	}
}

// Residency runs first: a denied plan is never measured.
func TestResidencyDenialSkipsCapacity(t *testing.T) {
	s := newService(DefaultPolicy())
	g := &fakeCapacity{report: short()}
	s.SetCapacityGate(g)
	s.SetResidencyGate(&fakeGate{err: errors.New("deny")})
	if _, err := s.Reserve(ctx, approved(t, s)); !errors.Is(err, ErrResidencyDenied) || g.seen != 0 {
		t.Fatalf("%v (measured %d times)", err, g.seen)
	}
}
