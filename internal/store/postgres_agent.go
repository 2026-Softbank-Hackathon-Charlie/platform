package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// The totals are aggregated per session on read. sum(bigint) is numeric in
// PostgreSQL, so the sums are cast back.
const (
	agentSessionColumns = `s.id, s.kind, s.title, s.actor, s.plan_ref, s.run_id, s.model, s.status, s.summary,
	s.created_at, s.ended_at,
	COALESCE(t.events, 0), COALESCE(t.tool_calls, 0), COALESCE(t.input_tokens, 0),
	COALESCE(t.output_tokens, 0), COALESCE(t.duration_ms, 0)`
	agentTotalsJoin = `LEFT JOIN LATERAL (
		SELECT count(*) AS events,
			count(*) FILTER (WHERE e.type = 'tool_call') AS tool_calls,
			sum(e.input_tokens)::bigint AS input_tokens,
			sum(e.output_tokens)::bigint AS output_tokens,
			sum(e.duration_ms)::bigint AS duration_ms
		FROM agent_events e WHERE e.session_id = s.id
	) t ON true`
)

func scanAgentSession(row pgx.Row) (AgentSession, error) {
	var a AgentSession
	err := row.Scan(&a.ID, &a.Kind, &a.Title, &a.Actor, &a.PlanRef, &a.RunID, &a.Model, &a.Status, &a.Summary,
		&a.CreatedAt, &a.EndedAt,
		&a.Totals.Events, &a.Totals.ToolCalls, &a.Totals.InputTokens, &a.Totals.OutputTokens, &a.Totals.DurationMillis)
	if err != nil {
		return AgentSession{}, err
	}
	a.CreatedAt = a.CreatedAt.UTC()
	if a.EndedAt != nil {
		t := a.EndedAt.UTC()
		a.EndedAt = &t
	}
	return a, nil
}

func (s *Postgres) CreateAgentSession(ctx context.Context, a AgentSession) error {
	a = a.Clean()
	_, err := s.pool.Exec(ctx, `INSERT INTO agent_sessions
		(id, kind, title, actor, plan_ref, run_id, model, status, summary, created_at, ended_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		a.ID, a.Kind, a.Title, a.Actor, a.PlanRef, a.RunID, a.Model, a.Status, a.Summary, a.CreatedAt, a.EndedAt)
	if isUniqueViolation(err) {
		return fmt.Errorf("%w: agent session %s exists", ErrConflict, a.ID)
	}
	return err
}

func (s *Postgres) GetAgentSession(ctx context.Context, id string) (AgentSession, error) {
	if !Clean(id) { // PostgreSQL would reject the parameter
		return AgentSession{}, fmt.Errorf("%w: agent session %q", ErrNotFound, id)
	}
	a, err := scanAgentSession(s.pool.QueryRow(ctx, `SELECT `+agentSessionColumns+`
		FROM agent_sessions s `+agentTotalsJoin+` WHERE s.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return AgentSession{}, fmt.Errorf("%w: agent session %s", ErrNotFound, id)
	}
	return a, err
}

func (s *Postgres) ListAgentSessions(ctx context.Context, f AgentSessionFilter) ([]AgentSession, error) {
	if !Clean(f.PlanRef) || !Clean(f.RunID) {
		return []AgentSession{}, nil
	}
	var lim *int // LIMIT NULL means no limit
	if f.Limit > 0 {
		lim = &f.Limit
	}
	rows, err := s.pool.Query(ctx, `SELECT `+agentSessionColumns+`
		FROM agent_sessions s `+agentTotalsJoin+`
		WHERE ($1 = '' OR s.plan_ref = $1) AND ($2 = '' OR s.run_id = $2)
		ORDER BY s.created_at DESC, s.id DESC LIMIT $3`, f.PlanRef, f.RunID, lim)
	if err != nil {
		return nil, err
	}
	return collect(rows, scanAgentSession)
}

func (s *Postgres) FinishAgentSession(ctx context.Context, id, status, summary string, endedAt time.Time) (AgentSession, error) {
	if !Clean(id) {
		return AgentSession{}, fmt.Errorf("%w: agent session %q", ErrNotFound, id)
	}
	status, summary = CleanText(status), CleanText(summary)
	// The row lock of this UPDATE waits for an append that holds the session
	// row, and an append after it sees the new status.
	tag, err := s.pool.Exec(ctx, `UPDATE agent_sessions SET status = $2, summary = $3, ended_at = $4
		WHERE id = $1 AND status = $5`, id, status, summary, endedAt, AgentRunning)
	if err != nil {
		return AgentSession{}, err
	}
	if tag.RowsAffected() == 0 {
		var current string
		err := s.pool.QueryRow(ctx, `SELECT status FROM agent_sessions WHERE id = $1`, id).Scan(&current)
		if errors.Is(err, pgx.ErrNoRows) {
			return AgentSession{}, fmt.Errorf("%w: agent session %s", ErrNotFound, id)
		}
		if err != nil {
			return AgentSession{}, err
		}
		return AgentSession{}, fmt.Errorf("%w: agent session is %s", ErrConflict, current)
	}
	return s.GetAgentSession(ctx, id)
}

const agentEventColumns = `seq, session_id, time, type, tool, input, output, status, duration_ms, input_tokens, output_tokens`

func scanAgentEvent(row pgx.Row) (AgentEvent, error) {
	var e AgentEvent
	err := row.Scan(&e.Seq, &e.SessionID, &e.Time, &e.Type, &e.Tool, &e.Input, &e.Output, &e.Status,
		&e.DurationMillis, &e.InputTokens, &e.OutputTokens)
	if err != nil {
		return AgentEvent{}, err
	}
	e.Time = e.Time.UTC()
	return e, nil
}

// AppendAgentEvent locks the session row for the insert. Concurrent appends
// to one session therefore commit one at a time in Seq order (a reader
// resuming with afterSeq cannot skip one), and a finish waits for them.
func (s *Postgres) AppendAgentEvent(ctx context.Context, e AgentEvent) (AgentEvent, error) {
	if !Clean(e.SessionID) {
		return AgentEvent{}, fmt.Errorf("%w: agent session %q", ErrNotFound, e.SessionID)
	}
	e, err := e.Clean()
	if err != nil {
		return AgentEvent{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return AgentEvent{}, err
	}
	defer tx.Rollback(ctx) // no-op after Commit

	var status string
	err = tx.QueryRow(ctx, `SELECT status FROM agent_sessions WHERE id = $1 FOR UPDATE`, e.SessionID).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return AgentEvent{}, fmt.Errorf("%w: agent session %s", ErrNotFound, e.SessionID)
	}
	if err != nil {
		return AgentEvent{}, err
	}
	if status != AgentRunning {
		return AgentEvent{}, fmt.Errorf("%w: agent session is %s", ErrConflict, status)
	}
	out, err := scanAgentEvent(tx.QueryRow(ctx, `INSERT INTO agent_events
		(session_id, time, type, tool, input, output, status, duration_ms, input_tokens, output_tokens)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10) RETURNING `+agentEventColumns,
		e.SessionID, e.Time, e.Type, e.Tool, e.Input, e.Output, e.Status, e.DurationMillis, e.InputTokens, e.OutputTokens))
	if err != nil {
		return AgentEvent{}, err
	}
	return out, tx.Commit(ctx)
}

func (s *Postgres) ListAgentEvents(ctx context.Context, sessionID string, afterSeq int64, limit int) ([]AgentEvent, error) {
	if !Clean(sessionID) {
		return []AgentEvent{}, nil
	}
	var lim *int
	if limit > 0 {
		lim = &limit
	}
	rows, err := s.pool.Query(ctx, `SELECT `+agentEventColumns+` FROM agent_events
		WHERE session_id = $1 AND seq > $2 ORDER BY seq LIMIT $3`, sessionID, afterSeq, lim)
	if err != nil {
		return nil, err
	}
	return collect(rows, scanAgentEvent)
}

func (s *Postgres) IdleAgentSessions(ctx context.Context, before time.Time) ([]AgentSession, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+agentSessionColumns+`
		FROM agent_sessions s `+agentTotalsJoin+`
		WHERE s.status = $1 AND s.created_at < $2
			AND NOT EXISTS (SELECT 1 FROM agent_events e WHERE e.session_id = s.id AND e.time >= $2)
		ORDER BY s.created_at, s.id`, AgentRunning, before)
	if err != nil {
		return nil, err
	}
	return collect(rows, scanAgentSession)
}
