package plans

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/preflight/pgversion"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store/storetest"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

type fakeVersion struct {
	result pgversion.Result
	err    error
	seen   int
}

func (f *fakeVersion) Check(context.Context, *plan.Plan) (pgversion.Result, error) {
	f.seen++
	return f.result, f.err
}

func pgv(major int, full string) *pgversion.Version {
	return &pgversion.Version{Major: major, Full: full, From: "server"}
}

func mismatch() pgversion.Result { return pgversion.Compare(pgv(17, "17.11"), pgv(18, "18.6"), "") }

func TestReserveRefusesVersionMismatch(t *testing.T) {
	s := newService(DefaultPolicy())
	capGate := &fakeCapacity{report: short()}
	s.SetCapacityGate(capGate)
	s.SetVersionGate(&fakeVersion{result: mismatch()})
	ref := approved(t, s)
	_, err := s.Reserve(ctx, ref)
	var mm *VersionMismatch
	if !errors.Is(err, ErrVersionMismatch) || !errors.As(err, &mm) || mm.Result.Code != pgversion.CodeMismatch {
		t.Fatalf("reserve: %v", err)
	}
	if !strings.Contains(err.Error(), "source PostgreSQL 17 (17.11, from server) and target 18 (18.6, from server)") || strings.Contains(err.Error(), "src-secret") {
		t.Fatalf("message %q", err)
	}
	if got, _ := s.Get(ctx, ref); got.Status != apiv1.PlanStatusApproved {
		t.Fatalf("plan is %s after a refusal", got.Status)
	}
	if capGate.seen != 0 {
		t.Fatal("a mismatched plan was measured for capacity")
	}
}

func TestReserveVersionOutcomes(t *testing.T) {
	for name, tc := range map[string]struct {
		gate *fakeVersion
		want string
	}{
		"same major":                  {&fakeVersion{result: pgversion.Compare(pgv(17, "17.11"), pgv(17, "17.6"), "")}, "pass"},
		"unknown target never blocks": {&fakeVersion{result: pgversion.Compare(pgv(17, "17.11"), nil, pgversion.CodeImageDefault)}, "unknown"},
		"a failed check never blocks": {&fakeVersion{result: mismatch(), err: context.DeadlineExceeded}, VersionError},
	} {
		t.Run(name, func(t *testing.T) {
			s := newService(DefaultPolicy())
			s.SetVersionGate(tc.gate)
			res, err := s.Reserve(ctx, approved(t, s))
			if err != nil || res.Version.Outcome != tc.want || tc.gate.seen != 1 {
				t.Fatalf("%+v %v", res.Version, err)
			}
		})
	}
	s := newService(DefaultPolicy())
	if res, err := s.Reserve(ctx, approved(t, s)); err != nil || res.Version.Outcome != VersionNotConfigured {
		t.Fatalf("without a gate: %+v %v", res.Version, err)
	}
}

func TestRollbackIsExemptFromVersion(t *testing.T) {
	t.Setenv("DST_PGPASSWORD", "dst-secret")
	s := New(DefaultPolicy(), storetest.NewMemory())
	g := &fakeVersion{result: mismatch()}
	s.SetVersionGate(g)
	orig := approved(t, s)
	rec, err := s.Rollback(ctx, "r1", orig, "carol", "slo violated")
	if err != nil {
		t.Fatal(err)
	}
	res, err := s.Reserve(ctx, rec.Ref)
	if err != nil || res.Version.Outcome != VersionRollbackExempt || g.seen != 0 {
		t.Fatalf("%+v %v (checked %d times)", res.Version, err, g.seen)
	}
}
