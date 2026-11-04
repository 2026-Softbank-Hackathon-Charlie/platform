package plans

import (
	"context"
	"errors"
	"testing"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/preflight/parity"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store/storetest"
)

type fakeParity struct {
	report parity.Report
	err    error
	seen   int
}

func (f *fakeParity) Check(context.Context, *plan.Plan) (parity.Report, error) {
	f.seen++
	return f.report, f.err
}

func failedParity() parity.Report {
	return parity.Report{Passed: false, Results: []parity.Result{{Capability: parity.Identity, Status: parity.Fail, Code: "rbac_mismatch"}}}
}

// Audit mode: whatever parity finds, the run still starts.
func TestReserveRecordsParityWithoutBlocking(t *testing.T) {
	for name, tc := range map[string]struct {
		gate *fakeParity
		want string
	}{
		"failed report is recorded": {&fakeParity{report: failedParity()}, ParityRecorded},
		"check error never blocks":  {&fakeParity{err: errors.New("boom")}, ParityError},
		"no app for the namespace":  {&fakeParity{err: parity.ErrNotConfigured}, ParityNoApp},
	} {
		t.Run(name, func(t *testing.T) {
			s := newService(DefaultPolicy())
			s.SetParityGate(tc.gate)
			res, err := s.Reserve(ctx, approved(t, s))
			if err != nil || res.Parity.Outcome != tc.want || tc.gate.seen != 1 {
				t.Fatalf("%+v %v", res.Parity, err)
			}
			if tc.want == ParityRecorded && (res.Parity.Report == nil || res.Parity.Report.Passed) {
				t.Fatalf("report %+v", res.Parity.Report)
			}
		})
	}
	s := newService(DefaultPolicy())
	if res, err := s.Reserve(ctx, approved(t, s)); err != nil || res.Parity.Outcome != ParityNotConfigured {
		t.Fatalf("without a gate: %+v %v", res.Parity, err)
	}
}

func TestParityRunsOnlyForRunsThatStart(t *testing.T) {
	s := newService(DefaultPolicy())
	g := &fakeParity{report: failedParity()}
	s.SetParityGate(g)
	s.SetCapacityGate(&fakeCapacity{report: short()})
	if _, err := s.Reserve(ctx, approved(t, s)); !errors.Is(err, ErrCapacityInsufficient) || g.seen != 0 {
		t.Fatalf("%v (parity ran %d times)", err, g.seen)
	}

	t.Setenv("DST_PGPASSWORD", "dst-secret")
	s = New(DefaultPolicy(), storetest.NewMemory())
	g = &fakeParity{report: failedParity()}
	s.SetParityGate(g)
	rec, err := s.Rollback(ctx, "r1", approved(t, s), "carol", "slo violated")
	if err != nil {
		t.Fatal(err)
	}
	if res, err := s.Reserve(ctx, rec.Ref); err != nil || res.Parity.Outcome != ParityRollbackExempt || g.seen != 0 {
		t.Fatalf("rollback: %+v %v (parity ran %d times)", res.Parity, err, g.seen)
	}
}
