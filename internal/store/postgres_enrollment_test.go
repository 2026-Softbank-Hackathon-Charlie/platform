package store_test

import (
	"crypto/sha256"
	"testing"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store/storetest"
)

func TestPostgresEnrollmentConformance(t *testing.T) {
	testDatabaseURL(t)
	storetest.ConformanceEnrollment(t, func(t *testing.T) store.Store { _, url := newSchema(t); return openSchema(t, url) })
}

func TestEnrollmentSurvivesReopenWithoutRecoverableCode(t *testing.T) {
	_, url := newSchema(t)
	s := openSchema(t, url)
	now := time.Now().UTC().Truncate(time.Microsecond)
	hash, key := sha256.Sum256([]byte("bootstrap-code-not-stored")), sha256.Sum256([]byte("restart-key"))
	in := store.OnpremEnrollment{ID: "enr_restart", Actor: "alice", Environment: "onprem", CodeHash: hash[:], RequestHash: key[:], Status: "pending", CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(15 * time.Minute)}
	if _, _, err := s.CreateOnpremEnrollment(t.Context(), in); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s = openSchema(t, url)
	defer s.Close()
	row, err := s.GetOnpremEnrollment(t.Context(), in.ID, now)
	if err != nil || row.Status != "pending" || string(row.CodeHash) != string(hash[:]) {
		t.Fatal("persistent grant changed across restart")
	}
	if _, err = s.ClaimOnpremEnrollment(t.Context(), in.ID, hash[:], now); err != nil {
		t.Fatal(err)
	}
}
