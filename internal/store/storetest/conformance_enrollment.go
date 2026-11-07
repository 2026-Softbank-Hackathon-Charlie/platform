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
