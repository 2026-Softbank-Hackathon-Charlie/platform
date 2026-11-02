package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/agentlog"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/auth"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

// AgentLog is the AI agent's activity log (agentlog.Service).
type AgentLog interface {
	Create(ctx context.Context, actor string, req apiv1.CreateAgentSessionRequest) (apiv1.AgentSession, error)
	Get(ctx context.Context, id string) (apiv1.AgentSession, error)
	List(ctx context.Context, planRef, runID string, limit int) ([]apiv1.AgentSession, error)
	Events(ctx context.Context, id string, after int64) ([]apiv1.AgentEvent, error)
	Append(ctx context.Context, p auth.Principal, id string, in agentlog.EventInput) (apiv1.AgentEvent, error)
	Finish(ctx context.Context, p auth.Principal, id string, req apiv1.FinishAgentSessionRequest) (apiv1.AgentSession, error)
	Subscribe(ctx context.Context, id string) (<-chan apiv1.AgentEvent, func(), error)
}

// agentsReady answers 503 when the server has no agent log.
func (s *Server) agentsReady(w http.ResponseWriter, r *http.Request) bool {
	if s.d.Agents == nil {
		s.fail(w, r, http.StatusServiceUnavailable, "agent_log_not_configured", "this server has no agent activity log")
		return false
	}
	return true
}

func (s *Server) agentError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, agentlog.ErrNotFound):
		s.fail(w, r, http.StatusNotFound, "agent_session_not_found", "agent session not found")
	case errors.Is(err, agentlog.ErrFinished):
		s.fail(w, r, http.StatusConflict, "agent_session_finished", "the agent session is finished and takes no more events")
	case errors.Is(err, agentlog.ErrForbidden):
		s.fail(w, r, http.StatusForbidden, "forbidden", err.Error())
	case errors.Is(err, agentlog.ErrTooLarge):
		s.fail(w, r, http.StatusRequestEntityTooLarge, "too_large", err.Error())
	case errors.Is(err, agentlog.ErrInvalid):
		s.fail(w, r, http.StatusBadRequest, "invalid_request", err.Error())
	default:
		s.internal(w, r, err)
	}
}

// POST /v1/agent/sessions
func (s *Server) createAgentSession(w http.ResponseWriter, r *http.Request) {
	if !s.agentsReady(w, r) {
		return
	}
	var req apiv1.CreateAgentSessionRequest
	if !s.decode(w, r, &req, true) {
		return
	}
	a, err := s.d.Agents.Create(r.Context(), actor(r), req)
	if err != nil {
		s.agentError(w, r, err)
		return
	}
	noteAudit(r, "sessionId", a.Id)
	noteAudit(r, "kind", a.Kind)
	if a.PlanRef != nil {
		noteAudit(r, "planRef", *a.PlanRef)
	}
	if a.RunId != nil {
		noteAudit(r, "runId", *a.RunId)
	}
	w.Header().Set("Location", "/v1/agent/sessions/"+a.Id)
	writeJSON(w, http.StatusCreated, a)
}

// GET /v1/agent/sessions?planRef=&runId=&limit=
func (s *Server) listAgentSessions(w http.ResponseWriter, r *http.Request) {
	if !s.agentsReady(w, r) {
		return
	}
	q := r.URL.Query()
	limit := 50
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 200 {
			s.fail(w, r, http.StatusBadRequest, "invalid_request", "limit must be between 1 and 200")
			return
		}
		limit = n
	}
	list, err := s.d.Agents.List(r.Context(), q.Get("planRef"), q.Get("runId"), limit)
	if err != nil {
		s.agentError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, apiv1.AgentSessionList{Sessions: list})
}

func (s *Server) getAgentSession(w http.ResponseWriter, r *http.Request) {
	if !s.agentsReady(w, r) {
		return
	}
	a, err := s.d.Agents.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		s.agentError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, a)
}

// POST /v1/agent/sessions/{id}/events: one tool call, result or message.
// The input and output are redacted and stored, never copied into the audit log.
func (s *Server) appendAgentEvent(w http.ResponseWriter, r *http.Request) {
	if !s.agentsReady(w, r) {
		return
	}
	id := r.PathValue("id")
	noteAudit(r, "sessionId", id)
	var in agentlog.EventInput
	if !s.decode(w, r, &in, true) {
		return
	}
	noteAudit(r, "type", in.Type)
	if in.Tool != nil {
		noteAudit(r, "tool", *in.Tool)
	}
	p, _ := auth.FromContext(r.Context())
	ev, err := s.d.Agents.Append(r.Context(), p, id, in)
	if err != nil {
		s.agentError(w, r, err)
		return
	}
	noteAudit(r, "seq", ev.Seq)
	writeJSON(w, http.StatusCreated, ev)
}

func (s *Server) finishAgentSession(w http.ResponseWriter, r *http.Request) {
	if !s.agentsReady(w, r) {
		return
	}
	id := r.PathValue("id")
	noteAudit(r, "sessionId", id)
	var req apiv1.FinishAgentSessionRequest
	if !s.decode(w, r, &req, true) {
		return
	}
	noteAudit(r, "status", req.Status)
	p, _ := auth.FromContext(r.Context())
	a, err := s.d.Agents.Finish(r.Context(), p, id, req)
	if err != nil {
		s.agentError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, a)
}

// GET /v1/agent/sessions/{id}/events
//
// The stream contract is the run events' one (see migrationEvents): a
// "snapshot" with the session first, then stored events after Last-Event-ID
// (or ?after=), then live events, with the event type as the SSE event name
// and the seq as the SSE id; when the session finishes, a final "snapshot"
// and the stream closes. ?format=json returns the stored events instead.
func (s *Server) agentEvents(w http.ResponseWriter, r *http.Request) {
	if !s.agentsReady(w, r) {
		return
	}
	id := r.PathValue("id")
	ctx := r.Context()
	after, err := lastEventID(r)
	if err != nil {
		s.fail(w, r, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	switch r.URL.Query().Get("format") {
	case "", "sse":
	case "json":
		events, err := s.d.Agents.Events(ctx, id, after)
		if err != nil {
			s.agentError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, apiv1.AgentEventList{Events: events})
		return
	default:
		s.fail(w, r, http.StatusBadRequest, "invalid_request", "format must be sse or json")
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		s.internal(w, r, errors.New("response writer cannot stream"))
		return
	}
	live, cancel, err := s.d.Agents.Subscribe(ctx, id) // before reading history, so nothing falls in between
	if err != nil {
		s.agentError(w, r, err)
		return
	}
	defer cancel()
	snap, err := s.d.Agents.Get(ctx, id)
	if err != nil {
		s.agentError(w, r, err)
		return
	}
	history, err := s.d.Agents.Events(ctx, id, after)
	if err != nil {
		s.agentError(w, r, err)
		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if writeSSE(w, "snapshot", 0, snap) != nil {
		return
	}
	last := after
	for _, ev := range history {
		if writeSSE(w, string(ev.Type), ev.Seq, ev) != nil {
			return
		}
		last = max(last, ev.Seq)
	}
	fl.Flush()

	tick := time.NewTicker(SSEHeartbeat)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if _, err := io.WriteString(w, ": keep-alive\n\n"); err != nil {
				return
			}
			fl.Flush()
		case ev, open := <-live:
			if !open {
				// Finished (or this client fell behind): send the
				// authoritative state and end; a lagging client reconnects.
				if snap, err := s.d.Agents.Get(ctx, id); err == nil {
					_ = writeSSE(w, "snapshot", 0, snap)
					fl.Flush()
				}
				return
			}
			if ev.Seq <= last {
				continue // already sent from history
			}
			if writeSSE(w, string(ev.Type), ev.Seq, ev) != nil {
				return
			}
			last = ev.Seq
			fl.Flush()
		}
	}
}
