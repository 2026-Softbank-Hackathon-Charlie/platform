package onprem

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store/storetest"
)

func TestIssueReplayClaimAndRevocation(t *testing.T) {
	st := storetest.NewMemory()
	now := time.Now().UTC().Truncate(time.Microsecond)
	s := Service{Store: st, Origin: "https://api.example.test", Now: func() time.Time { return now }}
	req := CreateRequest{Environment: "onprem", InstallK3s: false}
	a, fresh, err := s.Create(t.Context(), "alice", "request-0001", req)
	if err != nil || !fresh || len(a.Code) != 47 || a.InstallCommand == "" {
		t.Fatalf("issuance: fresh=%v err=%v", fresh, err)
	}
	row, err := st.GetOnpremEnrollment(t.Context(), a.Enrollment.ID, now)
	sum := sha256.Sum256([]byte(a.Code))
	if err != nil || string(row.CodeHash) != string(sum[:]) || !row.ExpiresAt.Equal(now.Add(15*time.Minute)) {
		t.Fatal("grant is not hash-only with 15 minute expiry")
	}
	// A fresh service on the same persisted store cannot recover the code.
	s2 := Service{Store: st, Origin: s.Origin, Now: s.Now}
	b, fresh, err := s2.Create(t.Context(), "alice", "request-0001", req)
	if err != nil || fresh || b.Enrollment.ID != a.Enrollment.ID || b.Code != "" || b.InstallCommand != "" {
		t.Fatal("retry issued another grant or exposed the code")
	}
	if _, _, err = s.Create(t.Context(), "alice", "request-0001", CreateRequest{Environment: "other"}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("different request reused key: %v", err)
	}
	if _, err = s.Claim(t.Context(), a.Enrollment.ID, a.Code, ClaimRequest{Environment: "other"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("grant escaped environment: %v", err)
	}
	v, err := s.Claim(t.Context(), a.Enrollment.ID, a.Code, ClaimRequest{Environment: "onprem"})
	if err != nil || v.Status != "claimed" {
		t.Fatalf("claim %v", err)
	}
	for _, c := range v.Checks {
		if c.Status != "unknown" || c.CheckedAt != nil {
			t.Fatal("self-report invented a successful probe")
		}
	}
	if _, err = s.Claim(t.Context(), v.ID, a.Code, ClaimRequest{Environment: "onprem"}); !errors.Is(err, store.ErrConflict) {
		t.Fatal("code was consumed twice")
	}
	if _, err = s.Revoke(t.Context(), v.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Claim(t.Context(), v.ID, a.Code, ClaimRequest{Environment: "onprem"}); !errors.Is(err, store.ErrConflict) {
		t.Fatal("revocation did not fence claims")
	}
}

func TestExpiryAndUnsafeIssuance(t *testing.T) {
	now := time.Now().UTC()
	st := storetest.NewMemory()
	s := Service{Store: st, Origin: "https://api.example.test", Now: func() time.Time { return now }}
	a, _, err := s.Create(context.Background(), "alice", "request-0002", CreateRequest{Environment: "onprem"})
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(15 * time.Minute)
	if _, err = s.Claim(t.Context(), a.Enrollment.ID, a.Code, ClaimRequest{Environment: "onprem"}); !errors.Is(err, store.ErrConflict) {
		t.Fatal("expired code was consumed")
	}
	v, err := s.Get(t.Context(), a.Enrollment.ID)
	if err != nil || v.Status != "expired" {
		t.Fatal("expiry not persisted")
	}
	for _, name := range []string{"eks", "gke", "../onprem", "onprem;touch", "", "a\n"} {
		if _, _, err = s.Create(t.Context(), "alice", "request-0003", CreateRequest{Environment: name}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("accepted unsafe environment %q", name)
		}
	}
	s.Origin = "https://user:secret@example.test"
	if _, _, err = s.Create(t.Context(), "alice", "request-0004", CreateRequest{Environment: "onprem"}); !errors.Is(err, ErrUnavailable) || strings.Contains(err.Error(), "secret") {
		t.Fatal("invalid issuer origin accepted or leaked")
	}
}
