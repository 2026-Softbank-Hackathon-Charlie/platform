package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/auth"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/runs"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

// MaxBodyBytes bounds request bodies.
const MaxBodyBytes = 1 << 20

func (s *Server) readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBodyBytes))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			s.fail(w, r, http.StatusRequestEntityTooLarge, "too_large", fmt.Sprintf("body exceeds %d bytes", MaxBodyBytes))
		} else {
			s.fail(w, r, http.StatusBadRequest, "invalid_request", "could not read body")
		}
		return nil, false
	}
	return body, true
}

// decode reads an optional (empty body allowed) or required JSON body strictly.
func (s *Server) decode(w http.ResponseWriter, r *http.Request, v any, required bool) bool {
	body, ok := s.readBody(w, r)
	if !ok {
		return false
	}
	if len(bytes.TrimSpace(body)) == 0 {
		if required {
			s.fail(w, r, http.StatusBadRequest, "invalid_request", "request body is required")
			return false
		}
		return true
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		s.fail(w, r, http.StatusBadRequest, "invalid_request", "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	writeJSON(w, http.StatusOK, apiv1.Principal{Name: p.Name, Role: p.Role})
}

// --- plans ---

// POST /v1/plans: the body is the engine plan JSON as-is.
func (s *Server) submitPlan(w http.ResponseWriter, r *http.Request) {
	body, ok := s.readBody(w, r)
	if !ok {
		return
	}
	p, _ := auth.FromContext(r.Context())
	rec, err := s.d.Plans.SubmitAs(r.Context(), body, p.Name, p.Role)
	if err != nil {
		s.planError(w, r, err)
		return
	}
	noteAudit(r, "planRef", rec.Ref)
	noteAudit(r, "planId", rec.Summary.PlanId)
	noteAudit(r, "route", rec.Summary.Source.Env+" -> "+rec.Summary.Target.Env)
	w.Header().Set("Location", "/v1/plans/"+rec.Ref)
	writeJSON(w, http.StatusCreated, rec)
}

func (s *Server) listPlans(w http.ResponseWriter, r *http.Request) {
	list, err := s.d.Plans.List(r.Context())
	if err != nil {
		s.internal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, apiv1.PlanList{Plans: list})
}

func (s *Server) getPlan(w http.ResponseWriter, r *http.Request) {
	rec, err := s.d.Plans.Get(r.Context(), r.PathValue("ref"))
	if err != nil {
		s.planError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

// POST /v1/plans/{ref}/approve: the human gate between AI planning and execution.
func (s *Server) approvePlan(w http.ResponseWriter, r *http.Request) {
	var req apiv1.ReviewRequest
	if !s.decode(w, r, &req, false) {
		return
	}
	ref := r.PathValue("ref")
	noteAudit(r, "planRef", ref)
	if req.Revision != nil && *req.Revision < 1 {
		s.fail(w, r, http.StatusBadRequest, "invalid_request", "revision must be at least 1")
		return
	}
	rec, err := s.d.Plans.Approve(r.Context(), ref, actor(r), deref(req.Comment), req.Revision)
	if err != nil {
		s.planError(w, r, err)
		return
	}
	noteAudit(r, "revision", rec.Revision)
	writeJSON(w, http.StatusOK, rec)
}

func (s *Server) rejectPlan(w http.ResponseWriter, r *http.Request) {
	var req apiv1.ReviewRequest
	if !s.decode(w, r, &req, true) {
		return
	}
	ref := r.PathValue("ref")
	noteAudit(r, "planRef", ref)
	if deref(req.Comment) == "" {
		s.fail(w, r, http.StatusBadRequest, "invalid_request", "a comment explaining the rejection is required")
		return
	}
	noteAudit(r, "comment", *req.Comment)
	rec, err := s.d.Plans.Reject(r.Context(), ref, actor(r), *req.Comment)
	if err != nil {
		s.planError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

// --- migrations ---

// POST /v1/migrations
//
// Retries: send the same Idempotency-Key to get the original run back instead
// of an error. Reusing a key for a different plan is a conflict.
func (s *Server) startMigration(w http.ResponseWriter, r *http.Request) {
	var req apiv1.StartMigrationRequest
	if !s.decode(w, r, &req, true) {
		return
	}
	if req.PlanRef == "" {
		s.fail(w, r, http.StatusBadRequest, "invalid_request", "planRef is required")
		return
	}
	noteAudit(r, "planRef", req.PlanRef)
	key := r.Header.Get("Idempotency-Key")
	if len(key) > 200 {
		s.fail(w, r, http.StatusBadRequest, "invalid_request", "Idempotency-Key is longer than 200 characters")
		return
	}
	// From here on every write must finish even if the client goes away:
	// a plan left in starting, a run without its plan link or a missing
	// idempotency key would each break the retry the key exists for.
	ctx := context.WithoutCancel(r.Context())

	s.startMu.Lock()
	defer s.startMu.Unlock()

	if key != "" {
		planRef, runID, err := s.d.Store.GetIdempotencyKey(ctx, key)
		switch {
		case err == nil && planRef != req.PlanRef:
			s.fail(w, r, http.StatusConflict, "idempotency_key_reused", "Idempotency-Key was already used for a different plan")
			return
		case err == nil:
			noteAudit(r, "runId", runID)
			noteAudit(r, "idempotentReplay", true)
			writeJSON(w, http.StatusAccepted, startResponse(runID, planRef))
			return
		case !errors.Is(err, store.ErrNotFound):
			s.internal(w, r, err)
			return
		}
	}

	res, err := s.d.Plans.Reserve(ctx, req.PlanRef)
	if err != nil {
		s.planError(w, r, err)
		return
	}
	noteResidency(r, res.Residency)
	noteCapacity(r, res.Capacity)
	noteVersion(r, res.Version)
	noteParity(r, res.Parity)
	run, err := s.d.Runs.Start(ctx, runs.StartOptions{
		PlanRef:    req.PlanRef,
		Plan:       res.Plan,
		App:        res.Summary.App,
		Source:     res.Summary.Source,
		Target:     res.Summary.Target,
		Actor:      actor(r),
		AutoSwitch: req.AutoSwitch != nil && *req.AutoSwitch,
		Canary:     req.Canary,
		Observe:    req.Observe,
		RollbackOf: res.RollbackOf, // a rollback plan started by hand is still that run's rollback
	})
	if err != nil {
		s.d.Plans.Release(ctx, req.PlanRef)
		s.runError(w, r, err)
		return
	}
	if err := s.d.Plans.Started(ctx, req.PlanRef, run.RunId); err != nil {
		s.cfg.Log.Error("link plan to run", "plan", req.PlanRef, "run", run.RunId, "err", err)
	}
	if key != "" {
		if _, _, err := s.d.Store.PutIdempotencyKey(ctx, key, req.PlanRef, run.RunId); err != nil {
			s.cfg.Log.Error("store idempotency key", "err", err)
		}
	}
	noteAudit(r, "runId", run.RunId)
	noteAudit(r, "autoSwitch", run.AutoSwitch)
	noteAudit(r, "canary", req.Canary != nil)
	noteAudit(r, "observe", req.Observe != nil)
	w.Header().Set("Location", "/v1/migrations/"+run.RunId)
	writeJSON(w, http.StatusAccepted, startResponse(run.RunId, req.PlanRef))
}

func startResponse(runID, planRef string) apiv1.StartMigrationResponse {
	resp := apiv1.StartMigrationResponse{RunId: runID, PlanRef: planRef}
	resp.Links.Self = "/v1/migrations/" + runID
	resp.Links.Events = "/v1/migrations/" + runID + "/events"
	return resp
}

func (s *Server) listMigrations(w http.ResponseWriter, r *http.Request) {
	list, err := s.d.Runs.List(r.Context())
	if err != nil {
		s.internal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, apiv1.RunList{Runs: list})
}

func (s *Server) getMigration(w http.ResponseWriter, r *http.Request) {
	run, err := s.d.Runs.Get(r.Context(), r.PathValue("runId"))
	if err != nil {
		s.runError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

func (s *Server) switchMigration(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("runId")
	noteAudit(r, "runId", id)
	run, err := s.d.Runs.Switch(r.Context(), id, actor(r))
	if err != nil {
		s.runError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

func (s *Server) abortMigration(w http.ResponseWriter, r *http.Request) {
	var req apiv1.AbortRequest
	if !s.decode(w, r, &req, false) {
		return
	}
	id := r.PathValue("runId")
	noteAudit(r, "runId", id)
	if req.Reason != nil {
		noteAudit(r, "reason", *req.Reason)
	}
	run, err := s.d.Runs.Abort(r.Context(), id, actor(r), deref(req.Reason))
	if err != nil {
		s.runError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

func (s *Server) resumeMigration(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("runId")
	noteAudit(r, "runId", id)
	ctx := context.WithoutCancel(r.Context())
	cur, err := s.d.Runs.Get(ctx, id)
	if err != nil {
		s.runError(w, r, err)
		return
	}
	if cur.Status == apiv1.RunStatusHalted {
		noteAudit(r, "afterHalt", true)
	}
	p, err := s.d.Plans.Load(ctx, cur.PlanRef)
	if err != nil {
		s.planError(w, r, err)
		return
	}
	s.startMu.Lock()
	defer s.startMu.Unlock()
	run, err := s.d.Runs.Resume(ctx, id, actor(r), p)
	if err != nil {
		s.runError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

// --- environments, traffic, matrix, audit ---

func (s *Server) listEnvs(w http.ResponseWriter, r *http.Request) {
	list := s.d.Envs.List(r.Context())
	s.markLive(r.Context(), list)
	writeJSON(w, http.StatusOK, apiv1.EnvironmentList{Environments: list})
}

func (s *Server) getEnv(w http.ResponseWriter, r *http.Request) {
	env, err := s.d.Envs.Get(r.Context(), r.PathValue("name"))
	if err != nil {
		if s.d.EnvNotFound != nil && errors.Is(err, s.d.EnvNotFound) {
			s.fail(w, r, http.StatusNotFound, "env_not_found", "environment not found")
			return
		}
		s.internal(w, r, err)
		return
	}
	one := []apiv1.Environment{env}
	s.markLive(r.Context(), one)
	writeJSON(w, http.StatusOK, one[0])
}

// markLive sets Live on the environment whose backend receives writes.
func (s *Server) markLive(ctx context.Context, envs []apiv1.Environment) {
	if s.d.Traffic == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	ts, err := s.d.Traffic.State(ctx, 10*time.Second)
	if err != nil {
		return // unknown is shown as not live; /v1/traffic reports the error
	}
	for i := range envs {
		envs[i].Live = envs[i].GatewayBackend == ts.WriteBackend
	}
}

func (s *Server) traffic(w http.ResponseWriter, r *http.Request) {
	if s.d.Traffic == nil {
		s.fail(w, r, http.StatusServiceUnavailable, "gateway_not_configured", "this server has no traffic gateway configured (PLATFORM_GATEWAY_URL)")
		return
	}
	window := 10 * time.Second
	if v := r.URL.Query().Get("window"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < time.Second || d > 5*time.Minute {
			s.fail(w, r, http.StatusBadRequest, "invalid_request", "window must be a duration between 1s and 5m")
			return
		}
		window = d
	}
	traffic := s.d.Traffic
	if app := r.URL.Query().Get("app"); app != "" {
		// Only an app plans can be drafted for (the apps file, or a
		// registered app with a database pair): the same names plans use
		// as gateway scopes.
		_, known, err := s.catalogApp(r.Context(), app)
		if err != nil {
			s.internal(w, r, err)
			return
		}
		if s.d.TrafficApp == nil || !known {
			s.fail(w, r, http.StatusBadRequest, "invalid_request", "app must be an app registered on this server (GET /v1/apps)")
			return
		}
		traffic = s.d.TrafficApp(app)
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	ts, err := traffic.State(ctx, window)
	if err != nil {
		s.cfg.Log.Warn("gateway state", "err", err)
		s.fail(w, r, http.StatusBadGateway, "gateway_unreachable", "could not read the traffic gateway state")
		return
	}
	writeJSON(w, http.StatusOK, ts)
}

var finished = map[apiv1.RunStatus]bool{
	apiv1.RunStatusSucceeded:  true,
	apiv1.RunStatusRolledBack: true,
	apiv1.RunStatusHalted:     true,
	apiv1.RunStatusFailed:     true,
	apiv1.RunStatusAborted:    true,
}

// GET /v1/matrix: the latest finished run for every ordered pair of environments.
func (s *Server) matrix(w http.ResponseWriter, r *http.Request) {
	list, err := s.d.Runs.List(r.Context())
	if err != nil {
		s.internal(w, r, err)
		return
	}
	type pair struct{ src, dst string }
	latest := map[pair]apiv1.Run{}
	for _, run := range list {
		if !finished[run.Status] || run.EndedAt == nil {
			continue
		}
		k := pair{run.Source.Env, run.Target.Env}
		if cur, ok := latest[k]; !ok || run.EndedAt.After(*cur.EndedAt) {
			latest[k] = run
		}
	}
	names := s.d.Envs.Names()
	m := apiv1.Matrix{Environments: names, Routes: []apiv1.MatrixRoute{}}
	for _, src := range names {
		for _, dst := range names {
			if src == dst {
				continue
			}
			row := apiv1.MatrixRoute{Source: src, Target: dst}
			if run, ok := latest[pair{src, dst}]; ok {
				id, st, res := run.RunId, run.Status, run.Result
				row.RunId, row.Status, row.FinishedAt, row.Result = &id, &st, run.EndedAt, &res
			}
			m.Routes = append(m.Routes, row)
		}
	}
	writeJSON(w, http.StatusOK, m)
}

func (s *Server) listAudit(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := 100
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 500 {
			s.fail(w, r, http.StatusBadRequest, "invalid_request", "limit must be between 1 and 500")
			return
		}
		limit = n
	}
	var before int64
	if v := q.Get("before"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 1 {
			s.fail(w, r, http.StatusBadRequest, "invalid_request", "before must be a positive id")
			return
		}
		before = n
	}
	rows, err := s.d.Store.ListAudit(r.Context(), before, limit)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	out := apiv1.AuditList{Entries: make([]apiv1.AuditEntry, 0, len(rows))}
	for _, a := range rows {
		e := apiv1.AuditEntry{Id: a.ID, Time: a.Time, Actor: a.Actor, Role: a.Role, Action: a.Action, Target: a.Target, Status: a.Status}
		if a.RequestID != "" {
			e.RequestId = &a.RequestID
		}
		if len(a.Detail) > 0 {
			var m map[string]any
			if json.Unmarshal(a.Detail, &m) == nil {
				e.Detail = &m
			}
		}
		out.Entries = append(out.Entries, e)
	}
	if len(rows) == limit {
		next := rows[len(rows)-1].ID
		out.NextBefore = &next
	}
	writeJSON(w, http.StatusOK, out)
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
