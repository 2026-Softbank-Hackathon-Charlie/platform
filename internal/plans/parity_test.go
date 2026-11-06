package plans

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
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

func TestParityBindingUsesUnexpandedApprovedBody(t *testing.T) {
	t.Setenv("SRC_PGPASSWORD", "parity-secret-source")
	t.Setenv("DST_PGPASSWORD", "parity-secret-target")
	s := newService(DefaultPolicy())
	s.SetParityGate(&fakeParity{report: failedParity()})
	ref := approved(t, s)
	row, err := s.store.GetPlan(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	res, err := s.Reserve(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(row.Raw)
	if b := res.Parity.Binding; b.PlanDigest != hex.EncodeToString(sum[:]) || b.Operation != "planned_migration" || len(b.PolicyDigest) != 64 {
		t.Fatalf("%+v", b)
	}
	raw, _ := json.Marshal(res.Parity)
	if strings.Contains(string(raw), "parity-secret") || strings.Contains(string(raw), "postgres://") {
		t.Fatal("secret-bearing plan leaked into parity evidence")
	}
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
			if b := res.Parity.Binding; b == nil || b.PlanRef == "" || b.Revision < 1 || len(b.PlanDigest) != 64 || b.ReviewedAt == nil {
				t.Fatalf("missing approval binding: %+v", b)
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
