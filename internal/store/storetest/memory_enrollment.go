package storetest

import (
	"bytes"
	"context"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
)

func copyEnrollment(e store.OnpremEnrollment) store.OnpremEnrollment {
	e.CodeHash, e.RequestHash = bytes.Clone(e.CodeHash), bytes.Clone(e.RequestHash)
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
	if e.Status == "ready" {
		return e, store.ErrConflict
	}
	e.Status = "revoked"
	e.UpdatedAt = pgTime(now)
	m.enrollments[id] = e
	return copyEnrollment(e), nil
}
