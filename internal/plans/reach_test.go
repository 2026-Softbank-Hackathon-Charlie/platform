package plans

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/preflight/reach"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

type fakeReach struct {
	result reach.Result
	err    error
	seen   []*plan.Plan
}

func (f *fakeReach) Check(_ context.Context, p *plan.Plan) (reach.Result, error) {
	f.seen = append(f.seen, p)
	return f.result, f.err
}

func unreachableDNS() reach.Result {
	return reach.Result{
		Status: reach.Fail, Code: reach.CodeDNS,
		Message: "the source database name does not resolve from the platform API (dns)",
		Source:  reach.Side{Status: reach.Fail, Code: reach.CodeDNS},
		Target:  reach.Side{Status: reach.Unknown, Code: reach.CodeInconclusive},
	}
}

func submitPending(t *testing.T, s *Service) string {
	t.Helper()
	t.Setenv("SRC_PGPASSWORD", "src-secret")
	rec, err := s.Submit(ctx, planJSON(""), "alice")
	if err != nil {
		t.Fatal(err)
	}
	return rec.Ref
}

// The case that made the gate: a plan whose source name is unknown where the
// API runs is refused at approval, before a run exists.
func TestApproveRefusesAnUnreachableSource(t *testing.T) {
	s := newService(DefaultPolicy())
	g := &fakeReach{result: unreachableDNS()}
	s.SetReachGate(g)
	ref := submitPending(t, s)

	_, err := s.Approve(ctx, ref, "bob", "", nil)
	var ur *DBUnreachable
	if !errors.Is(err, ErrDBUnreachable) || !errors.As(err, &ur) || ur.Result.Code != reach.CodeDNS {
		t.Fatalf("approve: %v", err)
	}
	if !strings.Contains(err.Error(), "does not resolve") || strings.Contains(err.Error(), "src-secret") {
		t.Fatalf("message %q", err)
	}
	got, _ := s.Get(ctx, ref)
	if got.Status != apiv1.PlanStatusPendingApproval {
		t.Fatalf("plan is %s after a refusal", got.Status)
	}
	if len(g.seen) != 1 || g.seen[0].Source.DSN == "" {
		t.Fatalf("gate saw %d plans", len(g.seen))
	}
}

// Once the route exists the same plan is approved: nothing was recorded
// against it by the refusal.
func TestApproveAfterTheRouteExists(t *testing.T) {
	s := newService(DefaultPolicy())
	g := &fakeReach{result: unreachableDNS()}
	s.SetReachGate(g)
	ref := submitPending(t, s)
	if _, err := s.Approve(ctx, ref, "bob", "", nil); !errors.Is(err, ErrDBUnreachable) {
		t.Fatalf("first approve: %v", err)
	}
	g.result = reach.Result{Status: reach.Pass, Code: reach.CodeReachable}
	rec, err := s.Approve(ctx, ref, "bob", "", nil)
	if err != nil || rec.Status != apiv1.PlanStatusApproved {
		t.Fatalf("second approve: %+v %v", rec, err)
	}
}

// Like the other gates, only a clear failure blocks.
func TestApproveOutcomes(t *testing.T) {
	for name, g := range map[string]*fakeReach{
		"reachable":                   {result: reach.Result{Status: reach.Pass, Code: reach.CodeReachable}},
		"unknown never blocks":        {result: reach.Result{Status: reach.Unknown, Code: reach.CodeInconclusive}},
		"a failed check never blocks": {result: unreachableDNS(), err: context.DeadlineExceeded},
	} {
		t.Run(name, func(t *testing.T) {
			s := newService(DefaultPolicy())
			s.SetReachGate(g)
			ref := submitPending(t, s)
			rec, err := s.Approve(ctx, ref, "bob", "", nil)
			if err != nil || rec.Status != apiv1.PlanStatusApproved || len(g.seen) != 1 {
				t.Fatalf("%+v %v seen=%d", rec, err, len(g.seen))
			}
		})
	}
	s := newService(DefaultPolicy())
	ref := submitPending(t, s)
	if rec, err := s.Approve(ctx, ref, "bob", "", nil); err != nil || rec.Status != apiv1.PlanStatusApproved {
		t.Fatalf("without a gate: %+v %v", rec, err)
	}
}

// Approving twice is a no-op and does not look at the network again.
func TestApprovingAgainIsNotChecked(t *testing.T) {
	s := newService(DefaultPolicy())
	g := &fakeReach{result: reach.Result{Status: reach.Pass, Code: reach.CodeReachable}}
	s.SetReachGate(g)
	ref := submitPending(t, s)
	if _, err := s.Approve(ctx, ref, "bob", "", nil); err != nil {
		t.Fatal(err)
	}
	g.result = unreachableDNS()
	if rec, err := s.Approve(ctx, ref, "bob", "", nil); err != nil || rec.Status != apiv1.PlanStatusApproved {
		t.Fatalf("second approve: %+v %v", rec, err)
	}
	if len(g.seen) != 1 {
		t.Fatalf("the gate ran %d times", len(g.seen))
	}
}
