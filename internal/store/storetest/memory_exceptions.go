package storetest

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
)

// exceptionsMemory is the memory store's part for store.ExceptionStore,
// guarded by memory.mu.
type exceptionsMemory struct {
	byID map[string]store.SecurityException // nil until the first create
}

func copySecurityException(e store.SecurityException) store.SecurityException {
	e.RevokedAt = pgTimePtr(e.RevokedAt)
	return e
}

func (m *memory) CreateSecurityException(_ context.Context, e store.SecurityException) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	e.ExpiresAt, e.CreatedAt, e.RevokedAt = pgTime(e.ExpiresAt), pgTime(e.CreatedAt), nil
	e.RevokedBy = ""
	if _, ok := m.exceptions.byID[e.ID]; ok {
		return fmt.Errorf("%w: security exception %s exists", store.ErrConflict, e.ID)
	}
	for _, have := range m.exceptions.byID {
		if have.TargetType == e.TargetType && have.TargetID == e.TargetID && have.RevokedAt == nil && have.ExpiresAt.After(e.CreatedAt) {
			return fmt.Errorf("%w: %s %s has an active exception", store.ErrConflict, e.TargetType, e.TargetID)
		}
	}
	if m.exceptions.byID == nil {
		m.exceptions.byID = map[string]store.SecurityException{}
	}
	m.exceptions.byID[e.ID] = e
	return nil
}

func (m *memory) RevokeSecurityException(_ context.Context, id, by string, at time.Time) (store.SecurityException, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.exceptions.byID[id]
	if !ok {
		return store.SecurityException{}, fmt.Errorf("%w: security exception %s", store.ErrNotFound, id)
	}
	if e.RevokedAt != nil {
		return store.SecurityException{}, fmt.Errorf("%w: security exception %s is revoked", store.ErrConflict, id)
	}
	e.RevokedAt, e.RevokedBy = pgTimePtr(&at), by
	m.exceptions.byID[id] = e
	return copySecurityException(e), nil
}

func (m *memory) ListSecurityExceptions(context.Context) ([]store.SecurityException, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []store.SecurityException{}
	for _, e := range m.exceptions.byID {
		out = append(out, copySecurityException(e))
	}
	slices.SortFunc(out, func(a, b store.SecurityException) int {
		return cmp.Or(b.CreatedAt.Compare(a.CreatedAt), strings.Compare(b.ID, a.ID))
	})
	return out, nil
}
