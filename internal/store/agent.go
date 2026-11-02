package store

import (
	"context"
	"fmt"
	"time"
)

// Agent session statuses, mirrored from apiv1.AgentSessionStatus.
const (
	AgentRunning   = "running"
	AgentSucceeded = "succeeded"
	AgentFailed    = "failed"
	AgentCancelled = "cancelled"
)

// AgentSession is one task of the AI agent: planning a migration, a sandbox
// rehearsal, a diagnosis. Its events are the agent's tool calls and results.
type AgentSession struct {
	ID      string
	Kind    string
	Title   string
	Actor   string // the token name that created the session
	PlanRef string
	RunID   string
	Model   string
	Status  string
	Summary string

	CreatedAt time.Time
	EndedAt   *time.Time // nil while running

	// Totals are computed from the session's events on read; writes ignore them.
	Totals AgentTotals
}

// AgentTotals sums a session's events.
type AgentTotals struct {
	Events         int64
	ToolCalls      int64
	InputTokens    int64
	OutputTokens   int64
	DurationMillis int64
}

// AgentEvent is one stored agent event.
type AgentEvent struct {
	// Seq is assigned by AppendAgentEvent. It increases across all sessions,
	// so it is also increasing within one session and usable as an SSE id.
	Seq       int64
	SessionID string
	Time      time.Time
	Type      string
	Tool      string
	Input     []byte // JSON value, or nil
	Output    []byte // JSON value, or nil
	Status    string
	// DurationMillis is nil when the agent did not report one.
	DurationMillis *int64
	InputTokens    int64
	OutputTokens   int64
}

// Clean returns a with every string passed through CleanText. Stores apply
// it on every write, so all of them keep the same text.
func (a AgentSession) Clean() AgentSession {
	for _, f := range []*string{&a.ID, &a.Kind, &a.Title, &a.Actor, &a.PlanRef, &a.RunID, &a.Model, &a.Status, &a.Summary} {
		*f = CleanText(*f)
	}
	return a
}

// Clean returns e with its strings passed through CleanText and Input and
// Output through CleanJSON. Stores apply it on every append. SessionID is a
// reference and stays as it is: an id that is not clean names no session.
func (e AgentEvent) Clean() (AgentEvent, error) {
	for _, f := range []*string{&e.Type, &e.Tool, &e.Status} {
		*f = CleanText(*f)
	}
	var err error
	if e.Input, err = CleanJSON(e.Input); err != nil {
		return AgentEvent{}, fmt.Errorf("agent event input: %w", err)
	}
	if e.Output, err = CleanJSON(e.Output); err != nil {
		return AgentEvent{}, fmt.Errorf("agent event output: %w", err)
	}
	return e, nil
}

// AgentSessionFilter selects sessions. Empty fields match everything.
type AgentSessionFilter struct {
	PlanRef string
	RunID   string
	Limit   int // <= 0 means no limit
}

// AgentStore persists agent sessions and their events. Every string is
// stored as Clean leaves it: without NUL and with invalid UTF-8 replaced.
type AgentStore interface {
	// CreateAgentSession inserts a session. ErrConflict if the id exists.
	CreateAgentSession(ctx context.Context, s AgentSession) error
	// GetAgentSession returns the session with its totals, or ErrNotFound.
	GetAgentSession(ctx context.Context, id string) (AgentSession, error)
	// ListAgentSessions returns matching sessions with their totals, newest
	// CreatedAt first.
	ListAgentSessions(ctx context.Context, f AgentSessionFilter) ([]AgentSession, error)
	// FinishAgentSession sets the final status, summary and end time of a
	// running session and returns it. ErrNotFound when the id does not exist;
	// ErrConflict (wrapped, with the current status) when it is not running.
	FinishAgentSession(ctx context.Context, id, status, summary string, endedAt time.Time) (AgentSession, error)
	// AppendAgentEvent stores an event of a running session and returns it
	// with Seq set. ErrNotFound when the session does not exist; ErrConflict
	// when it is not running. Appends and finishes of one session are
	// serialized, so no event is stored after the session finished and
	// events commit in Seq order within a session.
	AppendAgentEvent(ctx context.Context, e AgentEvent) (AgentEvent, error)
	// ListAgentEvents returns a session's events with Seq > afterSeq in Seq
	// order, at most limit (<= 0 means no limit).
	ListAgentEvents(ctx context.Context, sessionID string, afterSeq int64, limit int) ([]AgentEvent, error)
	// IdleAgentSessions returns the running sessions created before before
	// whose events (if any) are all older than before, oldest CreatedAt first.
	IdleAgentSessions(ctx context.Context, before time.Time) ([]AgentSession, error)
}
