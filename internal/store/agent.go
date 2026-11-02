package store

import (
	"context"
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

// AgentSessionFilter selects sessions. Empty fields match everything.
type AgentSessionFilter struct {
	PlanRef string
	RunID   string
	Limit   int // <= 0 means no limit
}

// AgentStore persists agent sessions and their events.
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
}
