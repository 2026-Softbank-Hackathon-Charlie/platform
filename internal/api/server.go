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
	"strings"
	"sync"
	"time"

	specdoc "github.com/2026-Softbank-Hackathon-Charlie/platform/api"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/airun"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/apps"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/auth"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/deploy"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/github"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/plans"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/preflight/capacity"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/preflight/gate"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/preflight/paritygate"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/runs"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/secrets"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/security"
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

// Deployer runs deployments of apps registered from GitHub: it builds the
// images, commits apps/<app>/<env>.yaml to the gitops repository and follows
// Argo CD, recording progress in the store. Enqueue hands it a stored
// deployment in status queued and returns once the deployer has taken it,
// not when the deployment finishes.
type Deployer interface {
	Enqueue(ctx context.Context, deploymentID string) error
}

// GitHub reads app repositories through the platform's GitHub App
// (internal/github). An error that wraps fs.ErrNotExist means the
// repository, ref or file does not exist or the installation cannot see it;
// any other error means GitHub could not be reached.
type GitHub interface {
	// ResolveRef returns the full commit sha of a branch, tag or sha of repo (owner/name).
	ResolveRef(ctx context.Context, repo, ref string) (string, error)
	// ReadFile returns the file at path (from the repository root) at ref.
	ReadFile(ctx context.Context, repo, ref, path string) ([]byte, error)
	// ListRepos returns the repositories the installation can see.
	ListRepos(ctx context.Context) ([]github.Repo, error)
	// Org is the organization the App is installed on.
	Org() string
	// ReadRepo returns repo's ID, its current owner/name and owner (after a
	// rename or transfer) and its topics.
	ReadRepo(ctx context.Context, repo string) (github.RepoInfo, error)
	// PushToken creates a token that may only write the contents of repo,
	// bound to its ID, for an hour (ADR 0023). github.ErrNotInOrg,
	// ErrNotInInstallation and ErrUnexpectedScope (the token is already
	// revoked) say why it did not.
	PushToken(ctx context.Context, repo github.RepoInfo) (github.PushToken, error)
	// RevokeToken revokes a token PushToken created.
	RevokeToken(ctx context.Context, token string) error
}

// Config configures the API.
type Config struct {
	// Tokens authenticates programs with static tokens. When Insecure is set
	// and Tokens and Admins are both empty, every request acts as the
	// operator "dev" (local development only).
	Tokens   *auth.Tokens
	Insecure bool
	// Admins log in with a username and password (POST /v1/auth/login) and
	// approve CLI device logins; empty answers login 503.
	Admins *auth.Admins
	// ConsoleURL is the web console's address; a device login sends people
	// to ConsoleURL + "/#device". Default DefaultConsoleURL.
	ConsoleURL string
	// CORSOrigins lists browser origins allowed to call the API ("*" allows any).
	CORSOrigins []string
	Log         *slog.Logger
	// GitHubWebhookSecret checks X-Hub-Signature-256 on POST
	// /v1/webhooks/github; empty answers it 503.
	GitHubWebhookSecret string
	// AppDomain is where public services of registered apps answer, as
	// https://<app>.<AppDomain>. Default DefaultAppDomain.
	AppDomain string
	// GitOpsRepo is the gitops repository (owner/name, PLATFORM_GITOPS_REPO),
	// which never gets a push token. Default deploy.DefaultGitOpsRepo.
	GitOpsRepo string
	// AppDBTopology makes a database pair (ADR 0030) for every app with a
	// postgres resource registered in exactly two environments of
	// AppDBEnvs while Deps.Secrets is set (PLATFORM_APP_DB_TOPOLOGY=1).
	AppDBTopology bool
	// AppDBEnvs are the environments with a database host template
	// (PLATFORM_APP_DB_HOST_TEMPLATES), the only ones a pair can be
	// migrated between.
	AppDBEnvs []string
}

// DefaultAppDomain is Config.AppDomain's default.
const DefaultAppDomain = "hopcharlie.dev"

// DefaultConsoleURL is Config.ConsoleURL's default.
const DefaultConsoleURL = "https://console.hopcharlie.dev"

// Deps are the services behind the API.
type Deps struct {
	Plans *plans.Service
	Runs  *runs.Runner
	Store store.Store // audit log, idempotency keys, apps from GitHub and their deployments
	Envs  Envs
	// Traffic is nil when no gateway is configured.
	Traffic Traffic
	// TrafficApp returns the gateway client of one app's scope
	// (<gateway>/apps/<app>, ADR 0012) for GET /v1/traffic?app=. Nil refuses
	// the parameter.
	TrafficApp func(app string) Traffic
	// EnvNotFound is the error Envs.Get returns for an unknown name.
	EnvNotFound error
	// Apps drafts plans from PLATFORM_APPS_FILE; nil when the file is not set.
	Apps *apps.Catalog
	// SourceStats measures a draft's source database for POST /v1/plans/draft
	// with sourceStats (capacity.Prober); nil answers not_measured.
	SourceStats SourceMeasurer
	// Security builds the security and governance view; nil turns /v1/security off.
	Security SecurityReporter
	// ImageScans reads stored image scans for /v1/security/controls and
	// /v1/security/vulnerabilities; nil reports the scanner as not configured.
	ImageScans security.ScanResults
	// Agents is the AI agent's activity log; nil turns /v1/agent off.
	Agents AgentLog
	// Deployer runs deployments; nil leaves them queued.
	Deployer Deployer
	// Remover removes registered apps and their added instances (DELETE
	// /v1/apps/{name} and /v1/apps/{name}/instances/{instance}); nil answers 503.
	Remover AppRemover
	// GitHub reads app repositories; nil answers the endpoints that need it 503 github_not_configured.
	GitHub GitHub
	// Secrets encrypts the app secrets (PLATFORM_SECRETS_KEY, ADR 0019); nil
	// answers the secrets endpoints 503 secrets_not_configured.
	Secrets *secrets.Cipher
	// StorageConfigured reports whether an environment has a storage
	// driver (ADR 0016); nil means none has. Registering an app with an s3
	// resource for an environment without one answers 503
	// storage_not_configured.
	StorageConfigured func(env string) bool
	// Buckets inspects the residency policy's buckets (gate.Gate); nil
	// answers POST /v1/residency/buckets/inspect 503 residency_not_configured.
	Buckets BucketInspector
	// AIRuns starts the AI runner as a Job (airun.Runner); nil answers the
	// /v1/ai-runs endpoints 503 ai_runner_not_configured.
	AIRuns AIRunner
	// DBRoutes makes and removes the management cluster's routes to the
	// databases of registered apps (dbroute.Manager, ADR 0030); nil when
	// they are made by hand.
	DBRoutes AppDBRoutes
}

// AppDBRoutes starts making the routes of a database pair just registered
// and removing those of an app just deleted. Both return at once; failures
// are logged and retried by the manager.
type AppDBRoutes interface {
	Added(d store.AppDatabase)
	Removed(app string)
}

// AIRunner starts and lists AI runner Jobs (airun.Runner).
type AIRunner interface {
	Start(ctx context.Context, req airun.Request, actor string) (airun.Run, error)
	List(ctx context.Context, app string) ([]airun.Run, error)
}

// BucketInspector measures a registered bucket next to its policy entry.
type BucketInspector interface {
	Inspect(ctx context.Context, bucket, prefix string) (gate.Inspection, error)
}

// SourceMeasurer reads a source database's numbers (capacity.Prober). ok is
// false when it could not; the reason is not returned, so nothing about the
// DSN reaches the response.
type SourceMeasurer interface {
	MeasureSource(ctx context.Context, dsn string) (stats capacity.SourceStats, ok bool)
}

// Server is the HTTP handler.
type Server struct {
	cfg Config
	d   Deps
	mux *http.ServeMux

	startMu sync.Mutex // one start at a time keeps idempotency and plan reservation consistent

	now        func() time.Time   // the clock for sessions, device logins and the login limiter
	logins     *auth.LoginLimiter // failed logins per username
	loginSlots chan struct{}      // one token per bcrypt comparison in progress
}

// New wires the routes.
func New(cfg Config, d Deps) *Server {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Tokens == nil {
		cfg.Tokens = &auth.Tokens{}
	}
	if cfg.AppDomain == "" {
		cfg.AppDomain = DefaultAppDomain
	}
	if cfg.GitOpsRepo == "" {
		cfg.GitOpsRepo = deploy.DefaultGitOpsRepo
	}
	if cfg.Admins == nil {
		cfg.Admins = &auth.Admins{}
	}
	if cfg.ConsoleURL == "" {
		cfg.ConsoleURL = DefaultConsoleURL
	}
	cfg.ConsoleURL = strings.TrimRight(cfg.ConsoleURL, "/")
	s := &Server{cfg: cfg, d: d, mux: http.NewServeMux(), now: storeTime,
		logins:     auth.NewLoginLimiter(loginMaxFailures, loginWindow, loginMaxUsernames),
		loginSlots: make(chan struct{}, loginConcurrency)}
	if d.Runs != nil {
		// The observe phase rolls back through the same path as the endpoint.
		d.Runs.SetAutoRollback(s.autoRollback)
	}
	viewer, agent, operator := apiv1.RoleViewer, apiv1.RoleAgent, apiv1.RoleOperator

	s.route("GET /healthz", "", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, apiv1.Health{Status: "ok"})
	})
	s.route("GET /openapi.yaml", "", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		_, _ = w.Write(specdoc.OpenAPI)
	})
	s.route("GET /v1/me", viewer, s.me)
	// Login and the CLI's side of a device login take no token; the
	// handlers audit what matters themselves (handlers_auth.go).
	s.route("POST /v1/auth/login", "", s.login)
	s.route("POST /v1/auth/logout", viewer, s.logout)
	s.route("POST /v1/auth/device", "", s.startDeviceLogin)
	s.route("POST /v1/auth/device/token", "", s.pollDeviceLogin)
	s.route("GET /v1/auth/device/{userCode}", operator, s.getDeviceLogin)
	s.route("POST /v1/auth/device/{userCode}/approve", operator, s.approveDeviceLogin)
	s.route("POST /v1/auth/device/{userCode}/deny", operator, s.denyDeviceLogin)
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
	s.route("POST /v1/written-records/{stream}", agent, s.appendWrittenRecords)
	s.route("GET /v1/written-records/{stream}", viewer, s.writtenRecordStats)
	s.route("PUT /v1/plans/{ref}/attachments/{name}", agent, s.putPlanAttachment)
	s.route("GET /v1/plans/{ref}/attachments/{name}", viewer, s.getPlanAttachment)
	s.route("GET /v1/apps", viewer, s.listApps)
	s.route("POST /v1/plans/draft", agent, s.draftPlan)
	s.route("GET /v1/ai-runs", viewer, s.listAIRuns)
	s.route("POST /v1/ai-runs", operator, s.startAIRun)
	s.route("POST /v1/residency/buckets/inspect", agent, s.inspectBucket)
	s.route("POST /v1/migrations/{runId}/rollback", operator, s.rollbackMigration)
	s.route("GET /{$}", "", s.root)
	s.route("GET /v1/security", viewer, s.security)
	s.route("GET /v1/security/ocsf", viewer, s.securityOCSF)
	s.route("GET /v1/security/controls", viewer, s.securityControls)
	s.route("GET /v1/security/vulnerabilities", viewer, s.securityVulnerabilities)
	// The image scan workflow posts what Grype found; the API only stores it (SEC-04).
	s.route("POST /v1/security/scans", agent, s.submitSecurityScan)
	s.route("POST /v1/agent/sessions", agent, s.createAgentSession)
	s.route("GET /v1/agent/sessions", viewer, s.listAgentSessions)
	s.route("GET /v1/agent/sessions/{id}", viewer, s.getAgentSession)
	s.route("POST /v1/agent/sessions/{id}/events", agent, s.appendAgentEvent)
	s.route("GET /v1/agent/sessions/{id}/events", viewer, s.agentEvents)
	s.route("POST /v1/agent/sessions/{id}/finish", agent, s.finishAgentSession)
	s.route("POST /v1/apps", operator, s.createApp)
	s.route("GET /v1/apps/{name}", viewer, s.getApp)
	s.route("POST /v1/apps/validate", viewer, s.validateAppSpec)
	s.route("GET /v1/github/repos", viewer, s.listGitHubRepos)
	s.route(pushTokenRoute, agent, s.createPushToken)
	// GitHub signs the webhook instead of sending a token; the handler
	// checks the signature and writes its own audit entries.
	s.route("POST /v1/webhooks/github", "", s.githubWebhook)
	s.route("DELETE /v1/apps/{name}", operator, s.deleteApp)
	s.route("POST /v1/apps/{name}/instances", operator, s.createAppInstance)
	s.route("DELETE /v1/apps/{name}/instances/{instance}", operator, s.deleteAppInstance)
	s.route("POST /v1/apps/{name}/deployments", operator, s.createDeployment)
	s.route("GET /v1/apps/{name}/deployments", viewer, s.listDeployments)
	s.route("GET /v1/deployments/{id}", viewer, s.getDeployment)
	s.route("GET /v1/deployments/{id}/events", viewer, s.deploymentEvents)
	s.route("GET /v1/apps/{name}/secrets", viewer, s.listAppSecrets)
	s.route("PUT /v1/apps/{name}/secrets", operator, s.updateAppSecrets)
	s.route(revealSecretRoute, operator, s.getAppSecret)
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
		audited := false // set by auditBeforeAnswer
		ctx := context.WithValue(r.Context(), noteKey{}, note)
		ctx = context.WithValue(ctx, auditedKey{}, &audited)

		var p auth.Principal
		if need != "" {
			var ok bool
			var err error
			p, ok, err = s.authenticate(r)
			switch {
			case err != nil:
				s.internal(rec, r.WithContext(ctx), err)
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
		if need != "" && !audited && shouldAudit(pattern, r.Method, rec.status) {
			s.audit(r, p, pattern, rec.status, note)
		}
	})
}

// authenticate checks the bearer token: a static token from PLATFORM_TOKENS
// first, then a console session or CLI token in the store. The principal
// says which kind it was. err is set only when the store could not be read.
func (s *Server) authenticate(r *http.Request) (auth.Principal, bool, error) {
	if s.cfg.Tokens.Len() == 0 && s.cfg.Admins.Len() == 0 && s.cfg.Insecure {
		// Local development may do everything, approving device logins too.
		return auth.Principal{Name: "dev", Role: apiv1.RoleOperator, Kind: auth.KindConsole}, true, nil
	}
	if p, ok := s.cfg.Tokens.Authenticate(r); ok {
		p.Kind = auth.KindToken
		return p, true, nil
	}
	raw, ok := auth.BearerToken(r)
	if !ok || !strings.HasPrefix(raw, auth.SessionPrefix) {
		return auth.Principal{}, false, nil
	}
	sess, err := s.d.Store.GetSession(r.Context(), auth.HashSecret(raw), s.now())
	switch {
	case errors.Is(err, store.ErrNotFound): // unknown, expired or logged out
		return auth.Principal{}, false, nil
	case err != nil:
		return auth.Principal{}, false, err
	}
	// A session lives only as long as its administrator: removing a name
	// from PLATFORM_ADMINS and restarting ends their console sessions and
	// every CLI token they approved.
	if !s.cfg.Admins.Has(sess.Name) {
		return auth.Principal{}, false, nil
	}
	return auth.Principal{Name: sess.Name, Role: apiv1.Role(sess.Role), Kind: sess.Kind, Client: sess.Client}, true, nil
}

// audit writes one audit entry and returns the store's error, which it also
// logs. Most callers write it after the response, and the error only goes
// to the log; getAppSecret writes it before the value and refuses on error.
func (s *Server) audit(r *http.Request, p auth.Principal, pattern string, status int, note map[string]any) error {
	actor, role := p.Name, string(p.Role)
	if actor == "" {
		actor, role = "anonymous", ""
	}
	if p.Kind == auth.KindCLI {
		// The actor is the administrator who approved the CLI token; via
		// names the program that used it (DeviceLogin.client).
		if note == nil {
			note = map[string]any{}
		}
		note["via"] = p.Client
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
		Target:    auditTarget(r, pattern),
		Status:    status,
		RequestID: reqID,
		Detail:    detail,
	})
	if err != nil {
		s.cfg.Log.Error("audit log write failed", "err", err, "action", pattern, "actor", actor)
	}
	return err
}

// auditBeforeAnswer writes the audit entry of the current request now, with
// the status the handler is about to send, for a handler that must not
// answer when the entry cannot be written. When it is written, the route
// writes no second entry; when it is not, the handler answers an error,
// which the route records as usual.
func (s *Server) auditBeforeAnswer(r *http.Request, pattern string, status int) error {
	p, _ := auth.FromContext(r.Context())
	note, _ := r.Context().Value(noteKey{}).(map[string]any)
	if err := s.audit(r, p, pattern, status, note); err != nil {
		return err
	}
	if done, ok := r.Context().Value(auditedKey{}).(*bool); ok {
		*done = true
	}
	return nil
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
	h.Set("Access-Control-Expose-Headers", "Location, Retry-After, X-Request-Id")
	if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
		h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
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
type auditedKey struct{}

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

// shouldAudit records state changes and every denied request. Written-record
// batches are excluded unless denied: a load generator sends them every few
// seconds, and they are data, not decisions an auditor needs to see. Reading
// a secret's value is a read here: getAppSecret records every call it
// answers itself, before it sends the value.
func shouldAudit(pattern, method string, status int) bool {
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return true
	}
	return !isRead(method) && pattern != "POST /v1/written-records/{stream}"
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
	case errors.Is(err, plans.ErrRehearsal):
		s.fail(w, r, http.StatusConflict, "rehearsal_required", err.Error())
	case errors.Is(err, plans.ErrResidencyDenied):
		// The verdict holds fixed codes and validated identifiers only.
		var denied *plans.ResidencyDenied
		if errors.As(err, &denied) {
			noteAudit(r, "residencyDigest", denied.Verdict.PolicyDigest)
			if denied.Verdict.Content != nil {
				noteAudit(r, "residencyContent", denied.Verdict.Content)
			}
			noteAudit(r, "residencyReasons", plans.ReasonTexts(denied.Verdict.Reasons))
			if len(denied.Verdict.Buckets) > 0 {
				noteAudit(r, "residencyBuckets", plans.BucketEvidence(denied.Verdict.Buckets))
			}
		}
		s.fail(w, r, http.StatusConflict, "residency_denied", err.Error())
	case errors.Is(err, plans.ErrCapacityInsufficient):
		// The report holds measured numbers only, never addresses.
		var short *plans.CapacityInsufficient
		if errors.As(err, &short) {
			noteAudit(r, "capacity", capacityNote(&short.Report))
		}
		s.fail(w, r, http.StatusConflict, "capacity_insufficient", err.Error())
	case errors.Is(err, plans.ErrVersionMismatch):
		// The result holds versions and an image name only.
		var mm *plans.VersionMismatch
		if errors.As(err, &mm) {
			noteAudit(r, "pgVersion", mm.Result)
		}
		s.fail(w, r, http.StatusConflict, "version_mismatch", err.Error())
	default:
		s.internal(w, r, err)
	}
}

// noteCapacity records what the capacity gate measured for a started plan.
func noteCapacity(r *http.Request, c plans.CapacityCheck) {
	switch {
	case c.Report != nil:
		noteAudit(r, "capacity", capacityNote(c.Report))
	case c.Outcome != "":
		noteAudit(r, "capacity", map[string]any{"outcome": c.Outcome})
	}
}

// noteVersion records what the PostgreSQL major version gate decided for a
// started plan.
func noteVersion(r *http.Request, c plans.VersionCheck) {
	switch {
	case c.Result != nil:
		noteAudit(r, "pgVersion", *c.Result)
	case c.Outcome != "":
		noteAudit(r, "pgVersion", map[string]any{"status": c.Outcome})
	}
}

// noteParity records what the parity probes found for a started plan. It is
// a record only (audit mode): passed=false here never stopped the run.
func noteParity(r *http.Request, c plans.ParityCheck) {
	switch {
	case c.Report != nil:
		noteAudit(r, "parity", map[string]any{"outcome": c.Outcome, "mode": "audit", "passed": c.Report.Passed, "results": paritygate.Lines(*c.Report), "binding": c.Binding, "configDigest": c.Report.ConfigDigest, "baselineStatus": c.Report.BaselineStatus, "observedAt": c.Report.ObservedAt, "expiresAt": c.Report.ExpiresAt})
	case c.Outcome != "":
		noteAudit(r, "parity", map[string]any{"outcome": c.Outcome, "mode": "audit", "binding": c.Binding})
	}
}

// capacityNote is the audit form of a capacity report: each check's status,
// code and message, and the raw measurements.
func capacityNote(rep *capacity.Report) map[string]any {
	checks := make([]string, 0, len(rep.Checks))
	for _, c := range rep.Checks {
		checks = append(checks, c.ID+" "+string(c.Status)+" "+c.Code+": "+c.Message)
	}
	return map[string]any{"allowed": rep.Allowed, "checks": checks, "measurements": rep.Measurements}
}

// noteResidency records what the residency gate decided for a started plan.
func noteResidency(r *http.Request, c plans.ResidencyCheck) {
	if c.Outcome == "" {
		return
	}
	noteAudit(r, "residency", c.Outcome)
	if c.Content != nil {
		noteAudit(r, "residencyContent", c.Content)
	}
	if c.PolicyDigest != "" {
		noteAudit(r, "residencyDigest", c.PolicyDigest)
	}
	if len(c.Buckets) > 0 {
		noteAudit(r, "residencyBuckets", plans.BucketEvidence(c.Buckets))
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
