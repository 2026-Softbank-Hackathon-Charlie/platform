package storetest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
)

// exceptionCases are the store.ExceptionStore part of Conformance.
var exceptionCases = []struct {
	name string
	run  func(t *testing.T, s store.Store)
}{
	{"SecurityExceptionCreateList", testSecurityExceptionCreateList},
	{"SecurityExceptionActiveDuplicate", testSecurityExceptionActiveDuplicate},
	{"SecurityExceptionAfterExpiry", testSecurityExceptionAfterExpiry},
	{"SecurityExceptionRevoke", testSecurityExceptionRevoke},
}

// securityException is created at minute n and expires at minute n+expires.
func securityException(id, targetType, targetID string, n, expires int) store.SecurityException {
	return store.SecurityException{ID: id, TargetType: targetType, TargetID: targetID, Reason: "accepted risk",
		CreatedBy: "alice", CreatedAt: at(n), ExpiresAt: at(n + expires)}
}

func testSecurityExceptionCreateList(t *testing.T, s store.Store) {
	ctx := context.Background()
	if got, err := s.ListSecurityExceptions(ctx); err != nil || got == nil || len(got) != 0 {
		t.Fatalf("empty list: %v %v", got, err)
	}
	for _, e := range []store.SecurityException{
		securityException("sx_1", "finding", "f1", 1, 60),
		securityException("sx_3", "vulnerability", "v1", 3, 60),
		securityException("sx_2", "vulnerability", "f1", 2, 60), // same id, other type
	} {
		if err := s.CreateSecurityException(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.ListSecurityExceptions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].ID != "sx_3" || got[1].ID != "sx_2" || got[2].ID != "sx_1" {
		t.Fatalf("order: %+v", got)
	}
	e := got[2]
	if e.TargetType != "finding" || e.TargetID != "f1" || e.Reason != "accepted risk" || e.CreatedBy != "alice" ||
		!e.CreatedAt.Equal(at(1).Truncate(time.Microsecond)) || e.CreatedAt.Location().String() != "UTC" ||
		!e.ExpiresAt.Equal(at(61).Truncate(time.Microsecond)) || e.ExpiresAt.Location().String() != "UTC" ||
		e.RevokedAt != nil || e.RevokedBy != "" {
		t.Fatalf("row %+v", e)
	}
}

func testSecurityExceptionActiveDuplicate(t *testing.T, s store.Store) {
	ctx := context.Background()
	if err := s.CreateSecurityException(ctx, securityException("sx_1", "finding", "f1", 1, 60)); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSecurityException(ctx, securityException("sx_2", "finding", "f1", 2, 60)); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("active duplicate: %v, want ErrConflict", err)
	}
	if err := s.CreateSecurityException(ctx, securityException("sx_1", "finding", "f2", 2, 60)); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("reused id: %v, want ErrConflict", err)
	}
	if got, _ := s.ListSecurityExceptions(ctx); len(got) != 1 {
		t.Fatalf("got %+v", got)
	}
}

func testSecurityExceptionAfterExpiry(t *testing.T, s store.Store) {
	ctx := context.Background()
	if err := s.CreateSecurityException(ctx, securityException("sx_1", "finding", "f1", 1, 10)); err != nil {
		t.Fatal(err)
	}
	// Created at minute 11, when sx_1 has expired (expires_at is not after created_at).
	if err := s.CreateSecurityException(ctx, securityException("sx_2", "finding", "f1", 11, 10)); err != nil {
		t.Fatalf("after expiry: %v", err)
	}
	// A revoked exception does not block either.
	if _, err := s.RevokeSecurityException(ctx, "sx_2", "bob", at(12)); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSecurityException(ctx, securityException("sx_3", "finding", "f1", 13, 10)); err != nil {
		t.Fatalf("after revoke: %v", err)
	}
}

func testSecurityExceptionRevoke(t *testing.T, s store.Store) {
	ctx := context.Background()
	if err := s.CreateSecurityException(ctx, securityException("sx_1", "finding", "f1", 1, 60)); err != nil {
		t.Fatal(err)
	}
	got, err := s.RevokeSecurityException(ctx, "sx_1", "bob", at(5))
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "sx_1" || got.TargetID != "f1" || got.RevokedBy != "bob" || got.RevokedAt == nil ||
		!got.RevokedAt.Equal(at(5).Truncate(time.Microsecond)) || got.RevokedAt.Location().String() != "UTC" {
		t.Fatalf("revoked %+v", got)
	}
	if _, err := s.RevokeSecurityException(ctx, "sx_1", "carol", at(6)); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("revoke twice: %v, want ErrConflict", err)
	}
	if _, err := s.RevokeSecurityException(ctx, "sx_missing", "bob", at(6)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown id: %v, want ErrNotFound", err)
	}
	list, _ := s.ListSecurityExceptions(ctx)
	if len(list) != 1 || list[0].RevokedBy != "bob" || list[0].RevokedAt == nil || !list[0].RevokedAt.Equal(at(5).Truncate(time.Microsecond)) {
		t.Fatalf("list %+v", list)
	}
}
