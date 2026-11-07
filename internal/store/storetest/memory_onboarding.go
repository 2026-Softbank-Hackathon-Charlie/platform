package storetest

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
	"slices"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
)

// onboardMemory holds onboardings and their events. Its zero value is
// ready. The store's mutex guards it, and it numbers events with its own
// sequence, like the separate bigserial in Postgres.
type onboardMemory struct {
	rows   map[string]store.Onboarding
	events []store.OnboardingEvent // in Seq order
	seq    int64
}

func copyOnboarding(o store.Onboarding) store.Onboarding {
	o, _ = o.Clean() // a stored row is clean; Clean copies the byte slices
	return o
}

// activeOnboarding returns the id of an onboarding of app that is not done,
// other than except. The caller holds the mutex.
func (m *onboardMemory) activeOnboarding(app, except string) string {
	for id, o := range m.rows {
		if id != except && o.App == app && o.State != store.OnboardingDone {
			return id
		}
	}
	return ""
}

func checkOnboarding(o store.Onboarding) error {
	if o.ID == "" || o.App == "" || o.Repo == "" || o.State == "" {
		return fmt.Errorf("storetest: onboarding %q needs an id, app, repo and state", o.ID)
	}
	if o.Doc == nil {
		return fmt.Errorf("storetest: onboarding %s has no doc", o.ID)
	}
	return nil
}

func (m *memory) CreateOnboarding(_ context.Context, o store.Onboarding) error {
	o, err := o.Clean()
	if err != nil {
		return err
	}
	if err := checkOnboarding(o); err != nil {
		return err
	}
	o.CreatedAt, o.UpdatedAt = pgTime(o.CreatedAt), pgTime(o.UpdatedAt)
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.onboard.rows[o.ID]; ok {
		return fmt.Errorf("%w: onboarding %s exists", store.ErrConflict, o.ID)
	}
	if o.State != store.OnboardingDone && m.onboard.activeOnboarding(o.App, "") != "" {
		return fmt.Errorf("%w: app %s has an onboarding that is not done", store.ErrConflict, o.App)
	}
	if m.onboard.rows == nil {
		m.onboard.rows = map[string]store.Onboarding{}
	}
	m.onboard.rows[o.ID] = o
	return nil
}

func (m *memory) GetOnboarding(_ context.Context, id string) (store.Onboarding, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.onboard.rows[id]
	if !ok {
		return store.Onboarding{}, fmt.Errorf("%w: onboarding %s", store.ErrNotFound, id)
	}
	return copyOnboarding(o), nil
}

func (m *memory) ListOnboardings(_ context.Context) ([]store.Onboarding, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []store.Onboarding{}
	for _, o := range m.onboard.rows {
		out = append(out, copyOnboarding(o))
	}
	slices.SortFunc(out, func(a, b store.Onboarding) int {
		if c := b.CreatedAt.Compare(a.CreatedAt); c != 0 {
			return c
		}
		return cmp.Compare(b.ID, a.ID)
	})
	return out, nil
}

func (m *memory) UpdateOnboarding(_ context.Context, o store.Onboarding, from []string) (store.Onboarding, error) {
	o, err := o.Clean()
	if err != nil {
		return store.Onboarding{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.onboard.rows[o.ID]
	if !ok {
		return store.Onboarding{}, fmt.Errorf("%w: onboarding %s", store.ErrNotFound, o.ID)
	}
	if !slices.Contains(from, cur.State) {
		return store.Onboarding{}, fmt.Errorf("%w: onboarding %s is %s", store.ErrConflict, o.ID, cur.State)
	}
	if o.State == "" || o.Doc == nil {
		return store.Onboarding{}, fmt.Errorf("storetest: onboarding %s needs a state and a doc", o.ID)
	}
	if o.State != store.OnboardingDone && m.onboard.activeOnboarding(cur.App, cur.ID) != "" {
		return store.Onboarding{}, fmt.Errorf("%w: app %s has another onboarding that is not done", store.ErrConflict, cur.App)
	}
	cur.State, cur.Doc, cur.Facts = o.State, o.Doc, o.Facts
	cur.KeyVersion, cur.KeyCheck = o.KeyVersion, o.KeyCheck
	cur.SecretsNonce, cur.SecretsCiphertext = o.SecretsNonce, o.SecretsCiphertext
	cur.SuperuserPasswordNonce, cur.SuperuserPasswordCiphertext = o.SuperuserPasswordNonce, o.SuperuserPasswordCiphertext
	cur.ReplicationPasswordNonce, cur.ReplicationPasswordCiphertext = o.ReplicationPasswordNonce, o.ReplicationPasswordCiphertext
	cur.UpdatedAt = pgTime(o.UpdatedAt)
	m.onboard.rows[o.ID] = cur
	return copyOnboarding(cur), nil
}

func (m *memory) DeleteOnboarding(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.onboard.rows[id]; !ok {
		return fmt.Errorf("%w: onboarding %s", store.ErrNotFound, id)
	}
	delete(m.onboard.rows, id)
	m.onboard.events = slices.DeleteFunc(m.onboard.events, func(e store.OnboardingEvent) bool { return e.OnboardingID == id })
	return nil
}

func (m *memory) AppendOnboardingEvent(_ context.Context, e store.OnboardingEvent) (store.OnboardingEvent, error) {
	if err := checkJSON("onboarding event data", e.Data); err != nil {
		return store.OnboardingEvent{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.onboard.rows[e.OnboardingID]; !ok {
		return store.OnboardingEvent{}, fmt.Errorf("%w: onboarding %s", store.ErrNotFound, e.OnboardingID)
	}
	m.onboard.seq++
	e.Seq, e.Time = m.onboard.seq, pgTime(e.Time)
	e.State, e.Kind, e.Message = store.CleanText(e.State), store.CleanText(e.Kind), store.CleanText(e.Message)
	e.Data = bytes.Clone(e.Data)
	m.onboard.events = append(m.onboard.events, e)
	out := e
	out.Data = bytes.Clone(e.Data)
	return out, nil
}

func (m *memory) ListOnboardingEvents(_ context.Context, id string, afterSeq int64, limit int) ([]store.OnboardingEvent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []store.OnboardingEvent{}
	for _, e := range m.onboard.events {
		if e.OnboardingID != id || e.Seq <= afterSeq {
			continue
		}
		if limit > 0 && len(out) == limit {
			break
		}
		e.Data = bytes.Clone(e.Data)
		out = append(out, e)
	}
	return out, nil
}
