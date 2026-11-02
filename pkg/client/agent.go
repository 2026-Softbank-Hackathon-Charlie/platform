package client

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

// CreateAgentSession starts an activity log for one task of the AI agent.
func (c *Client) CreateAgentSession(ctx context.Context, req apiv1.CreateAgentSessionRequest) (*apiv1.AgentSession, error) {
	return call[apiv1.AgentSession](ctx, c, request{method: http.MethodPost, path: "/v1/agent/sessions", body: req})
}

// AppendAgentEvent records one tool call, result, message, blocked action,
// decision or error of a running session.
func (c *Client) AppendAgentEvent(ctx context.Context, sessionID string, req apiv1.AgentEventRequest) (*apiv1.AgentEvent, error) {
	return call[apiv1.AgentEvent](ctx, c, request{method: http.MethodPost, path: "/v1/agent/sessions/%s/events", params: []string{sessionID}, body: req})
}

// FinishAgentSession ends a running session.
func (c *Client) FinishAgentSession(ctx context.Context, sessionID string, req apiv1.FinishAgentSessionRequest) (*apiv1.AgentSession, error) {
	return call[apiv1.AgentSession](ctx, c, request{method: http.MethodPost, path: "/v1/agent/sessions/%s/finish", params: []string{sessionID}, body: req})
}

// AgentSessionFilter selects sessions. Zero values mean no filter; Limit <= 0
// uses the server's default.
type AgentSessionFilter struct {
	PlanRef string
	RunID   string
	Limit   int
}

// ListAgentSessions returns sessions, newest first.
func (c *Client) ListAgentSessions(ctx context.Context, f AgentSessionFilter) (*apiv1.AgentSessionList, error) {
	q := url.Values{}
	if f.PlanRef != "" {
		q.Set("planRef", f.PlanRef)
	}
	if f.RunID != "" {
		q.Set("runId", f.RunID)
	}
	if f.Limit > 0 {
		q.Set("limit", strconv.Itoa(f.Limit))
	}
	return call[apiv1.AgentSessionList](ctx, c, request{method: http.MethodGet, path: "/v1/agent/sessions", query: q})
}

// GetAgentSession returns one session with its totals.
func (c *Client) GetAgentSession(ctx context.Context, sessionID string) (*apiv1.AgentSession, error) {
	return call[apiv1.AgentSession](ctx, c, request{method: http.MethodGet, path: "/v1/agent/sessions/%s", params: []string{sessionID}})
}

// agentEventPage is how many events ListAgentEvents asks for at once: the
// server's maximum page.
const agentEventPage = 2000

// ListAgentEvents returns a session's stored events with seq > after, as one
// list instead of a stream. It reads them a page at a time, following
// nextAfter until the last page.
func (c *Client) ListAgentEvents(ctx context.Context, sessionID string, after int64) (*apiv1.AgentEventList, error) {
	out := &apiv1.AgentEventList{Events: []apiv1.AgentEvent{}}
	for {
		q := url.Values{"format": {"json"}, "limit": {strconv.Itoa(agentEventPage)}}
		if after > 0 {
			q.Set("after", strconv.FormatInt(after, 10))
		}
		page, err := call[apiv1.AgentEventList](ctx, c, request{method: http.MethodGet, path: "/v1/agent/sessions/%s/events", params: []string{sessionID}, query: q})
		if err != nil {
			return nil, err
		}
		out.Events = append(out.Events, page.Events...)
		if page.NextAfter == nil {
			return out, nil
		}
		if *page.NextAfter <= after {
			return nil, &protocolError{fmt.Errorf("client: agent events page after %d points back to %d", after, *page.NextAfter)}
		}
		after = *page.NextAfter
	}
}

// AgentMessage is one message of an agent session's event stream.
type AgentMessage struct {
	// Name is the SSE event name: "snapshot", or the event type.
	Name string
	// Seq is the event seq; 0 for a snapshot.
	Seq int64
	// Snapshot is the session; set when Name is "snapshot".
	Snapshot *apiv1.AgentSession
	// Event is set for every other message.
	Event *apiv1.AgentEvent
}

// StreamAgentEvents reads a session's event stream once and calls fn for each
// message, like StreamEvents does for a run.
func (c *Client) StreamAgentEvents(ctx context.Context, sessionID string, after int64, fn func(AgentMessage) error) error {
	h := http.Header{"Accept": {"text/event-stream"}, "Cache-Control": {"no-cache"}}
	if after > 0 {
		h.Set("Last-Event-ID", strconv.FormatInt(after, 10))
	}
	resp, err := c.send(ctx, request{method: http.MethodGet, path: "/v1/agent/sessions/%s/events", params: []string{sessionID}, header: h})
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	defer resp.Body.Close()
	if mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); mt != "text/event-stream" {
		return &protocolError{fmt.Errorf("client: agent event stream answered with Content-Type %q", resp.Header.Get("Content-Type"))}
	}
	var fnErr error
	err = readSSE(resp.Body, func(ev sseEvent) error {
		m, err := toAgentMessage(ev)
		if err != nil {
			return err
		}
		if err := fn(m); err != nil {
			fnErr = err
			return err
		}
		return nil
	})
	switch {
	case fnErr != nil:
		return fnErr
	case ctx.Err() != nil:
		return ctx.Err()
	case errors.Is(err, bufio.ErrTooLong):
		return &protocolError{fmt.Errorf("client: agent event stream line exceeds %d bytes", maxSSELine)}
	case err != nil:
		var pe *protocolError
		if errors.As(err, &pe) {
			return err
		}
		return fmt.Errorf("client: read agent event stream: %w", err)
	}
	return nil
}

func toAgentMessage(ev sseEvent) (AgentMessage, error) {
	m := AgentMessage{Name: ev.name}
	if ev.name == "snapshot" {
		var s apiv1.AgentSession
		if err := json.Unmarshal([]byte(ev.data), &s); err != nil {
			return m, &protocolError{fmt.Errorf("client: decode agent session snapshot: %w", err)}
		}
		m.Snapshot = &s
		return m, nil
	}
	var e apiv1.AgentEvent
	if err := json.Unmarshal([]byte(ev.data), &e); err != nil {
		return m, &protocolError{fmt.Errorf("client: decode %q agent event: %w", ev.name, err)}
	}
	m.Seq, m.Event = e.Seq, &e
	return m, nil
}

// WatchAgentSession follows a session until it finishes and calls fn for
// each message. It reconnects the way Watch does: with backoff, sending the
// last delivered seq as Last-Event-ID, never delivering a seq twice. It
// returns nil once a snapshot shows the session finished and that stream has
// closed, fn's error as is, and ctx.Err() when ctx is done.
func (c *Client) WatchAgentSession(ctx context.Context, sessionID string, fn func(AgentMessage) error) error {
	var (
		lastSeq   int64
		connected bool
		empty     int
		fnErr     error
	)
	delay := c.retryMin
	for {
		progressed, ended := false, false
		err := c.StreamAgentEvents(ctx, sessionID, lastSeq, func(m AgentMessage) error {
			progressed, connected = true, true
			if m.Event != nil && m.Seq > 0 {
				if m.Seq <= lastSeq {
					return nil // replayed after a reconnect
				}
				lastSeq = m.Seq
			}
			if m.Snapshot != nil {
				ended = m.Snapshot.Status != apiv1.AgentSessionStatusRunning
			}
			if err := fn(m); err != nil {
				fnErr = err
				return err
			}
			return nil
		})
		switch {
		case fnErr != nil:
			return fnErr
		case ctx.Err() != nil:
			return ctx.Err()
		case err == nil && ended:
			return nil
		case err != nil && (!connected || !retryable(err)):
			return err
		case err == nil && !progressed:
			if !connected {
				return fmt.Errorf("client: event stream of agent session %s closed before its first message", sessionID)
			}
			if empty++; empty >= maxEmptyStreams {
				return fmt.Errorf("client: event stream of agent session %s closed without a message %d times in a row", sessionID, empty)
			}
		}
		if progressed {
			delay, empty = c.retryMin, 0
		}
		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
		if !progressed {
			delay = min(2*delay, c.retryMax)
		}
	}
}
