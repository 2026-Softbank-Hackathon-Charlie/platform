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

func TestRegistrationSessionAndLeaseSurviveReopen(t *testing.T) {
	_, url := newSchema(t)
	s := openSchema(t, url)
	now := time.Now().UTC().Truncate(time.Microsecond)
	code, key, session := sha256.Sum256([]byte("persist-code")), sha256.Sum256([]byte("persist-key")), sha256.Sum256([]byte("persist-session"))
	in := store.OnpremEnrollment{ID: "enr_registration_restart", Actor: "alice", Environment: "restart-edge", Status: "pending", CodeHash: code[:], RequestHash: key[:], CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(time.Minute)}
	if _, _, err := s.CreateOnpremEnrollment(t.Context(), in); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimOnpremRegistration(t.Context(), in.ID, code[:], session[:], now.Add(30*24*time.Hour), now); err != nil {
		t.Fatal(err)
	}
	lease, err := s.BeginOnpremRegistration(t.Context(), in.ID, session[:], "restart-cluster", "restart-lease", now, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	s = openSchema(t, url)
	defer s.Close()
	row, err := s.GetOnpremEnrollment(t.Context(), in.ID, now)
	if err != nil || row.ClusterUID != "restart-cluster" || row.Version != lease.Version || string(row.SessionHash) != string(session[:]) || row.LeaseID != "restart-lease" {
		t.Fatal("lost registration identity across restart", err)
	}
	if _, err = s.BeginOnpremRegistration(t.Context(), in.ID, session[:], "other-cluster", "other-lease", now.Add(2*time.Minute), now.Add(3*time.Minute)); err != store.ErrConflict {
		t.Fatal("restart lost UID pin", err)
	}
}
