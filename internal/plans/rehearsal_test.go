package plans

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store/storetest"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

func hexSum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func report(passed bool, planSum, manifestSum string) []byte {
	return []byte(fmt.Sprintf(`{"schemaVersion":%q,"passed":%t,"candidate":{"planSha256":%q,"manifestSha256":%q},"attempts":[]}`,
		RehearsalReportSchema, passed, planSum, manifestSum))
}

// An AI plan (submitted by an agent) is approved only with a passing
// rehearsal report for the plan body and target.yaml attached now (#24).
func TestApproveAIPlanNeedsMatchingRehearsal(t *testing.T) {
	raw := withManifest("attachment:target.yaml")
	manifest := []byte("apiVersion: postgresql.cnpg.io/v1\nkind: Cluster\n")
	setup := func(t *testing.T) (*Service, string) {
		t.Helper()
		s := New(DefaultPolicy(), storetest.NewMemory())
		rec, err := s.SubmitAs(ctx, raw, "hopai", apiv1.RoleAgent)
		if err != nil {
			t.Fatal(err)
		}
		if rec.SubmittedRole == nil || *rec.SubmittedRole != apiv1.RoleAgent {
			t.Fatalf("submittedRole = %v", rec.SubmittedRole)
		}
		if _, err := s.PutAttachment(ctx, rec.Ref, "target.yaml", manifest, "hopai"); err != nil {
			t.Fatal(err)
		}
		return s, rec.Ref
	}
	approve := func(s *Service, ref string) error {
		cur, err := s.Get(ctx, ref)
		if err != nil {
			return err
		}
		_, err = s.Approve(ctx, ref, "op", "", &cur.Revision)
		return err
	}
	cases := map[string]struct {
		report []byte // nil: no report
		want   string // "" approves
	}{
		"no report":           {nil, "no rehearsal report"},
		"failed":              {report(false, hexSum(raw), hexSum(manifest)), "did not pass"},
		"other plan body":     {report(true, hexSum([]byte("x")), hexSum(manifest)), "different plan body"},
		"other manifest":      {report(true, hexSum(raw), hexSum([]byte("old"))), "different target.yaml"},
		"wrong schema":        {[]byte(`{"schemaVersion":"v0","passed":true}`), "schemaVersion"},
		"not json":            {[]byte("nope"), "not valid JSON"},
		"passed for this one": {report(true, hexSum(raw), hexSum(manifest)), ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			s, ref := setup(t)
			if tc.report != nil {
				if _, err := s.PutAttachment(ctx, ref, RehearsalReportName, tc.report, "hopai"); err != nil {
					t.Fatal(err)
				}
			}
			got, err := s.Get(ctx, ref)
			if err != nil || got.Rehearsal == nil || !got.Rehearsal.Required || got.Rehearsal.Passed != (tc.want == "") {
				t.Fatalf("gate = %+v, %v", got.Rehearsal, err)
			}
			err = approve(s, ref)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("approve: %v", err)
				}
				return
			}
			if !errors.Is(err, ErrRehearsal) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("approve err = %v, want %q", err, tc.want)
			}
			if got.Rehearsal.Reason == nil || !strings.Contains(*got.Rehearsal.Reason, tc.want) {
				t.Fatalf("gate reason = %v", got.Rehearsal.Reason)
			}
		})
	}
}

// A new target.yaml after a passing rehearsal needs a new rehearsal.
func TestApproveAIPlanAfterManifestChange(t *testing.T) {
	s := New(DefaultPolicy(), storetest.NewMemory())
	raw := withManifest("attachment:target.yaml")
	rec, err := s.SubmitAs(ctx, raw, "hopai", apiv1.RoleAgent)
	if err != nil {
		t.Fatal(err)
	}
	m1 := []byte("kind: Cluster\n")
	for _, a := range []struct {
		name string
		body []byte
	}{{"target.yaml", m1}, {RehearsalReportName, report(true, hexSum(raw), hexSum(m1))}, {"target.yaml", []byte("kind: Cluster # changed\n")}} {
		if _, err := s.PutAttachment(ctx, rec.Ref, a.name, a.body, "hopai"); err != nil {
			t.Fatal(err)
		}
	}
	cur, _ := s.Get(ctx, rec.Ref)
	if _, err := s.Approve(ctx, rec.Ref, "op", "", &cur.Revision); !errors.Is(err, ErrRehearsal) {
		t.Fatalf("approve after the manifest changed: %v", err)
	}
}

// Plans a human wrote keep the plain approval flow; plans stored before the
// role was kept need the gate only when they carry a rehearsal report.
func TestApproveWithoutRehearsalForOtherPlans(t *testing.T) {
	for _, role := range []apiv1.Role{apiv1.RoleOperator, ""} {
		s := New(DefaultPolicy(), storetest.NewMemory())
		rec, err := s.SubmitAs(ctx, planJSON(""), "alice", role)
		if err != nil {
			t.Fatal(err)
		}
		if rec.Rehearsal == nil || rec.Rehearsal.Required {
			t.Fatalf("role %q: gate = %+v", role, rec.Rehearsal)
		}
		if _, err := s.Approve(ctx, rec.Ref, "op", "", nil); err != nil {
			t.Fatalf("role %q: approve: %v", role, err)
		}
	}
	// legacy plan (no role) with a failed report, like the plans hopai submitted before the role was kept
	s := New(DefaultPolicy(), storetest.NewMemory())
	raw := planJSON("")
	rec, err := s.Submit(ctx, raw, "hopai")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutAttachment(ctx, rec.Ref, RehearsalReportName, report(false, hexSum(raw), ""), "hopai"); err != nil {
		t.Fatal(err)
	}
	cur, _ := s.Get(ctx, rec.Ref)
	if _, err := s.Approve(ctx, rec.Ref, "op", "", &cur.Revision); !errors.Is(err, ErrRehearsal) {
		t.Fatalf("legacy plan with a failed report: %v", err)
	}
}

// The gate only shows on plans waiting for approval.
func TestRehearsalGateOnlyWhilePending(t *testing.T) {
	s := New(DefaultPolicy(), storetest.NewMemory())
	rec, err := s.SubmitAs(ctx, planJSON(""), "alice", apiv1.RoleOperator)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reject(ctx, rec.Ref, "op", "no"); err != nil {
		t.Fatal(err)
	}
	list, err := s.List(ctx)
	if err != nil || len(list) != 1 || list[0].Rehearsal != nil || list[0].Status != apiv1.PlanStatus(store.PlanRejected) {
		t.Fatalf("list = %+v, %v", list, err)
	}
}

// Only the principal that submitted an AI plan can supply its rehearsal report.
func TestRehearsalReportFromAnotherPrincipal(t *testing.T) {
	s := New(DefaultPolicy(), storetest.NewMemory())
	raw := planJSON("")
	rec, err := s.SubmitAs(ctx, raw, "hopai", apiv1.RoleAgent)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutAttachment(ctx, rec.Ref, RehearsalReportName, report(true, hexSum(raw), ""), "someone-else"); err != nil {
		t.Fatal(err)
	}
	cur, _ := s.Get(ctx, rec.Ref)
	if _, err := s.Approve(ctx, rec.Ref, "op", "", &cur.Revision); !errors.Is(err, ErrRehearsal) || !strings.Contains(err.Error(), "uploaded by") {
		t.Fatalf("approve with another principal's report: %v", err)
	}
}

// The rehearsal report cannot double as the target manifest.
func TestManifestCannotBeTheRehearsalReport(t *testing.T) {
	_, err := New(DefaultPolicy(), storetest.NewMemory()).Submit(ctx, withManifest("attachment:"+RehearsalReportName), "hopai")
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "rehearsal report") {
		t.Fatalf("submit: %v", err)
	}
}
