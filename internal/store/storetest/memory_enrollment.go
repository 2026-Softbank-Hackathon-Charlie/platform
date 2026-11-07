package storetest

import (
	"bytes"
	"context"
	"sort"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
)

func copyEnrollment(e store.OnpremEnrollment) store.OnpremEnrollment {
	e.CodeHash, e.RequestHash = bytes.Clone(e.CodeHash), bytes.Clone(e.RequestHash)
	e.SessionHash, e.RegistrationCiphertext = bytes.Clone(e.SessionHash), bytes.Clone(e.RegistrationCiphertext)
	e.Checks = append([]store.OnpremEnrollmentCheck(nil), e.Checks...)
	for i := range e.Checks {
		if e.Checks[i].CheckedAt != nil {
			v := *e.Checks[i].CheckedAt
			e.Checks[i].CheckedAt = &v
		}
	}
	e.CreatedAt, e.UpdatedAt, e.ExpiresAt = pgTime(e.CreatedAt), pgTime(e.UpdatedAt), pgTime(e.ExpiresAt)
	return e
}
func (m *memory) CreateOnpremEnrollment(_ context.Context, e store.OnpremEnrollment) (store.OnpremEnrollment, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.enrollments == nil {
		m.enrollments = map[string]store.OnpremEnrollment{}
	}
	n := 0
	for _, old := range m.enrollments {
		if old.Actor == e.Actor && bytes.Equal(old.RequestHash, e.RequestHash) {
			if old.Environment != e.Environment || old.InstallK3s != e.InstallK3s {
				return store.OnpremEnrollment{}, false, store.ErrConflict
			}
			return copyEnrollment(old), false, nil
		}
		if old.ID == e.ID || bytes.Equal(old.CodeHash, e.CodeHash) {
			return store.OnpremEnrollment{}, false, store.ErrConflict
		}
		if (old.Status == "pending" || old.Status == "claimed" || old.Status == "verifying") && old.ExpiresAt.After(e.CreatedAt) {
			n++
		}
	}
	if n >= 64 {
		return store.OnpremEnrollment{}, false, store.ErrConflict
	}
	e = copyEnrollment(e)
	m.enrollments[e.ID] = e
	return copyEnrollment(e), true, nil
}
func (m *memory) GetOnpremEnrollment(_ context.Context, id string, now time.Time) (store.OnpremEnrollment, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.enrollments[id]
	if !ok {
		return e, store.ErrNotFound
	}
	if e.Status == "pending" && !e.ExpiresAt.After(now) {
		e.Status = "expired"
		e.UpdatedAt = pgTime(now)
		m.enrollments[id] = e
	}
	return copyEnrollment(e), nil
}
func (m *memory) ClaimOnpremEnrollment(_ context.Context, id string, hash []byte, now time.Time) (store.OnpremEnrollment, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.enrollments[id]
	if !ok || e.Status != "pending" || !e.ExpiresAt.After(now) || !bytes.Equal(e.CodeHash, hash) {
		return store.OnpremEnrollment{}, store.ErrConflict
	}
	e.Status = "claimed"
	e.UpdatedAt = pgTime(now)
	m.enrollments[id] = e
	return copyEnrollment(e), nil
}
func (m *memory) RevokeOnpremEnrollment(_ context.Context, id string, now time.Time) (store.OnpremEnrollment, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.enrollments[id]
	if !ok {
		return e, store.ErrNotFound
	}
	e.Status = "revoked"
	e.SessionHash = nil
	e.SessionExpiresAt = time.Time{}
	e.RegistrationCiphertext = nil
	e.LeaseID = ""
	e.LeaseExpiresAt = time.Time{}
	e.Version++
	e.UpdatedAt = pgTime(now)
	m.enrollments[id] = e
	return copyEnrollment(e), nil
}

func (m *memory) ClaimOnpremRegistration(_ context.Context, id string, codeHash, sessionHash []byte, sessionExpiresAt, now time.Time) (store.OnpremEnrollment, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.enrollments[id]
	if !ok || e.Status != "pending" || !e.ExpiresAt.After(now) || !bytes.Equal(e.CodeHash, codeHash) || len(sessionHash) != 32 || !sessionExpiresAt.After(now) || sessionExpiresAt.Sub(now) > 30*24*time.Hour {
		return store.OnpremEnrollment{}, store.ErrConflict
	}
	e.Status = "claimed"
	e.SessionHash = bytes.Clone(sessionHash)
	e.SessionExpiresAt = pgTime(sessionExpiresAt)
	e.UpdatedAt = pgTime(now)
	e.Version++
	m.enrollments[id] = e
	return copyEnrollment(e), nil
}

func (m *memory) ReserveOnpremBootstrap(_ context.Context, id string, sessionHash []byte, now time.Time) (store.OnpremEnrollment, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.enrollments[id]
	if !ok || len(sessionHash) != 32 || !bytes.Equal(e.SessionHash, sessionHash) || !e.SessionExpiresAt.After(now) || e.Status != "claimed" || e.Stage != "" || e.ClusterUID != "" {
		return store.OnpremEnrollment{}, store.ErrConflict
	}
	e.Stage = "bootstrap_issued"
	e.UpdatedAt = pgTime(now)
	e.Version++
	m.enrollments[id] = e
	return copyEnrollment(e), nil
}

func (m *memory) BeginOnpremRegistration(_ context.Context, id string, sessionHash []byte, clusterUID, leaseID string, now, leaseUntil time.Time) (store.OnpremEnrollment, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.enrollments[id]
	if !ok || len(sessionHash) != 32 || !bytes.Equal(e.SessionHash, sessionHash) || !e.SessionExpiresAt.After(now) || (e.Status != "claimed" && e.Status != "verifying" && e.Status != "ready" && e.Status != "failed") || clusterUID == "" || len(clusterUID) > 128 || leaseID == "" || len(leaseID) > 128 || !leaseUntil.After(now) || leaseUntil.Sub(now) > 5*time.Minute || (e.ClusterUID != "" && e.ClusterUID != clusterUID) || (e.LeaseID != "" && e.LeaseExpiresAt.After(now)) {
		return store.OnpremEnrollment{}, store.ErrConflict
	}
	for _, old := range m.enrollments {
		if old.Environment == e.Environment && old.ClusterUID != "" && (old.ID != id || old.ClusterUID != clusterUID) {
			return store.OnpremEnrollment{}, store.ErrConflict
		}
	}
	e.ClusterUID = clusterUID
	e.LeaseID = leaseID
	e.LeaseExpiresAt = pgTime(leaseUntil)
	e.UpdatedAt = pgTime(now)
	e.Version++
	m.enrollments[id] = e
	return copyEnrollment(e), nil
}

func (m *memory) SaveOnpremRegistration(_ context.Context, id, leaseID string, expectedVersion int64, u store.OnpremRegistrationUpdate, now time.Time) (store.OnpremEnrollment, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.enrollments[id]
	if !ok || leaseID == "" || e.LeaseID != leaseID || e.Version != expectedVersion || !e.LeaseExpiresAt.After(now) || !e.SessionExpiresAt.After(now) || (e.Status != "claimed" && e.Status != "verifying" && e.Status != "ready" && e.Status != "failed") || !u.Valid(now, len(e.RegistrationCiphertext) > 0) {
		return store.OnpremEnrollment{}, store.ErrConflict
	}
	e.Status, e.Stage, e.Checks = u.Status, u.Stage, u.Checks
	if u.RegistrationCiphertext != nil {
		e.RegistrationCiphertext = bytes.Clone(u.RegistrationCiphertext)
	}
	if u.SessionExpiresAt != nil {
		e.SessionExpiresAt = pgTime(*u.SessionExpiresAt)
	}
	if u.ReleaseLease {
		e.LeaseID = ""
		e.LeaseExpiresAt = time.Time{}
	}
	e.UpdatedAt = pgTime(now)
	e.Version++
	e = copyEnrollment(e)
	m.enrollments[id] = e
	return copyEnrollment(e), nil
}

func (m *memory) ListRegisteredOnpremEnvironments(_ context.Context, now time.Time) ([]store.OnpremEnrollment, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []store.OnpremEnrollment{}
	for _, e := range m.enrollments {
		if e.Status == "ready" && e.SessionExpiresAt.After(now) && len(e.RegistrationCiphertext) > 0 {
			out = append(out, copyEnrollment(e))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Environment < out[j].Environment })
	return out, nil
}
