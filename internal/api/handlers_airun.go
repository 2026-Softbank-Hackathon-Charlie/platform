package api

import (
	"errors"
	"net/http"
	"slices"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/airun"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

func (s *Server) aiRunner(w http.ResponseWriter, r *http.Request) bool {
	if s.d.AIRuns == nil {
		s.fail(w, r, http.StatusServiceUnavailable, "ai_runner_not_configured", "the AI runner is not configured on this server (PLATFORM_AIRUN_JOB_TEMPLATE); run hopai by hand")
		return false
	}
	return true
}

func (s *Server) startAIRun(w http.ResponseWriter, r *http.Request) {
	if !s.aiRunner(w, r) {
		return
	}
	var req apiv1.AIRunRequest
	if !s.decode(w, r, &req, false) {
		return
	}
	noteAudit(r, "app", req.App)
	noteAudit(r, "route", req.Source+" -> "+req.Target)
	// Only apps and routes the server can draft a plan for: the runner starts
	// from POST /v1/plans/draft and would fail there anyway.
	if s.d.Apps == nil {
		s.fail(w, r, http.StatusServiceUnavailable, "apps_not_configured", "the server has no apps file (PLATFORM_APPS_FILE)")
		return
	}
	a, known, err := s.d.Apps.Lookup(r.Context(), req.App)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	if known {
		envs := make([]string, 0, len(a.Environments))
		for _, e := range a.Environments {
			envs = append(envs, e.Env)
		}
		known = slices.Contains(envs, req.Source) && slices.Contains(envs, req.Target)
	}
	if !known || req.Source == req.Target {
		s.fail(w, r, http.StatusBadRequest, "invalid_request", "app "+req.App+" is not configured for "+req.Source+" -> "+req.Target+" (GET /v1/apps)")
		return
	}
	run, err := s.d.AIRuns.Start(r.Context(), airun.Request{App: req.App, Source: req.Source, Target: req.Target}, actor(r))
	switch {
	case errors.Is(err, airun.ErrBusy):
		s.fail(w, r, http.StatusConflict, "ai_run_active", err.Error())
		return
	case errors.Is(err, airun.ErrInvalid):
		s.fail(w, r, http.StatusBadRequest, "invalid_request", err.Error())
		return
	case err != nil:
		s.internal(w, r, err)
		return
	}
	noteAudit(r, "job", run.Name)
	writeJSON(w, http.StatusAccepted, aiRunOf(run))
}

func (s *Server) listAIRuns(w http.ResponseWriter, r *http.Request) {
	if !s.aiRunner(w, r) {
		return
	}
	runs, err := s.d.AIRuns.List(r.Context(), r.URL.Query().Get("app"))
	if errors.Is(err, airun.ErrInvalid) {
		s.fail(w, r, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if err != nil {
		s.internal(w, r, err)
		return
	}
	out := apiv1.AIRunList{Runs: make([]apiv1.AIRun, 0, len(runs))}
	for _, x := range runs {
		out.Runs = append(out.Runs, aiRunOf(x))
	}
	writeJSON(w, http.StatusOK, out)
}

func aiRunOf(x airun.Run) apiv1.AIRun {
	return apiv1.AIRun{
		Name: x.Name, App: x.App, Source: x.Source, Target: x.Target,
		Status: apiv1.AIRunStatus(x.Status), RequestedBy: x.RequestedBy,
		CreatedAt: x.CreatedAt, FinishedAt: x.FinishedAt,
	}
}
