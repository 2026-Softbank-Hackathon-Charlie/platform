// Package api is the platform HTTP API. The contract is api/openapi.yaml; the
// request and response types come from pkg/apiv1.
package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"sync"
	"time"

	specdoc "github.com/2026-Softbank-Hackathon-Charlie/platform/api"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/auth"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/plans"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/runs"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

// Envs is the environment registry.
type Envs interface {
	List(ctx context.Context) []apiv1.Environment
	Get(ctx context.Context, name string) (apiv1.Environment, error)
	Names() []string
}

// Traffic reads the gateway state.
type Traffic interface {
	State(ctx context.Context, window time.Duration) (apiv1.TrafficState, error)
}

// Config configures the API.
type Config struct {
	// Tokens authenticates callers. When Insecure is set and Tokens is empty,
	// every request acts as the operator "dev" (local development only).
	Tokens   *auth.Tokens
	Insecure bool
	// CORSOrigins lists browser origins allowed to call the API ("*" allows any).
	CORSOrigins []string
	Log         *slog.Logger
}

// Deps are the services behind the API.
type Deps struct {
	Plans *plans.Service
	Runs  *runs.Runner
	Store store.Store // audit log and idempotency keys
	Envs  Envs
	// Traffic is nil when no gateway is configured.
	Traffic Traffic
	// EnvNotFound is the error Envs.Get returns for an unknown name.
	EnvNotFound error
}

// Server is the HTTP handler.
type Server struct {
	cfg Config
	d   Deps
	mux *http.ServeMux

	startMu sync.Mutex // one start at a time keeps idempotency and plan reservation consistent
}

// New wires the routes.
func New(cfg Config, d Deps) *Server {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Tokens == nil {
		cfg.Tokens = &auth.Tokens{}
	}
	s := &Server{cfg: cfg, d: d, mux: http.NewServeMux()}
	viewer, agent, operator := apiv1.RoleViewer, apiv1.RoleAgent, apiv1.RoleOperator

	s.route("GET /healthz", "", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, apiv1.Health{Status: "ok"})
	})
	s.route("GET /openapi.yaml", "", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		_, _ = w.Write(specdoc.OpenAPI)
	})
	s.route("GET /v1/me", viewer, s.me)
	s.route("POST /v1/plans", agent, s.submitPlan)
	s.route("GET /v1/plans", viewer, s.listPlans)
	s.route("GET /v1/plans/{ref}", viewer, s.getPlan)
	s.route("POST /v1/plans/{ref}/approve", operator, s.approvePlan)
	s.route("POST /v1/plans/{ref}/reject", operator, s.rejectPlan)
	s.route("POST /v1/migrations", operator, s.startMigration)
	s.route("GET /v1/migrations", viewer, s.listMigrations)
	s.route("GET /v1/migrations/{runId}", viewer, s.getMigration)
	s.route("GET /v1/migrations/{runId}/events", viewer, s.migrationEvents)
	s.route("POST /v1/migrations/{runId}/switch", operator, s.switchMigration)
	s.route("POST /v1/migrations/{runId}/abort", operator, s.abortMigration)
	s.route("POST /v1/migrations/{runId}/resume", operator, s.resumeMigration)
	s.route("GET /v1/envs", viewer, s.listEnvs)
	s.route("GET /v1/envs/{name}", viewer, s.getEnv)
	s.route("GET /v1/traffic", viewer, s.traffic)
	s.route("GET /v1/matrix", viewer, s.matrix)
	s.route("GET /v1/audit", viewer, s.listAudit)
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	id := r.Header.Get("X-Request-Id")
	if !requestIDPattern.MatchString(id) {
		id = randomHex(8)
	}
	w.Header().Set("X-Request-Id", id)
	r = r.WithContext(context.WithValue(r.Context(), requestIDKey{}, id))
	if s.cors(w, r) {
		return
	}
	s.mux.ServeHTTP(w, r)
}

// route registers h behind authentication, the role check and the audit log.
// need == "" means the route is public.
func (s *Server) route(pattern string, need apiv1.Role, h http.HandlerFunc) {
	s.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		rec := &recorder{ResponseWriter: w, status: http.StatusOK}
		note := map[string]any{}
		ctx := context.WithValue(r.Context(), noteKey{}, note)

		var p auth.Principal
		if need != "" {
			var ok bool
			p, ok = s.authenticate(r)
			switch {
			case !ok:
				writeError(rec, http.StatusUnauthorized, "unauthorized", "missing or invalid bearer token")
			case !p.Allows(need):
				writeError(rec, http.StatusForbidden, "forbidden", "role "+string(p.Role)+" may not do this; it needs "+string(need))
			default:
				h(rec, r.WithContext(auth.WithPrincipal(ctx, p)))
			}
		} else {
			h(rec, r.WithContext(ctx))
		}
		if need != "" && (!isRead(r.Method) || rec.status == http.StatusUnauthorized || rec.status == http.StatusForbidden) {
			s.audit(r, p, pattern, rec.status, note)
		}
	})
}

func (s *Server) authenticate(r *http.Request) (auth.Principal, bool) {
	if s.cfg.Tokens.Len() == 0 && s.cfg.Insecure {
		return auth.Principal{Name: "dev", Role: apiv1.RoleOperator}, true
	}
	return s.cfg.Tokens.Authenticate(r)
}

func (s *Server) audit(r *http.Request, p auth.Principal, pattern string, status int, note map[string]any) {
	actor, role := p.Name, string(p.Role)
	if actor == "" {
		actor, role = "anonymous", ""
	}
	var detail []byte
	if len(note) > 0 {
		detail, _ = json.Marshal(note)
	}
	reqID, _ := r.Context().Value(requestIDKey{}).(string)
	// The response is already sent; a slow store must not hold the connection.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
	defer cancel()
	_, err := s.d.Store.AppendAudit(ctx, store.Audit{
		Time:      time.Now().UTC(),
		Actor:     actor,
		Role:      role,
		Action:    pattern,
		Target:    r.URL.Path,
		Status:    status,
		RequestID: reqID,
		Detail:    detail,
	})
	if err != nil {
		s.cfg.Log.Error("audit log write failed", "err", err, "action", pattern, "actor", actor)
	}
}

// cors answers preflight requests and sets CORS headers. It reports whether
// the request was fully handled.
func (s *Server) cors(w http.ResponseWriter, r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" || len(s.cfg.CORSOrigins) == 0 {
		return false
	}
	if !slices.Contains(s.cfg.CORSOrigins, "*") && !slices.Contains(s.cfg.CORSOrigins, origin) {
		return false
	}
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", origin)
	h.Add("Vary", "Origin")
	h.Set("Access-Control-Expose-Headers", "Location, X-Request-Id")
	if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
		h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Idempotency-Key, Last-Event-ID, X-Request-Id")
		h.Set("Access-Control-Max-Age", "600")
		w.WriteHeader(http.StatusNoContent)
		return true
	}
	return false
}

// --- helpers ---

type requestIDKey struct{}
type noteKey struct{}

var requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// noteAudit adds a field to the audit entry of the current request.
func noteAudit(r *http.Request, key string, v any) {
	if m, ok := r.Context().Value(noteKey{}).(map[string]any); ok {
		m[key] = v
	}
}

func actor(r *http.Request) string {
	p, _ := auth.FromContext(r.Context())
	return p.Name
}

func isRead(method string) bool {
	return method == http.MethodGet || method == http.MethodHead || method == http.MethodOptions
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// recorder captures the status for the audit log and keeps streaming working.
type recorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (r *recorder) WriteHeader(code int) {
	if !r.wroteHeader {
		r.status, r.wroteHeader = code, true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	r.wroteHeader = true
	return r.ResponseWriter.Write(b)
}

func (r *recorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// --- errors ---

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, apiv1.Error{Error: apiv1.ErrorBody{Code: code, Message: msg}})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) planError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, plans.ErrNotFound):
		s.fail(w, r, http.StatusNotFound, "plan_not_found", "plan not found")
	case errors.Is(err, plans.ErrInvalid):
		s.fail(w, r, http.StatusBadRequest, "invalid_plan", err.Error())
	case errors.Is(err, plans.ErrWrongStatus):
		s.fail(w, r, http.StatusConflict, "plan_state_conflict", err.Error())
	default:
		s.internal(w, r, err)
	}
}

func (s *Server) runError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, runs.ErrNotFound):
		s.fail(w, r, http.StatusNotFound, "run_not_found", "run not found")
	case errors.Is(err, runs.ErrBusy):
		s.fail(w, r, http.StatusConflict, "cluster_busy", err.Error())
	case errors.Is(err, runs.ErrWrongStatus):
		s.fail(w, r, http.StatusConflict, "run_state_conflict", err.Error())
	case errors.Is(err, runs.ErrInvalid):
		s.fail(w, r, http.StatusBadRequest, "invalid_request", err.Error())
	case errors.Is(err, runs.ErrEngineInit):
		// The cause can name server paths or cluster URLs: log it, return a hint.
		s.cfg.Log.Error("engine init failed", "err", err)
		s.fail(w, r, http.StatusBadGateway, "engine_init_failed", "could not initialize the migration engine; check the plan's kube contexts and the server logs")
	default:
		s.internal(w, r, err)
	}
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, status int, code, msg string) {
	noteAudit(r, "error", code)
	writeError(w, status, code, msg)
}

// internal logs the cause and returns a generic message: engine and store
// errors can carry connection details.
func (s *Server) internal(w http.ResponseWriter, r *http.Request, err error) {
	s.cfg.Log.Error("internal error", "err", err, "path", r.URL.Path)
	s.fail(w, r, http.StatusInternalServerError, "internal", "internal error; see server logs")
}
