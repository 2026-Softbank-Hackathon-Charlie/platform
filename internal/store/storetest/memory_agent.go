package storetest

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
)

// agentMemory holds agent sessions and events. Its zero value is ready. The
// store's mutex guards it, and it numbers events with its own sequence, like
// the separate bigserial in Postgres.
type agentMemory struct {
	sessions map[string]store.AgentSession
	events   []store.AgentEvent // in Seq order
	seq      int64
}

func copyAgentSession(a store.AgentSession) store.AgentSession {
	a.EndedAt = pgTimePtr(a.EndedAt)
	a.Totals = store.AgentTotals{}
	return a
}

func copyAgentEvent(e store.AgentEvent) store.AgentEvent {
	e.Input, e.Output = bytes.Clone(e.Input), bytes.Clone(e.Output)
	if e.DurationMillis != nil {
		d := *e.DurationMillis
		e.DurationMillis = &d
	}
	return e
}

// withTotals returns a copy of a with its totals computed. Call with m.mu held.
func (m *memory) withTotals(a store.AgentSession) store.AgentSession {
	a = copyAgentSession(a)
	for _, e := range m.agent.events {
		if e.SessionID != a.ID {
			continue
		}
		a.Totals.Events++
		if e.Type == "tool_call" {
			a.Totals.ToolCalls++
		}
		a.Totals.InputTokens += e.InputTokens
		a.Totals.OutputTokens += e.OutputTokens
		if e.DurationMillis != nil {
			a.Totals.DurationMillis += *e.DurationMillis
		}
	}
	return a
}

func (m *memory) CreateAgentSession(_ context.Context, a store.AgentSession) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.agent.sessions[a.ID]; ok {
		return fmt.Errorf("%w: agent session %s exists", store.ErrConflict, a.ID)
	}
	if m.agent.sessions == nil {
		m.agent.sessions = map[string]store.AgentSession{}
	}
	a = copyAgentSession(a.Clean())
	a.CreatedAt = pgTime(a.CreatedAt)
	m.agent.sessions[a.ID] = a
	return nil
}

func (m *memory) GetAgentSession(_ context.Context, id string) (store.AgentSession, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.agent.sessions[id]
	if !ok {
		return store.AgentSession{}, fmt.Errorf("%w: agent session %s", store.ErrNotFound, id)
	}
	return m.withTotals(a), nil
}

func (m *memory) ListAgentSessions(_ context.Context, f store.AgentSessionFilter) ([]store.AgentSession, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []store.AgentSession{}
	for _, a := range m.agent.sessions {
		if (f.PlanRef == "" || a.PlanRef == f.PlanRef) && (f.RunID == "" || a.RunID == f.RunID) {
			out = append(out, m.withTotals(a))
		}
	}
	// Same order as Postgres: created_at DESC, id DESC.
	slices.SortFunc(out, func(a, b store.AgentSession) int {
		return cmp.Or(b.CreatedAt.Compare(a.CreatedAt), cmp.Compare(b.ID, a.ID))
	})
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return out, nil
}

func (m *memory) FinishAgentSession(_ context.Context, id, status, summary string, endedAt time.Time) (store.AgentSession, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.agent.sessions[id]
	if !ok {
		return store.AgentSession{}, fmt.Errorf("%w: agent session %s", store.ErrNotFound, id)
	}
	if a.Status != store.AgentRunning {
		return store.AgentSession{}, fmt.Errorf("%w: agent session is %s", store.ErrConflict, a.Status)
	}
	a.Status, a.Summary, a.EndedAt = store.CleanText(status), store.CleanText(summary), pgTimePtr(&endedAt)
	m.agent.sessions[id] = a
	return m.withTotals(a), nil
}

func (m *memory) AppendAgentEvent(_ context.Context, e store.AgentEvent) (store.AgentEvent, error) {
	e, err := e.Clean()
	if err != nil {
		return store.AgentEvent{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.agent.sessions[e.SessionID]
	if !ok {
		return store.AgentEvent{}, fmt.Errorf("%w: agent session %s", store.ErrNotFound, e.SessionID)
	}
	if a.Status != store.AgentRunning {
		return store.AgentEvent{}, fmt.Errorf("%w: agent session is %s", store.ErrConflict, a.Status)
	}
	m.agent.seq++
	e = copyAgentEvent(e)
	e.Seq, e.Time = m.agent.seq, pgTime(e.Time)
	m.agent.events = append(m.agent.events, e)
	return copyAgentEvent(e), nil
}

func (m *memory) ListAgentEvents(_ context.Context, sessionID string, afterSeq int64, limit int) ([]store.AgentEvent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []store.AgentEvent{}
	for _, e := range m.agent.events {
		if limit > 0 && len(out) == limit {
			break
		}
		if e.SessionID == sessionID && e.Seq > afterSeq {
			out = append(out, copyAgentEvent(e))
		}
	}
	return out, nil
}

func (m *memory) IdleAgentSessions(_ context.Context, before time.Time) ([]store.AgentSession, error) {
	before = pgTime(before)
	m.mu.Lock()
	defer m.mu.Unlock()
	last := map[string]time.Time{}
	for _, e := range m.agent.events {
		if e.Time.After(last[e.SessionID]) {
			last[e.SessionID] = e.Time
		}
	}
	out := []store.AgentSession{}
	for _, a := range m.agent.sessions {
		if a.Status == store.AgentRunning && a.CreatedAt.Before(before) && last[a.ID].Before(before) {
			out = append(out, m.withTotals(a))
		}
	}
	// Same order as Postgres: created_at, id.
	slices.SortFunc(out, func(a, b store.AgentSession) int {
		return cmp.Or(a.CreatedAt.Compare(b.CreatedAt), cmp.Compare(a.ID, b.ID))
	})
	return out, nil
}
