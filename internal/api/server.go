// Package api is the platform HTTP API.
//
//	GET  /healthz
//	POST /v1/plans                         submit a plan (AI or human) → pending_approval
//	GET  /v1/plans                         list plans
//	GET  /v1/plans/{ref}                   one plan (never includes DSNs or tokens)
//	POST /v1/plans/{ref}/approve           human approval
//	POST /v1/migrations                    start an approved plan → 202 + runId
//	GET  /v1/migrations                    list runs (engine state)
//	GET  /v1/migrations/{runId}            one run (engine state)
//	GET  /v1/migrations/{runId}/events     SSE: snapshot, then live engine events
package api

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/engine"
	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/plans"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/runs"
)

// Runs is what the API needs from the runner.
type Runs interface {
	Start(p *plan.Plan) (string, error)
	Get(id string) (engine.State, error)
	List() []engine.State
	Subscribe(id string) (<-chan engine.Event, func(), error)
}

// Config configures the API.
type Config struct {
	// Token is the operator bearer token. Empty disables auth (local development only).
	Token string
	// Actor is recorded as submitter/approver for requests authenticated with Token.
	Actor string
	Log   *slog.Logger
}

// Server is the HTTP handler.
type Server struct {
	cfg   Config
	plans *plans.Service
	runs  Runs
	mux   *http.ServeMux

	startMu sync.Mutex
	idem    map[string]idemEntry // Idempotency-Key → started run
}

type idemEntry struct{ planRef, runID string }

// New wires the routes.
func New(cfg Config, ps *plans.Service, rs Runs) *Server {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Actor == "" {
		cfg.Actor = "operator"
	}
	s := &Server{cfg: cfg, plans: ps, runs: rs, mux: http.NewServeMux(), idem: map[string]idemEntry{}}
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })
	s.mux.HandleFunc("POST /v1/plans", s.auth(s.submitPlan))
	s.mux.HandleFunc("GET /v1/plans", s.auth(s.listPlans))
	s.mux.HandleFunc("GET /v1/plans/{ref}", s.auth(s.getPlan))
	s.mux.HandleFunc("POST /v1/plans/{ref}/approve", s.auth(s.approvePlan))
	s.mux.HandleFunc("POST /v1/migrations", s.auth(s.startMigration))
	s.mux.HandleFunc("GET /v1/migrations", s.auth(s.listMigrations))
	s.mux.HandleFunc("GET /v1/migrations/{runId}", s.auth(s.getMigration))
	s.mux.HandleFunc("GET /v1/migrations/{runId}/events", s.auth(s.migrationEvents))
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

func (s *Server) auth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.Token != "" {
			got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			if !ok || subtle.ConstantTimeCompare([]byte(got), []byte(s.cfg.Token)) != 1 {
				writeError(w, http.StatusUnauthorized, "unauthorized", "missing or invalid bearer token")
				return
			}
		}
		h(w, r)
	}
}

// --- errors ---

// Error is the body of every non-2xx response.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]Error{"error": {Code: code, Message: msg}})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// planError maps plan service errors to HTTP.
func (s *Server) planError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, plans.ErrNotFound):
		writeError(w, http.StatusNotFound, "plan_not_found", "plan not found")
	case errors.Is(err, plans.ErrInvalid):
		writeError(w, http.StatusBadRequest, "invalid_plan", err.Error())
	case errors.Is(err, plans.ErrWrongStatus):
		writeError(w, http.StatusConflict, "plan_state_conflict", err.Error())
	default:
		s.internal(w, err)
	}
}

func (s *Server) runError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, runs.ErrNotFound):
		writeError(w, http.StatusNotFound, "run_not_found", "run not found")
	case errors.Is(err, runs.ErrBusy):
		writeError(w, http.StatusConflict, "cluster_busy", err.Error())
	case errors.Is(err, runs.ErrEngineInit):
		// The cause can name server paths or cluster URLs: log it, return a hint.
		s.cfg.Log.Error("engine init failed", "err", err)
		writeError(w, http.StatusBadGateway, "engine_init_failed", "could not initialize the migration engine; check the plan's kube contexts and the server logs")
	default:
		s.internal(w, err)
	}
}

// internal logs the cause and returns a generic message: engine errors can
// carry connection details.
func (s *Server) internal(w http.ResponseWriter, err error) {
	s.cfg.Log.Error("internal error", "err", err)
	writeError(w, http.StatusInternalServerError, "internal", "internal error; see server logs")
}
