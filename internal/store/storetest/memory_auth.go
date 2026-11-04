package storetest

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
)

// authMemory holds sessions and device logins, by the string of their hash.
// Its zero value is ready; the store's mutex guards it.
type authMemory struct {
	sessions map[string]store.Session
	devices  map[string]store.DeviceLogin
}

func copySession(s store.Session) store.Session {
	s.TokenHash = bytes.Clone(s.TokenHash)
	s.CreatedAt, s.ExpiresAt = pgTime(s.CreatedAt), pgTime(s.ExpiresAt)
	return s
}

func copyDeviceLogin(d store.DeviceLogin) store.DeviceLogin {
	d.DeviceCodeHash = bytes.Clone(d.DeviceCodeHash)
	d.CreatedAt, d.ExpiresAt = pgTime(d.CreatedAt), pgTime(d.ExpiresAt)
	d.ReviewedAt = pgTimePtr(d.ReviewedAt)
	return d
}

// insertSession stores s unless its hash exists. Call with m.mu held.
func (m *memory) insertSession(s store.Session) (store.Session, error) {
	if _, ok := m.auth.sessions[string(s.TokenHash)]; ok {
		return store.Session{}, fmt.Errorf("%w: session exists", store.ErrConflict)
	}
	if m.auth.sessions == nil {
		m.auth.sessions = map[string]store.Session{}
	}
	s = copySession(s.Clean())
	m.auth.sessions[string(s.TokenHash)] = s
	return copySession(s), nil
}

// purgeSessions removes the sessions that expired at now. Call with m.mu held.
func (m *memory) purgeSessions(now time.Time) {
	now = pgTime(now)
	for k, old := range m.auth.sessions {
		if !old.ExpiresAt.After(now) {
			delete(m.auth.sessions, k)
		}
	}
}

func (m *memory) CreateSession(_ context.Context, s store.Session) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.purgeSessions(s.CreatedAt)
	_, err := m.insertSession(s)
	return err
}

func (m *memory) GetSession(_ context.Context, tokenHash []byte, now time.Time) (store.Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.auth.sessions[string(tokenHash)]
	if !ok || !s.ExpiresAt.After(pgTime(now)) {
		return store.Session{}, fmt.Errorf("%w: session", store.ErrNotFound)
	}
	return copySession(s), nil
}

func (m *memory) DeleteSession(_ context.Context, tokenHash []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.auth.sessions[string(tokenHash)]; !ok {
		return fmt.Errorf("%w: session", store.ErrNotFound)
	}
	delete(m.auth.sessions, string(tokenHash))
	return nil
}

func (m *memory) CreateDeviceLogin(_ context.Context, d store.DeviceLogin, maxPending int) error {
	d = copyDeviceLogin(d.Clean())
	m.mu.Lock()
	defer m.mu.Unlock()
	pending := 0
	for k, old := range m.auth.devices {
		switch {
		case !old.ExpiresAt.After(d.CreatedAt):
			delete(m.auth.devices, k)
		case old.Status == store.DeviceLoginPending:
			pending++
		}
	}
	if pending >= maxPending {
		return store.ErrDeviceLoginLimit
	}
	for k, old := range m.auth.devices {
		if old.UserCode == d.UserCode || k == string(d.DeviceCodeHash) {
			return fmt.Errorf("%w: device login exists", store.ErrConflict)
		}
	}
	if m.auth.devices == nil {
		m.auth.devices = map[string]store.DeviceLogin{}
	}
	m.auth.devices[string(d.DeviceCodeHash)] = d
	return nil
}

// liveDeviceLogin returns the key and row of the device login with userCode
// whose ExpiresAt is after now. Call with m.mu held.
func (m *memory) liveDeviceLogin(userCode string, now time.Time) (string, store.DeviceLogin, bool) {
	now = pgTime(now)
	for k, d := range m.auth.devices {
		if d.UserCode == userCode && d.ExpiresAt.After(now) {
			return k, d, true
		}
	}
	return "", store.DeviceLogin{}, false
}

func (m *memory) GetDeviceLogin(_ context.Context, userCode string, now time.Time) (store.DeviceLogin, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, d, ok := m.liveDeviceLogin(userCode, now)
	if !ok {
		return store.DeviceLogin{}, fmt.Errorf("%w: device login", store.ErrNotFound)
	}
	return copyDeviceLogin(d), nil
}

func (m *memory) ReviewDeviceLogin(_ context.Context, userCode string, r store.DeviceLoginReview) (store.DeviceLogin, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k, d, ok := m.liveDeviceLogin(userCode, r.At)
	if !ok {
		return store.DeviceLogin{}, fmt.Errorf("%w: device login", store.ErrNotFound)
	}
	if d.Status != store.DeviceLoginPending {
		return store.DeviceLogin{}, fmt.Errorf("%w: device login is %s", store.ErrConflict, d.Status)
	}
	d.Status, d.ReviewedBy, d.TokenRole, d.ReviewedAt = r.Status, r.By, r.TokenRole, &r.At
	if keep := pgTime(r.KeepUntil); keep.After(d.ExpiresAt) {
		d.ExpiresAt = keep
	}
	d = copyDeviceLogin(d.Clean())
	m.auth.devices[k] = d
	return copyDeviceLogin(d), nil
}

func (m *memory) ExchangeDeviceLogin(_ context.Context, deviceCodeHash []byte, s store.Session, now time.Time) (store.DeviceLogin, store.Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := string(deviceCodeHash)
	d, ok := m.auth.devices[k]
	if !ok || !d.ExpiresAt.After(pgTime(now)) {
		return store.DeviceLogin{}, store.Session{}, fmt.Errorf("%w: device login", store.ErrNotFound)
	}
	var out store.Session
	switch d.Status {
	case store.DeviceLoginPending:
		return copyDeviceLogin(d), store.Session{}, nil
	case store.DeviceLoginApproved:
		s.Name, s.Role, s.Client = d.ReviewedBy, d.TokenRole, d.Client
		if _, taken := m.auth.sessions[string(s.TokenHash)]; taken {
			return store.DeviceLogin{}, store.Session{}, fmt.Errorf("%w: session exists", store.ErrConflict)
		}
		m.purgeSessions(s.CreatedAt)
		var err error
		if out, err = m.insertSession(s); err != nil {
			return store.DeviceLogin{}, store.Session{}, err
		}
	}
	delete(m.auth.devices, k)
	return copyDeviceLogin(d), out, nil
}
