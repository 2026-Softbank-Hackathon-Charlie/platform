package storetest

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
	"slices"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
)

type secretKey struct{ app, instance, name string }

type storageKey struct{ instance, env string }

// secretsMemory holds app secrets, instance storage rows and backup storage
// rows (by app and environment). Its zero value is ready. The store's mutex
// guards it.
type secretsMemory struct {
	secrets map[secretKey]store.AppSecret
	storage map[storageKey]store.InstanceStorage
	backup  map[storageKey]store.InstanceStorage
}

func copyAppSecret(s store.AppSecret) store.AppSecret {
	s.Nonce, s.Ciphertext = bytes.Clone(s.Nonce), bytes.Clone(s.Ciphertext)
	return s
}

// deleteSecrets deletes the secrets gone reports. The caller holds the
// mutex.
func (m *secretsMemory) deleteSecrets(gone func(secretKey) bool) {
	for k := range m.secrets {
		if gone(k) {
			delete(m.secrets, k)
		}
	}
}

func (m *memory) ListAppSecrets(_ context.Context, app string) ([]store.AppSecret, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []store.AppSecret{}
	for k, s := range m.secrets.secrets {
		if k.app == app {
			out = append(out, copyAppSecret(s))
		}
	}
	slices.SortFunc(out, func(a, b store.AppSecret) int {
		return cmp.Or(cmp.Compare(a.Instance, b.Instance), cmp.Compare(a.Name, b.Name))
	})
	return out, nil
}

func (m *memory) GetAppSecret(_ context.Context, app, instance, name string) (store.AppSecret, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.secrets.secrets[secretKey{app, instance, name}]
	if !ok {
		return store.AppSecret{}, fmt.Errorf("%w: secret %s", store.ErrNotFound, name)
	}
	return copyAppSecret(s), nil
}

func (m *memory) ChangeAppSecrets(_ context.Context, app, instance string, set []store.AppSecret, del []string, max int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.deploy.apps[app]; !ok {
		return fmt.Errorf("%w: app %s", store.ErrNotFound, app)
	}
	if instance != "" && instance != app {
		found := false
		for _, b := range m.deploy.branches[app] {
			found = found || b.Instance == instance
		}
		if !found {
			return fmt.Errorf("%w: app %s has no instance %s", store.ErrInstanceNotFound, app, instance)
		}
	}
	// Work on a copy of the scope, so a refused change leaves nothing.
	scope := map[string]store.AppSecret{}
	for k, s := range m.secrets.secrets {
		if k.app == app && k.instance == instance {
			scope[k.name] = s
		}
	}
	before := len(scope)
	for _, n := range del {
		delete(scope, n)
	}
	for _, r := range set {
		r = r.Clean()
		r.App, r.Instance = app, instance
		r.UpdatedAt = pgTime(r.UpdatedAt)
		scope[r.Name] = r
	}
	if len(scope) > max && len(scope) > before {
		return fmt.Errorf("%w: the change leaves %d secrets, at most %d are allowed", store.ErrSecretLimit, len(scope), max)
	}
	m.secrets.deleteSecrets(func(k secretKey) bool { return k.app == app && k.instance == instance })
	if m.secrets.secrets == nil {
		m.secrets.secrets = map[secretKey]store.AppSecret{}
	}
	for n, s := range scope {
		m.secrets.secrets[secretKey{app, instance, n}] = s
	}
	return nil
}

func (m *memory) PutInstanceStorage(_ context.Context, s store.InstanceStorage) error {
	s = s.Clean()
	s.CreatedAt = pgTime(s.CreatedAt)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.secrets.storage == nil {
		m.secrets.storage = map[storageKey]store.InstanceStorage{}
	}
	k := storageKey{s.Instance, s.Env}
	if old, ok := m.secrets.storage[k]; ok {
		s.CreatedAt = old.CreatedAt
	}
	m.secrets.storage[k] = s
	return nil
}

func (m *memory) GetInstanceStorage(_ context.Context, instance, env string) (store.InstanceStorage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.secrets.storage[storageKey{instance, env}]
	if !ok {
		return store.InstanceStorage{}, fmt.Errorf("%w: storage of instance %s in %s", store.ErrNotFound, instance, env)
	}
	return s, nil
}

func (m *memory) ListInstanceStorage(_ context.Context, app, instance string) ([]store.InstanceStorage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []store.InstanceStorage{}
	for _, s := range m.secrets.storage {
		if (app == "" || s.App == app) && (instance == "" || s.Instance == instance) {
			out = append(out, s)
		}
	}
	slices.SortFunc(out, func(a, b store.InstanceStorage) int {
		return cmp.Or(cmp.Compare(a.Instance, b.Instance), cmp.Compare(a.Env, b.Env))
	})
	return out, nil
}

func (m *memory) DeleteInstanceStorage(_ context.Context, instance, env string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := storageKey{instance, env}
	if _, ok := m.secrets.storage[k]; !ok {
		return fmt.Errorf("%w: storage of instance %s in %s", store.ErrNotFound, instance, env)
	}
	delete(m.secrets.storage, k)
	return nil
}

func (m *memory) PutBackupStorage(_ context.Context, s store.InstanceStorage) error {
	s = s.Clean()
	s.CreatedAt = pgTime(s.CreatedAt)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.secrets.backup == nil {
		m.secrets.backup = map[storageKey]store.InstanceStorage{}
	}
	k := storageKey{s.App, s.Env}
	if old, ok := m.secrets.backup[k]; ok {
		s.CreatedAt = old.CreatedAt
	}
	m.secrets.backup[k] = s
	return nil
}

func (m *memory) GetBackupStorage(_ context.Context, app, env string) (store.InstanceStorage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.secrets.backup[storageKey{app, env}]
	if !ok {
		return store.InstanceStorage{}, fmt.Errorf("%w: backup storage of app %s in %s", store.ErrNotFound, app, env)
	}
	return s, nil
}

func (m *memory) ListBackupStorage(_ context.Context) ([]store.InstanceStorage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []store.InstanceStorage{}
	for _, s := range m.secrets.backup {
		out = append(out, s)
	}
	slices.SortFunc(out, func(a, b store.InstanceStorage) int {
		return cmp.Or(cmp.Compare(a.App, b.App), cmp.Compare(a.Env, b.Env))
	})
	return out, nil
}

func (m *memory) DeleteBackupStorage(_ context.Context, app, env string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := storageKey{app, env}
	if _, ok := m.secrets.backup[k]; !ok {
		return fmt.Errorf("%w: backup storage of app %s in %s", store.ErrNotFound, app, env)
	}
	delete(m.secrets.backup, k)
	return nil
}
