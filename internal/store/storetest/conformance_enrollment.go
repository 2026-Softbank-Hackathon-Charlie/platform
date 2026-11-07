package storetest

import (
	"crypto/sha256"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
)

func ConformanceEnrollment(t *testing.T, newStore func(*testing.T) store.Store) {
	t.Run("BootstrapReservationIsOneUse", func(t *testing.T) {
		s := newStore(t)
		defer s.Close()
		now := time.Now().UTC().Truncate(time.Microsecond)
		code, key, session := sha256.Sum256([]byte("bootstrap-reservation-code")), sha256.Sum256([]byte("bootstrap-reservation-key")), sha256.Sum256([]byte("bootstrap-reservation-session"))
		e := store.OnpremEnrollment{ID: "enr_bootstrap_reservation", Actor: "alice", Environment: "reserved-edge", Status: "pending", CodeHash: code[:], RequestHash: key[:], CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(time.Minute)}
		if _, _, err := s.CreateOnpremEnrollment(t.Context(), e); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ClaimOnpremRegistration(t.Context(), e.ID, code[:], session[:], now.Add(time.Hour), now); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		var mu sync.Mutex
		success := 0
		for range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, err := s.ReserveOnpremBootstrap(t.Context(), e.ID, session[:], now)
				mu.Lock()
				defer mu.Unlock()
				if err == nil {
					success++
				} else if !errors.Is(err, store.ErrConflict) {
					t.Error(err)
				}
			}()
		}
		wg.Wait()
		if success != 1 {
			t.Fatalf("mint reserved %d times", success)
		}
		row, err := s.GetOnpremEnrollment(t.Context(), e.ID, now)
		if err != nil || row.Stage != "bootstrap_issued" {
			t.Fatal("reservation not persisted", err)
		}
		if _, err = s.RevokeOnpremEnrollment(t.Context(), e.ID, now); err != nil {
			t.Fatal(err)
		}
		if _, err = s.ReserveOnpremBootstrap(t.Context(), e.ID, session[:], now); !errors.Is(err, store.ErrConflict) {
			t.Fatal("revoked bootstrap reserved", err)
		}
	})
	t.Run("RegistrationOwnershipLeaseAndRevocation", func(t *testing.T) {
		s := newStore(t)
		defer s.Close()
		now := time.Now().UTC().Truncate(time.Microsecond)
		code, session := sha256.Sum256([]byte("registration-code")), sha256.Sum256([]byte("registration-session"))
		key := sha256.Sum256([]byte("registration-key"))
		in := store.OnpremEnrollment{ID: "enr_registration", Actor: "alice", Environment: "edge", Status: "pending", CodeHash: code[:], RequestHash: key[:], CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(time.Minute)}
		if _, _, err := s.CreateOnpremEnrollment(t.Context(), in); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ClaimOnpremRegistration(t.Context(), in.ID, code[:], session[:], now.Add(30*24*time.Hour), now); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ClaimOnpremRegistration(t.Context(), in.ID, code[:], session[:], now.Add(time.Hour), now); !errors.Is(err, store.ErrConflict) {
			t.Fatal("claim replay accepted", err)
		}
		row, err := s.BeginOnpremRegistration(t.Context(), in.ID, session[:], "cluster-one", "lease-one", now, now.Add(time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.BeginOnpremRegistration(t.Context(), in.ID, session[:], "cluster-two", "lease-two", now, now.Add(time.Minute)); !errors.Is(err, store.ErrConflict) {
			t.Fatal("UID changed", err)
		}
		if _, err := s.BeginOnpremRegistration(t.Context(), in.ID, session[:], "cluster-one", "lease-two", now, now.Add(time.Minute)); !errors.Is(err, store.ErrConflict) {
			t.Fatal("active lease stolen", err)
		}
		if _, err := s.SaveOnpremRegistration(t.Context(), in.ID, "lease-one", row.Version, store.OnpremRegistrationUpdate{Status: "ready", Stage: "done", RegistrationCiphertext: []byte("encrypted")}, now); !errors.Is(err, store.ErrConflict) {
			t.Fatal("ready without observations", err)
		}
		checks := []store.OnpremEnrollmentCheck{}
		for _, id := range []string{"k3s", "network", "permissions", "image_pull", "hello_world"} {
			stamp := now
			checks = append(checks, store.OnpremEnrollmentCheck{ID: id, Status: "passed", CheckedAt: &stamp})
		}
		update := store.OnpremRegistrationUpdate{Status: "ready", Stage: "done", Checks: checks, RegistrationCiphertext: []byte("encrypted"), ReleaseLease: true}
		ready, err := s.SaveOnpremRegistration(t.Context(), in.ID, "lease-one", row.Version, update, now)
		if err != nil || ready.ClusterUID != "cluster-one" {
			t.Fatal("save", ready, err)
		}
		restored, err := s.ListRegisteredOnpremEnvironments(t.Context(), now)
		if err != nil || len(restored) != 1 || string(restored[0].RegistrationCiphertext) != "encrypted" {
			t.Fatal("restore", restored, err)
		}
		// A second enrollment can never take a bound environment, even after revocation.
		in.ID = "enr_other"
		otherCode, otherKey := sha256.Sum256([]byte("other-code")), sha256.Sum256([]byte("other-key"))
		in.CodeHash, in.RequestHash = otherCode[:], otherKey[:]
		if _, _, err = s.CreateOnpremEnrollment(t.Context(), in); err != nil {
			t.Fatal(err)
		}
		if _, err = s.ClaimOnpremRegistration(t.Context(), in.ID, otherCode[:], session[:], now.Add(time.Hour), now); err != nil {
			t.Fatal(err)
		}
		if _, err = s.BeginOnpremRegistration(t.Context(), in.ID, session[:], "cluster-one", "other-lease", now, now.Add(time.Minute)); !errors.Is(err, store.ErrConflict) {
			t.Fatal("environment stolen", err)
		}
		renew, err := s.BeginOnpremRegistration(t.Context(), ready.ID, session[:], "cluster-one", "renew", now, now.Add(time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.SaveOnpremRegistration(t.Context(), ready.ID, "lease-one", row.Version, update, now); !errors.Is(err, store.ErrConflict) {
			t.Fatal("stale version accepted", err)
		}
		if _, err = s.RevokeOnpremEnrollment(t.Context(), ready.ID, now); err != nil {
			t.Fatal(err)
		}
		if _, err = s.SaveOnpremRegistration(t.Context(), ready.ID, "renew", renew.Version, update, now); !errors.Is(err, store.ErrConflict) {
			t.Fatal("late save undid revoke", err)
		}
		if _, err = s.BeginOnpremRegistration(t.Context(), ready.ID, session[:], "cluster-one", "revoked", now, now.Add(time.Minute)); !errors.Is(err, store.ErrConflict) {
			t.Fatal("revoked session accepted", err)
		}
		restored, err = s.ListRegisteredOnpremEnvironments(t.Context(), now)
		if err != nil || len(restored) != 0 {
			t.Fatal("revoked restored", restored, err)
		}
		if _, err = s.BeginOnpremRegistration(t.Context(), in.ID, session[:], "cluster-one", "other-lease", now, now.Add(time.Minute)); !errors.Is(err, store.ErrConflict) {
			t.Fatal("revoked binding transferred", err)
		}
	})
	t.Run("ExpiredSessionAndLeaseAreFenced", func(t *testing.T) {
		s := newStore(t)
		defer s.Close()
		now := time.Now().UTC().Truncate(time.Microsecond)
		code, key, session := sha256.Sum256([]byte("expiration-code")), sha256.Sum256([]byte("expiration-key")), sha256.Sum256([]byte("expiration-session"))
		in := store.OnpremEnrollment{ID: "enr_expiring_session", Actor: "alice", Environment: "edge-expiring", Status: "pending", CodeHash: code[:], RequestHash: key[:], CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(time.Minute)}
		if _, _, err := s.CreateOnpremEnrollment(t.Context(), in); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ClaimOnpremRegistration(t.Context(), in.ID, code[:], session[:], now.Add(time.Hour), now); err != nil {
			t.Fatal(err)
		}
		row, err := s.BeginOnpremRegistration(t.Context(), in.ID, session[:], "cluster-expiring", "lease", now, now.Add(time.Second))
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.SaveOnpremRegistration(t.Context(), in.ID, "lease", row.Version, store.OnpremRegistrationUpdate{Status: "failed", Stage: "timeout"}, now.Add(time.Second)); !errors.Is(err, store.ErrConflict) {
			t.Fatal("expired lease accepted", err)
		}
		if _, err = s.BeginOnpremRegistration(t.Context(), in.ID, session[:], "cluster-expiring", "new", now.Add(time.Second), now.Add(time.Minute)); err != nil {
			t.Fatal("expired lease not recoverable", err)
		}
		if _, err = s.BeginOnpremRegistration(t.Context(), in.ID, session[:], "cluster-expiring", "expired", now.Add(time.Hour), now.Add(2*time.Hour)); !errors.Is(err, store.ErrConflict) {
			t.Fatal("expired session accepted", err)
		}
	})
	t.Run("AtomicClaimAndRevoke", func(t *testing.T) {
		s := newStore(t)
		defer s.Close()
		now := time.Now().UTC().Truncate(time.Microsecond)
		hash, key := sha256.Sum256([]byte("random-code")), sha256.Sum256([]byte("key-one"))
		in := store.OnpremEnrollment{ID: "enr_test", Actor: "alice", Environment: "onprem", CodeHash: hash[:], RequestHash: key[:], Status: "pending", CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(15 * time.Minute)}
		row, created, err := s.CreateOnpremEnrollment(t.Context(), in)
		if err != nil || !created {
			t.Fatalf("create %v", err)
		}
		replay, created, err := s.CreateOnpremEnrollment(t.Context(), in)
		if err != nil || created || replay.ID != row.ID {
			t.Fatal("idempotency failed")
		}
		var wg sync.WaitGroup
		var mu sync.Mutex
		success := 0
		for range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, err := s.ClaimOnpremEnrollment(t.Context(), in.ID, hash[:], now)
				mu.Lock()
				defer mu.Unlock()
				if err == nil {
					success++
				} else if !errors.Is(err, store.ErrConflict) {
					t.Errorf("unexpected claim: %v", err)
				}
			}()
		}
		wg.Wait()
		if success != 1 {
			t.Fatalf("code consumed %d times", success)
		}
		if _, err = s.RevokeOnpremEnrollment(t.Context(), in.ID, now); err != nil {
			t.Fatal(err)
		}
		if _, err = s.ClaimOnpremEnrollment(t.Context(), in.ID, hash[:], now); !errors.Is(err, store.ErrConflict) {
			t.Fatal("revoked grant accepted")
		}
	})
	t.Run("RevokeWinsAndExpiryPersists", func(t *testing.T) {
		s := newStore(t)
		defer s.Close()
		now := time.Now().UTC().Truncate(time.Microsecond)
		hash, key := sha256.Sum256([]byte("code-two")), sha256.Sum256([]byte("key-two"))
		in := store.OnpremEnrollment{ID: "enr_race", Actor: "bob", Environment: "onprem", CodeHash: hash[:], RequestHash: key[:], Status: "pending", CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(15 * time.Minute)}
		if _, _, err := s.CreateOnpremEnrollment(t.Context(), in); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, err := s.ClaimOnpremEnrollment(t.Context(), in.ID, hash[:], now)
			if err != nil && !errors.Is(err, store.ErrConflict) {
				t.Errorf("claim race: %v", err)
			}
		}()
		go func() {
			defer wg.Done()
			if _, err := s.RevokeOnpremEnrollment(t.Context(), in.ID, now); err != nil {
				t.Errorf("revoke race: %v", err)
			}
		}()
		wg.Wait()
		row, err := s.GetOnpremEnrollment(t.Context(), in.ID, now)
		if err != nil || row.Status != "revoked" {
			t.Fatal("late claim undid revocation")
		}
		in.ID = "enr_expired"
		h2, k2 := sha256.Sum256([]byte("code-three")), sha256.Sum256([]byte("key-three"))
		in.CodeHash, in.RequestHash = h2[:], k2[:]
		if _, _, err = s.CreateOnpremEnrollment(t.Context(), in); err != nil {
			t.Fatal(err)
		}
		if _, err = s.ClaimOnpremEnrollment(t.Context(), in.ID, h2[:], in.ExpiresAt); !errors.Is(err, store.ErrConflict) {
			t.Fatal("expired grant consumed")
		}
		row, err = s.GetOnpremEnrollment(t.Context(), in.ID, in.ExpiresAt)
		if err != nil || row.Status != "expired" {
			t.Fatal("expiry not observed")
		}
	})
}
