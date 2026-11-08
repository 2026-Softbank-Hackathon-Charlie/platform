package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/onboard"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/onboard/job"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

// onboardingRoutes registers the onboarding API (ADR 0042). Without a
// runner (Deps.Onboarding) every handler answers 501.
func (s *Server) onboardingRoutes(viewer, operator apiv1.Role) {
	if s.d.Onboarding == nil {
		for _, p := range []string{"POST /v1/onboardings", "GET /v1/onboardings", "GET /v1/onboardings/{id}", "DELETE /v1/onboardings/{id}",
			"GET /v1/onboardings/{id}/events", "POST /v1/onboardings/{id}/approve", "POST /v1/onboardings/{id}/cutover",
			"POST /v1/onboardings/{id}/decommission", "POST /v1/onboardings/{id}/resume"} {
			role := operator
			if strings.HasPrefix(p, "GET") {
				role = viewer
			}
			s.route(p, role, notImplemented)
		}
		return
	}
	cfg := *s.d.Onboarding
	cfg.Store, cfg.Apps = s.d.Store, onboardApps{s}
	if cfg.Secrets == nil {
		cfg.Secrets = s.d.Secrets
	}
	if cfg.AppDomain == "" {
		cfg.AppDomain = s.cfg.AppDomain
	}
	s.onb = job.New(cfg)
	if s.d.Plans != nil {
		s.d.Plans.SetOnboardingScopes(s.onb.Scope)
	}
	s.route("POST /v1/onboardings", operator, s.startOnboarding)
	s.route("GET /v1/onboardings", viewer, s.listOnboardings)
	s.route("GET /v1/onboardings/{id}", viewer, s.onboardingOp(func(ctx context.Context, id string, _ *http.Request) (apiv1.Onboarding, error) {
		return s.onb.Get(ctx, id)
	}, http.StatusOK))
	s.route("DELETE /v1/onboardings/{id}", operator, s.onboardingOp(func(ctx context.Context, id string, r *http.Request) (apiv1.Onboarding, error) {
		return s.onb.Delete(ctx, id, actor(r))
	}, http.StatusAccepted))
	s.route("GET /v1/onboardings/{id}/events", viewer, s.onboardingEvents)
	s.route("POST /v1/onboardings/{id}/approve", operator, s.approveOnboarding)
	s.route("POST /v1/onboardings/{id}/cutover", operator, s.onboardingOp(func(ctx context.Context, id string, r *http.Request) (apiv1.Onboarding, error) {
		return s.onb.Cutover(context.WithoutCancel(ctx), id, actor(r))
	}, http.StatusAccepted))
	s.route("POST /v1/onboardings/{id}/decommission", operator, s.onboardingOp(func(ctx context.Context, id string, r *http.Request) (apiv1.Onboarding, error) {
		return s.onb.Decommission(ctx, id, actor(r))
	}, http.StatusAccepted))
	s.route("POST /v1/onboardings/{id}/resume", operator, s.onboardingOp(func(ctx context.Context, id string, r *http.Request) (apiv1.Onboarding, error) {
		return s.onb.Resume(ctx, id, actor(r))
	}, http.StatusAccepted))
}

// Onboardings is the onboarding runner, nil without the onboarding API;
// cmd/api runs it.
func (s *Server) Onboardings() *job.Runner { return s.onb }

func notImplemented(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotImplemented, "not_implemented", r.Pattern+" needs the onboarding runner, which this server is not configured with")
}

func (s *Server) startOnboarding(w http.ResponseWriter, r *http.Request) {
	var req apiv1.OnboardingRequest
	if !s.decode(w, r, &req, true) {
		return
	}
	noteAudit(r, "repo", req.Repo)
	noteAudit(r, "serviceUrl", req.ServiceUrl)
	o, err := s.onb.Start(context.WithoutCancel(r.Context()), req, actor(r))
	if err != nil {
		s.onboardingError(w, r, err)
		return
	}
	noteAudit(r, "onboarding", o.Id)
	writeJSON(w, http.StatusAccepted, o)
}

func (s *Server) listOnboardings(w http.ResponseWriter, r *http.Request) {
	rows, err := s.d.Store.ListOnboardings(r.Context())
	if err != nil {
		s.internal(w, r, err)
		return
	}
	out := apiv1.OnboardingList{Onboardings: make([]apiv1.Onboarding, 0, len(rows))}
	for _, row := range rows {
		o, err := job.View(row)
		if err != nil {
			s.internal(w, r, err)
			return
		}
		out.Onboardings = append(out.Onboardings, o)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) onboardingOp(f func(ctx context.Context, id string, r *http.Request) (apiv1.Onboarding, error), status int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		noteAudit(r, "onboarding", id)
		o, err := f(r.Context(), id, r)
		if err != nil {
			s.onboardingError(w, r, err)
			return
		}
		writeJSON(w, status, o)
	}
}

func (s *Server) approveOnboarding(w http.ResponseWriter, r *http.Request) {
	var req apiv1.OnboardingApproveRequest
	if !s.decode(w, r, &req, true) {
		return
	}
	id := r.PathValue("id")
	noteAudit(r, "onboarding", id)
	noteAudit(r, "headSha", req.HeadSha)
	o, err := s.onb.Approve(context.WithoutCancel(r.Context()), id, req.HeadSha, actor(r))
	if err != nil {
		s.onboardingError(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, o)
}

func (s *Server) onboardingError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, job.ErrNotFound):
		s.fail(w, r, http.StatusNotFound, "onboarding_not_found", "onboarding not found")
	case errors.Is(err, job.ErrInvalid):
		s.fail(w, r, http.StatusBadRequest, "invalid_request", err.Error())
	case errors.Is(err, job.ErrExists):
		s.fail(w, r, http.StatusConflict, "onboarding_exists", err.Error())
	case errors.Is(err, job.ErrAppExists):
		s.fail(w, r, http.StatusConflict, "app_exists", err.Error())
	case errors.Is(err, job.ErrDeleting):
		s.fail(w, r, http.StatusConflict, "onboarding_deleting", err.Error())
	case errors.Is(err, job.ErrHeadMismatch):
		s.fail(w, r, http.StatusConflict, "pr_head_mismatch", err.Error())
	case errors.Is(err, job.ErrState), errors.Is(err, store.ErrConflict):
		s.fail(w, r, http.StatusConflict, "onboarding_state_conflict", err.Error())
	case errors.Is(err, job.ErrNotConfigured):
		s.fail(w, r, http.StatusServiceUnavailable, "unavailable", err.Error())
	default:
		s.internal(w, r, err)
	}
}

// OnboardingEventPoll is how often the events stream reads the store.
var OnboardingEventPoll = time.Second

// onboardingEvents streams an onboarding's events: a "snapshot" with the
// onboarding, then its events (kind as the SSE event, seq as the id),
// polled from the store, until the client leaves or the row is gone.
func (s *Server) onboardingEvents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ctx := r.Context()
	after, err := lastEventID(r)
	if err != nil {
		s.fail(w, r, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		s.internal(w, r, errors.New("response writer cannot stream"))
		return
	}
	o, err := s.onb.Get(ctx, id)
	if err != nil {
		s.onboardingError(w, r, err)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if writeSSE(w, "snapshot", 0, o) != nil {
		return
	}
	last := after
	send := func() error {
		evs, err := s.d.Store.ListOnboardingEvents(ctx, id, last, 500)
		if err != nil {
			return err
		}
		for _, e := range evs {
			if err := writeSSE(w, e.Kind, e.Seq, onboardingEventJSON(e)); err != nil {
				return err
			}
			last = e.Seq
		}
		fl.Flush()
		return nil
	}
	if send() != nil {
		return
	}
	poll := time.NewTicker(OnboardingEventPoll)
	defer poll.Stop()
	beat := time.NewTicker(SSEHeartbeat)
	defer beat.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-beat.C:
			if _, err := io.WriteString(w, ": keep-alive\n\n"); err != nil {
				return
			}
			fl.Flush()
		case <-poll.C:
			if send() != nil {
				return
			}
			if _, err := s.d.Store.GetOnboarding(ctx, id); errors.Is(err, store.ErrNotFound) {
				return // deleted
			}
		}
	}
}

func onboardingEventJSON(e store.OnboardingEvent) apiv1.OnboardingEvent {
	out := apiv1.OnboardingEvent{Seq: e.Seq, OnboardingId: e.OnboardingID, Time: e.Time, State: apiv1.OnboardingState(e.State), Kind: apiv1.OnboardingEventKind(e.Kind)}
	if e.Message != "" {
		out.Message = &e.Message
	}
	if len(e.Data) > 0 {
		var d map[string]any
		if json.Unmarshal(e.Data, &d) == nil {
			out.Data = &d
		}
	}
	return out
}

// onboardingGuard answers 409 onboarding_exists when app has an onboarding
// that is not done (ADR 0042 addendum): app create and delete, environment
// add and remove, plans of its database pair. It reports whether it did.
func (s *Server) onboardingGuard(w http.ResponseWriter, r *http.Request, app string) bool {
	if !s.cfg.Onboarding || app == "" {
		return false
	}
	active, _, err := job.ActiveFor(r.Context(), s.d.Store, app)
	if err != nil {
		s.internal(w, r, err)
		return true
	}
	if active {
		s.fail(w, r, http.StatusConflict, "onboarding_exists", fmt.Sprintf("app %s is being onboarded; resume or delete its onboarding first", app))
		return true
	}
	return false
}

// onboardingName refuses the test apps' names for POST /v1/apps.
func (s *Server) onboardingName(w http.ResponseWriter, r *http.Request, app string) bool {
	if s.cfg.Onboarding && strings.HasPrefix(app, onboard.IDPrefix) {
		s.fail(w, r, http.StatusConflict, "app_exists", fmt.Sprintf("names starting with %s are kept for onboardings' test apps", onboard.IDPrefix))
		return true
	}
	return s.onboardingGuard(w, r, app)
}

// guardApp wraps a route of /v1/apps/{name} with onboardingGuard.
func (s *Server) guardApp(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.onboardingGuard(w, r, r.PathValue("name")) {
			return
		}
		h(w, r)
	}
}

// guardPlan wraps POST /v1/plans, /v1/plans/draft and /v1/migrations: a
// plan of an app being onboarded (by the draft's app, the plan's
// namespaces, or the started plan's) is 409 onboarding_exists. Only the
// onboarding's own plan moves it, and that is not submitted here.
func (s *Server) guardPlan(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.cfg.Onboarding {
			h(w, r)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBodyBytes))
		if err != nil {
			s.fail(w, r, http.StatusRequestEntityTooLarge, "invalid_request", "the body is over "+fmt.Sprint(MaxBodyBytes)+" bytes or unreadable")
			return
		}
		r.Body = io.NopCloser(strings.NewReader(string(body)))
		var v struct {
			App     string `json:"app"`
			PlanRef string `json:"planRef"`
			Source  struct {
				Namespace string `json:"namespace"`
			} `json:"source"`
			Target struct {
				Namespace string `json:"namespace"`
			} `json:"target"`
		}
		_ = json.Unmarshal(body, &v)
		if v.PlanRef != "" {
			if p, err := s.d.Store.GetPlan(r.Context(), v.PlanRef); err == nil {
				_ = json.Unmarshal(p.Raw, &v)
			}
		}
		for _, app := range []string{v.App, v.Source.Namespace, v.Target.Namespace} {
			if s.onboardingGuard(w, r, app) {
				return
			}
		}
		h(w, r)
	}
}
