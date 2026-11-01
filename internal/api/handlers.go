package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// MaxPlanBytes bounds request bodies.
const MaxPlanBytes = 1 << 20

func readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxPlanBytes))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, "too_large", fmt.Sprintf("body exceeds %d bytes", MaxPlanBytes))
		} else {
			writeError(w, http.StatusBadRequest, "invalid_request", "could not read body")
		}
		return nil, false
	}
	return body, true
}

// POST /v1/plans — body is the engine plan JSON as-is.
func (s *Server) submitPlan(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	rec, err := s.plans.Submit(body, s.cfg.Actor)
	if err != nil {
		s.planError(w, err)
		return
	}
	w.Header().Set("Location", "/v1/plans/"+rec.Ref)
	writeJSON(w, http.StatusCreated, rec)
}

func (s *Server) listPlans(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"plans": s.plans.List()})
}

func (s *Server) getPlan(w http.ResponseWriter, r *http.Request) {
	rec, err := s.plans.Get(r.PathValue("ref"))
	if err != nil {
		s.planError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

// POST /v1/plans/{ref}/approve — the human gate between AI planning and execution.
func (s *Server) approvePlan(w http.ResponseWriter, r *http.Request) {
	rec, err := s.plans.Approve(r.PathValue("ref"), s.cfg.Actor)
	if err != nil {
		s.planError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

// StartRequest is the body of POST /v1/migrations.
type StartRequest struct {
	PlanRef string `json:"planRef"`
}

// StartResponse acknowledges that a run was accepted. Completion is observed via
// GET /v1/migrations/{runId} or its event stream.
type StartResponse struct {
	RunID   string            `json:"runId"`
	PlanRef string            `json:"planRef"`
	Links   map[string]string `json:"links"`
}

// POST /v1/migrations
//
// Retries: send the same Idempotency-Key to get the original run back instead
// of an error. Reusing a key for a different plan is a conflict.
func (s *Server) startMigration(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	var req StartRequest
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || req.PlanRef == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", `body must be {"planRef": "<ref>"}`)
		return
	}
	key := r.Header.Get("Idempotency-Key")

	// One start at a time keeps the idempotency map and plan reservation consistent.
	s.startMu.Lock()
	defer s.startMu.Unlock()

	if key != "" {
		if prev, ok := s.idem[key]; ok {
			if prev.planRef != req.PlanRef {
				writeError(w, http.StatusConflict, "idempotency_key_reused", "Idempotency-Key was already used for a different plan")
				return
			}
			writeJSON(w, http.StatusAccepted, startResponse(prev.runID, prev.planRef))
			return
		}
	}

	p, err := s.plans.Reserve(req.PlanRef)
	if err != nil {
		s.planError(w, err)
		return
	}
	runID, err := s.runs.Start(p)
	if err != nil {
		s.plans.Release(req.PlanRef)
		s.runError(w, err)
		return
	}
	s.plans.Started(req.PlanRef, runID)
	if key != "" {
		s.idem[key] = idemEntry{planRef: req.PlanRef, runID: runID}
	}
	w.Header().Set("Location", "/v1/migrations/"+runID)
	writeJSON(w, http.StatusAccepted, startResponse(runID, req.PlanRef))
}

func startResponse(runID, planRef string) StartResponse {
	return StartResponse{RunID: runID, PlanRef: planRef, Links: map[string]string{
		"self":   "/v1/migrations/" + runID,
		"events": "/v1/migrations/" + runID + "/events",
	}}
}

func (s *Server) listMigrations(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"runs": s.runs.List()})
}

func (s *Server) getMigration(w http.ResponseWriter, r *http.Request) {
	st, err := s.runs.Get(r.PathValue("runId"))
	if err != nil {
		s.runError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// SSEHeartbeat keeps idle connections open through proxies.
var SSEHeartbeat = 15 * time.Second

// GET /v1/migrations/{runId}/events
//
// Stream contract:
//   - first message: "snapshot" with the full run state (the source of truth)
//   - then each engine event, with the engine's kind as the SSE event name
//   - when the run ends: a final "snapshot", then the stream closes
//
// The engine does not number its events, so there is no Last-Event-ID resume:
// a reconnecting client receives a fresh snapshot and continues from there.
// Events may repeat information already in the snapshot. A client that falls
// more than runs.SubscriberBuffer events behind is disconnected.
func (s *Server) migrationEvents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("runId")
	fl, ok := w.(http.Flusher)
	if !ok {
		s.internal(w, errors.New("response writer cannot stream"))
		return
	}
	events, cancel, err := s.runs.Subscribe(id) // before the snapshot, so nothing is missed
	if err != nil {
		s.runError(w, err)
		return
	}
	defer cancel()
	st, err := s.runs.Get(id)
	if err != nil {
		s.runError(w, err)
		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if writeSSE(w, "snapshot", st) != nil {
		return
	}
	fl.Flush()

	tick := time.NewTicker(SSEHeartbeat)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-tick.C:
			if _, err := io.WriteString(w, ": keep-alive\n\n"); err != nil {
				return
			}
			fl.Flush()
		case ev, open := <-events:
			if !open {
				// Run finished (or we fell behind): send the authoritative state and end.
				if st, err := s.runs.Get(id); err == nil {
					_ = writeSSE(w, "snapshot", st)
					fl.Flush()
				}
				return
			}
			if writeSSE(w, ev.Kind, ev) != nil {
				return
			}
			fl.Flush()
		}
	}
}

func writeSSE(w io.Writer, event string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
	return err
}
