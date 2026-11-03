package api

import (
	"cmp"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/auth"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/deploy"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/appspec"
)

// reservedAppNames are served by the platform itself under the app domain
// (the API, the console and the gateway's default app, ADR 0012), so no
// registered app may take them.
var reservedAppNames = []string{"api", "console", "app"}

// appStatusDeployments is how many of an app's newest deployments its
// per-environment status is read from.
const appStatusDeployments = 50

// maxWebhookBytes is GitHub's own cap on a webhook payload, so no delivery
// GitHub sends is refused for its size.
const maxWebhookBytes = 25 << 20

var (
	repoPattern   = regexp.MustCompile(`^[A-Za-z0-9-]+/[A-Za-z0-9._-]+$`)
	branchPattern = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,200}$`)
	commitPattern = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)
)

// validBranch accepts the branch names git accepts, limited to letters,
// digits and . _ / -.
func validBranch(b string) bool {
	return branchPattern.MatchString(b) && !strings.HasPrefix(b, "-") && !strings.HasPrefix(b, "/") &&
		!strings.HasSuffix(b, "/") && !strings.HasSuffix(b, ".") && !strings.HasSuffix(b, ".lock") &&
		!strings.Contains(b, "..") && !strings.Contains(b, "//") && !strings.Contains(b, "/.") && !strings.HasPrefix(b, ".")
}

// --- apps ---

// POST /v1/apps: register an app from GitHub and queue its first deployment.
func (s *Server) createApp(w http.ResponseWriter, r *http.Request) {
	var req apiv1.AppCreateRequest
	if !s.decode(w, r, &req, true) {
		return
	}
	branch := "main"
	if req.Branch != nil {
		branch = *req.Branch
	}
	autoDeploy := req.AutoDeploy == nil || *req.AutoDeploy
	noteAudit(r, "repo", req.Repo)
	noteAudit(r, "branch", branch)
	noteAudit(r, "environments", req.Environments)
	if !repoPattern.MatchString(req.Repo) {
		s.fail(w, r, http.StatusBadRequest, "invalid_request", "repo must be owner/name")
		return
	}
	if !validBranch(branch) {
		s.fail(w, r, http.StatusBadRequest, "invalid_request", "branch must be a git branch name of letters, digits and . _ / -")
		return
	}
	targets, msg := s.targetEnvs(req.Environments)
	if msg != "" {
		s.fail(w, r, http.StatusBadRequest, "invalid_request", msg)
		return
	}
	if s.d.GitHub == nil {
		s.githubNotConfigured(w, r)
		return
	}

	ctx := r.Context()
	commit, err := s.d.GitHub.ResolveRef(ctx, req.Repo, branch)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		s.appSpecInvalid(w, r, refNotFound(req.Repo, branch))
		return
	case err != nil:
		s.githubUnreachable(w, r, err)
		return
	}
	noteAudit(r, "commit", commit)
	v, spec, err := deploy.CheckSpec(ctx, s.d.GitHub, req.Repo, commit)
	if err != nil {
		s.githubUnreachable(w, r, err)
		return
	}
	if spec == nil {
		s.appSpecInvalid(w, r, v)
		return
	}
	noteAudit(r, "app", spec.Name)
	if slices.Contains(reservedAppNames, spec.Name) {
		s.fail(w, r, http.StatusConflict, "app_exists", fmt.Sprintf("the platform serves %s.%s itself; rename the app in %s", spec.Name, s.cfg.AppDomain, appspec.FileName))
		return
	}
	specJSON, err := json.Marshal(spec)
	if err != nil {
		s.internal(w, r, err)
		return
	}

	// Registered means deployed: finish even if the caller goes away.
	ctx = context.WithoutCancel(ctx)
	now := storeTime()
	app := store.App{
		Name: spec.Name, Repo: req.Repo, Branch: branch, Environments: targets, AutoDeploy: autoDeploy,
		Spec: specJSON, SpecCommit: commit, CreatedBy: actor(r), CreatedAt: now,
	}
	if err := s.d.Store.CreateApp(ctx, app); err != nil {
		if errors.Is(err, store.ErrConflict) {
			s.fail(w, r, http.StatusConflict, "app_exists", fmt.Sprintf("an app named %s is already registered", spec.Name))
			return
		}
		s.internal(w, r, err)
		return
	}
	d := newDeployment(app, branch, commit, apiv1.DeploymentTriggerRegister, actor(r), now)
	if err := s.d.Store.CreateDeployment(ctx, d); err != nil {
		s.internal(w, r, err)
		return
	}
	noteAudit(r, "deployment", d.ID)
	s.enqueue(ctx, d.ID)
	w.Header().Set("Location", "/v1/apps/"+app.Name)
	writeJSON(w, http.StatusCreated, apiv1.AppCreated{App: s.appView(app, []store.Deployment{d}), Deployment: deploymentJSON(d)})
}

// targetEnvs checks the environments of a registration and returns them in
// PLATFORM_ENVS_FILE order, or a message for a 400.
func (s *Server) targetEnvs(names []string) ([]string, string) {
	if len(names) == 0 {
		return nil, "environments must name at least one environment"
	}
	known := s.d.Envs.Names()
	seen := map[string]bool{}
	for _, n := range names {
		switch {
		case seen[n]:
			return nil, fmt.Sprintf("environment %q is listed twice", n)
		case !slices.Contains(known, n):
			return nil, fmt.Sprintf("unknown environment %q; this server has %s", n, strings.Join(known, ", "))
		}
		seen[n] = true
	}
	out := make([]string, 0, len(names))
	for _, n := range known {
		if seen[n] {
			out = append(out, n)
		}
	}
	return out, ""
}

// GET /v1/apps: the apps file in its order, then the apps only registered
// from GitHub by name. An app in both is the file's entry with deploy set.
func (s *Server) listApps(w http.ResponseWriter, r *http.Request) {
	list := []apiv1.App{}
	if s.d.Apps != nil {
		list = s.d.Apps.List()
	}
	registered, err := s.d.Store.ListApps(r.Context())
	if err != nil {
		s.internal(w, r, err)
		return
	}
	inFile := map[string]int{}
	for i, a := range list {
		inFile[a.Name] = i
	}
	for _, a := range registered {
		deps, err := s.d.Store.ListDeployments(r.Context(), a.Name, appStatusDeployments)
		if err != nil {
			s.internal(w, r, err)
			return
		}
		deploy := s.appDeploy(a, deps)
		if i, ok := inFile[a.Name]; ok {
			list[i].Deploy = &deploy
			continue
		}
		list = append(list, registeredOnly(a.Name, &deploy))
	}
	writeJSON(w, http.StatusOK, apiv1.AppList{Apps: list})
}

// GET /v1/apps/{name}: one app, as GET /v1/apps lists it.
func (s *Server) getApp(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	file, inFile := s.fileApp(name)
	a, err := s.d.Store.GetApp(r.Context(), name)
	switch {
	case errors.Is(err, store.ErrNotFound):
		if !inFile {
			s.fail(w, r, http.StatusNotFound, "app_not_found", "app not found")
			return
		}
		writeJSON(w, http.StatusOK, file)
		return
	case err != nil:
		s.internal(w, r, err)
		return
	}
	deps, err := s.d.Store.ListDeployments(r.Context(), name, appStatusDeployments)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, s.appView(a, deps))
}

// fileApp returns the app of PLATFORM_APPS_FILE with the name.
func (s *Server) fileApp(name string) (apiv1.App, bool) {
	if s.d.Apps == nil {
		return apiv1.App{}, false
	}
	for _, a := range s.d.Apps.List() {
		if a.Name == name {
			return a, true
		}
	}
	return apiv1.App{}, false
}

// appView is a registered app as the API shows it: the apps file's entry
// when the file has the name, otherwise the defaults, with deploy set.
// deps are its newest deployments, newest first.
func (s *Server) appView(a store.App, deps []store.Deployment) apiv1.App {
	deploy := s.appDeploy(a, deps)
	if out, ok := s.fileApp(a.Name); ok {
		out.Deploy = &deploy
		return out
	}
	return registeredOnly(a.Name, &deploy)
}

// registeredOnly is a registered app that is not in the apps file: its
// namespace is its name and its database the CNPG defaults of the app chart.
func registeredOnly(name string, deploy *apiv1.AppDeploy) apiv1.App {
	return apiv1.App{
		Name: name, Namespace: name,
		Database:     apiv1.AppDatabase{Name: "app", User: "app", Schemas: []string{}},
		Environments: []apiv1.AppEnvironment{},
		Deploy:       deploy,
	}
}

// appDeploy is a registered app's deploy section. deps are its newest
// deployments, newest first; each environment shows the newest one that
// reached it. instances holds the default instance only, built from the
// same fields.
func (s *Server) appDeploy(a store.App, deps []store.Deployment) apiv1.AppDeploy {
	var spec apiv1.AppSpec
	if err := json.Unmarshal(a.Spec, &spec); err != nil {
		s.cfg.Log.Error("stored app spec does not decode", "app", a.Name, "err", err)
	}
	if spec.Services == nil {
		spec.Services = []apiv1.AppSpecService{}
	}
	out := apiv1.AppDeploy{
		Repo: a.Repo, Branch: a.Branch, Environments: a.Environments, AutoDeploy: a.AutoDeploy,
		Spec: spec, SpecCommit: a.SpecCommit, CreatedAt: a.CreatedAt, CreatedBy: a.CreatedBy,
		Status: make([]apiv1.AppEnvironmentStatus, 0, len(a.Environments)),
	}
	if slices.ContainsFunc(spec.Services, func(sv apiv1.AppSpecService) bool { return sv.Public }) {
		u := "https://" + a.Name + "." + s.cfg.AppDomain
		out.Url = &u
	}
	if len(deps) > 0 {
		last := deploymentJSON(deps[0])
		out.LastDeployment = &last
	}
	for _, env := range a.Environments {
		st := apiv1.AppEnvironmentStatus{Env: env, Status: apiv1.DeploymentEnvironmentStatusPending}
	newest:
		for _, d := range deps {
			for _, e := range d.Environments {
				if e.Env != env || e.Status == string(apiv1.DeploymentEnvironmentStatusPending) {
					continue
				}
				commit, at := d.Commit, d.CreatedAt
				if d.FinishedAt != nil {
					at = *d.FinishedAt
				} else if d.StartedAt != nil {
					at = *d.StartedAt
				}
				st.Status = apiv1.DeploymentEnvironmentStatus(e.Status)
				st.SyncStatus, st.HealthStatus, st.Url = optional(e.SyncStatus), optional(e.HealthStatus), optional(e.URL)
				st.Commit, st.UpdatedAt = &commit, &at
				break newest
			}
		}
		out.Status = append(out.Status, st)
	}
	out.Instances = []apiv1.AppInstance{{
		Name: a.Name, Branch: out.Branch, Default: true, AutoDeploy: out.AutoDeploy,
		Spec: out.Spec, SpecCommit: out.SpecCommit, Url: out.Url, CreatedAt: out.CreatedAt, CreatedBy: out.CreatedBy,
		LastDeployment: out.LastDeployment, Status: out.Status,
	}}
	return out
}

// --- platform.yaml checks ---

// POST /v1/apps/validate: check platform.yaml from a repository branch or
// as given, without registering anything.
func (s *Server) validateAppSpec(w http.ResponseWriter, r *http.Request) {
	var req apiv1.AppSpecValidateRequest
	if !s.decode(w, r, &req, true) {
		return
	}
	if (req.Repo == nil) == (req.Spec == nil) || (req.Spec != nil && req.Branch != nil) {
		s.fail(w, r, http.StatusBadRequest, "invalid_request", "send either repo (with branch) or spec")
		return
	}
	if req.Spec != nil {
		if len(*req.Spec) > appspec.MaxSize {
			s.fail(w, r, http.StatusRequestEntityTooLarge, "too_large", fmt.Sprintf("spec exceeds %d bytes", appspec.MaxSize))
			return
		}
		spec, errs := appspec.Check([]byte(*req.Spec))
		writeJSON(w, http.StatusOK, validation(spec, errs, nil))
		return
	}

	repo, branch := *req.Repo, "main"
	if req.Branch != nil {
		branch = *req.Branch
	}
	noteAudit(r, "repo", repo)
	noteAudit(r, "branch", branch)
	if !repoPattern.MatchString(repo) {
		s.fail(w, r, http.StatusBadRequest, "invalid_request", "repo must be owner/name")
		return
	}
	if !validBranch(branch) {
		s.fail(w, r, http.StatusBadRequest, "invalid_request", "branch must be a git branch name of letters, digits and . _ / -")
		return
	}
	if s.d.GitHub == nil {
		s.githubNotConfigured(w, r)
		return
	}
	commit, err := s.d.GitHub.ResolveRef(r.Context(), repo, branch)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		writeJSON(w, http.StatusOK, refNotFound(repo, branch))
		return
	case err != nil:
		s.githubUnreachable(w, r, err)
		return
	}
	v, _, err := deploy.CheckSpec(r.Context(), s.d.GitHub, repo, commit)
	if err != nil {
		s.githubUnreachable(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// validation is the API form of a check's result.
func validation(spec *appspec.Spec, errs []appspec.FieldError, commit *string) apiv1.AppSpecValidation {
	v := apiv1.AppSpecValidation{Valid: len(errs) == 0, Commit: commit, Errors: make([]apiv1.AppSpecFieldError, len(errs))}
	for i, e := range errs {
		v.Errors[i] = apiv1.AppSpecFieldError{Path: e.Path, Message: e.Message}
	}
	if spec != nil {
		// The spec's JSON is the API's AppSpec.
		b, _ := json.Marshal(spec)
		var out apiv1.AppSpec
		_ = json.Unmarshal(b, &out)
		v.Spec = &out
	}
	return v
}

// refNotFound is the check result for a repository or branch GitHub does
// not have, or the installation cannot see.
func refNotFound(repo, branch string) apiv1.AppSpecValidation {
	msg := fmt.Sprintf("repository %s has no branch %s, or the GitHub App installation cannot see the repository", repo, branch)
	return validation(nil, []appspec.FieldError{{Message: msg}}, nil)
}

// appSpecInvalid answers 422 invalid_app_spec with the check's result.
func (s *Server) appSpecInvalid(w http.ResponseWriter, r *http.Request, v apiv1.AppSpecValidation) {
	noteAudit(r, "error", "invalid_app_spec")
	msg := appspec.FileName + " is invalid"
	if len(v.Errors) > 0 {
		e := appspec.FieldError{Path: v.Errors[0].Path, Message: v.Errors[0].Message}
		msg += ": " + e.Error()
		if n := len(v.Errors) - 1; n > 0 {
			msg += fmt.Sprintf(" (and %d more)", n)
		}
	}
	writeJSON(w, http.StatusUnprocessableEntity, apiv1.AppSpecInvalid{Error: apiv1.ErrorBody{Code: "invalid_app_spec", Message: msg}, Validation: v})
}

// --- GitHub ---

// GET /v1/github/repos: the repositories the installation can see.
func (s *Server) listGitHubRepos(w http.ResponseWriter, r *http.Request) {
	if s.d.GitHub == nil {
		s.githubNotConfigured(w, r)
		return
	}
	repos, err := s.d.GitHub.ListRepos(r.Context())
	if err != nil {
		s.githubUnreachable(w, r, err)
		return
	}
	out := apiv1.GitHubRepoList{Repos: make([]apiv1.GitHubRepo, len(repos))}
	for i, g := range repos {
		out.Repos[i] = apiv1.GitHubRepo{FullName: g.FullName, Private: g.Private, DefaultBranch: g.DefaultBranch, HtmlUrl: g.HTMLURL}
	}
	slices.SortFunc(out.Repos, func(a, b apiv1.GitHubRepo) int { return strings.Compare(a.FullName, b.FullName) })
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) githubNotConfigured(w http.ResponseWriter, r *http.Request) {
	s.fail(w, r, http.StatusServiceUnavailable, "github_not_configured", "this server has no GitHub App configured (PLATFORM_GITHUB_APP_ID and PLATFORM_GITHUB_APP_PRIVATE_KEY)")
}

// githubUnreachable logs the cause and answers 502 github_unreachable.
func (s *Server) githubUnreachable(w http.ResponseWriter, r *http.Request, err error) {
	s.cfg.Log.Warn("github", "err", err, "path", r.URL.Path)
	s.fail(w, r, http.StatusBadGateway, "github_unreachable", "could not read from GitHub through the App; see the server logs")
}

// --- webhook ---

// pushEvent is the part of a GitHub push payload the platform reads.
type pushEvent struct {
	Ref        string `json:"ref"`
	After      string `json:"after"`
	Deleted    bool   `json:"deleted"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
	Pusher struct {
		Name string `json:"name"`
	} `json:"pusher"`
	Sender struct {
		Login string `json:"login"`
	} `json:"sender"`
}

// POST /v1/webhooks/github. The route takes no token, so it is audited
// here: a push that queued deployments as actor "github", and a bad
// signature as "anonymous". The payload itself is never recorded.
func (s *Server) githubWebhook(w http.ResponseWriter, r *http.Request) {
	status := s.handleWebhook(w, r)
	if status != http.StatusAccepted && status != http.StatusUnauthorized {
		return
	}
	var p auth.Principal
	if status == http.StatusAccepted {
		p.Name = "github"
	}
	note, _ := r.Context().Value(noteKey{}).(map[string]any)
	s.audit(r, p, "POST /v1/webhooks/github", status, note)
}

// handleWebhook answers the webhook and returns the status it sent.
func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) int {
	fail := func(status int, code, msg string) int {
		s.fail(w, r, status, code, msg)
		return status
	}
	if s.cfg.GitHubWebhookSecret == "" {
		return fail(http.StatusServiceUnavailable, "github_not_configured", "this server has no GitHub webhook secret (PLATFORM_GITHUB_WEBHOOK_SECRET)")
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBytes))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return fail(http.StatusRequestEntityTooLarge, "too_large", fmt.Sprintf("body exceeds %d bytes", maxWebhookBytes))
		}
		return fail(http.StatusBadRequest, "invalid_request", "could not read body")
	}
	if !validSignature(s.cfg.GitHubWebhookSecret, body, r.Header.Get("X-Hub-Signature-256")) {
		return fail(http.StatusUnauthorized, "invalid_signature", "X-Hub-Signature-256 is missing or does not match the body")
	}

	event, delivery := r.Header.Get("X-GitHub-Event"), r.Header.Get("X-GitHub-Delivery")
	noteAudit(r, "event", event)
	noteAudit(r, "delivery", delivery)
	if event != "push" {
		w.WriteHeader(http.StatusNoContent)
		return http.StatusNoContent
	}
	if delivery == "" {
		return fail(http.StatusBadRequest, "invalid_request", "X-GitHub-Delivery is required")
	}
	var push pushEvent
	if err := json.Unmarshal(body, &push); err != nil || push.Ref == "" || push.Repository.FullName == "" {
		return fail(http.StatusBadRequest, "invalid_request", "the body is not a GitHub push payload")
	}
	branch, isBranch := strings.CutPrefix(push.Ref, "refs/heads/")
	if !isBranch || push.Deleted || !commitPattern.MatchString(push.After) || strings.Trim(push.After, "0") == "" {
		w.WriteHeader(http.StatusNoContent) // a tag, or a deleted branch
		return http.StatusNoContent
	}
	noteAudit(r, "repo", push.Repository.FullName)
	noteAudit(r, "ref", push.Ref)
	noteAudit(r, "commit", push.After)

	// GitHub gives up after 10 seconds; what it sent is still handled.
	ctx := context.WithoutCancel(r.Context())
	now := storeTime()
	fresh, err := s.d.Store.RecordWebhookDelivery(ctx, delivery, now)
	if err != nil {
		s.internal(w, r, err)
		return http.StatusInternalServerError
	}
	if !fresh {
		w.WriteHeader(http.StatusNoContent) // a redelivery
		return http.StatusNoContent
	}
	apps, err := s.d.Store.ListApps(ctx)
	if err != nil {
		s.internal(w, r, err)
		return http.StatusInternalServerError
	}
	by := "github:" + cmp.Or(push.Sender.Login, push.Pusher.Name, "unknown")
	out := apiv1.DeploymentList{Deployments: []apiv1.Deployment{}}
	var ids []string
	for _, a := range apps {
		if !a.AutoDeploy || a.Branch != branch || !strings.EqualFold(a.Repo, push.Repository.FullName) {
			continue
		}
		d := newDeployment(a, branch, push.After, apiv1.DeploymentTriggerPush, by, now)
		if err := s.d.Store.CreateDeployment(ctx, d); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				continue // unregistered meanwhile
			}
			s.internal(w, r, err)
			return http.StatusInternalServerError
		}
		s.enqueue(ctx, d.ID)
		out.Deployments = append(out.Deployments, deploymentJSON(d))
		ids = append(ids, d.ID)
	}
	if len(ids) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return http.StatusNoContent
	}
	noteAudit(r, "deployments", ids)
	writeJSON(w, http.StatusAccepted, out)
	return http.StatusAccepted
}

// validSignature checks X-Hub-Signature-256 ("sha256=<hex HMAC>") over the
// raw body in constant time.
func validSignature(secret string, body []byte, header string) bool {
	sig, ok := strings.CutPrefix(header, "sha256=")
	if !ok {
		return false
	}
	got, err := hex.DecodeString(sig)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil))
}

// --- deployments ---

// storeTime is now at the store's precision (PostgreSQL keeps
// microseconds), so an answer shows the times a later read returns.
func storeTime() time.Time { return time.Now().UTC().Truncate(time.Microsecond) }

// newDeploymentID is "dep_" and 12 hex digits.
func newDeploymentID() string { return "dep_" + randomHex(6) }

// newDeployment is a queued deployment of commit into a's default instance,
// to all of a's environments.
func newDeployment(a store.App, ref, commit string, trigger apiv1.DeploymentTrigger, by string, now time.Time) store.Deployment {
	envs := make([]store.DeploymentEnvironment, len(a.Environments))
	for i, e := range a.Environments {
		envs[i] = store.DeploymentEnvironment{Env: e, Status: string(apiv1.DeploymentEnvironmentStatusPending)}
	}
	return store.Deployment{
		ID: newDeploymentID(), App: a.Name, Branch: a.Branch, Instance: a.Name, Ref: ref, Commit: commit, Trigger: string(trigger), TriggeredBy: by,
		Status: store.DeployQueued, Images: []store.DeploymentImage{}, Environments: envs, CreatedAt: now,
	}
}

// enqueue hands a stored deployment to the deployer. Without one, or when
// it fails, the deployment stays queued; the deployer resumes queued
// deployments when it starts.
func (s *Server) enqueue(ctx context.Context, id string) {
	if s.d.Deployer == nil {
		return
	}
	if err := s.d.Deployer.Enqueue(ctx, id); err != nil {
		s.cfg.Log.Error("enqueue deployment", "deployment", id, "err", err)
	}
}

// deploymentJSON is the API form of a stored deployment.
func deploymentJSON(d store.Deployment) apiv1.Deployment {
	out := apiv1.Deployment{
		Id: d.ID, App: d.App, Branch: d.Branch, Instance: d.Instance, Ref: d.Ref, Commit: d.Commit,
		Trigger: apiv1.DeploymentTrigger(d.Trigger), TriggeredBy: d.TriggeredBy, Status: apiv1.DeploymentStatus(d.Status),
		Images:       make([]apiv1.DeploymentImage, len(d.Images)),
		Environments: make([]apiv1.DeploymentEnvironment, len(d.Environments)),
		Error:        optional(d.Error), BuildId: optional(d.BuildID),
		CreatedAt: d.CreatedAt, StartedAt: d.StartedAt, FinishedAt: d.FinishedAt,
	}
	for i, im := range d.Images {
		out.Images[i] = apiv1.DeploymentImage{Service: im.Service, Image: im.Image}
	}
	for i, e := range d.Environments {
		out.Environments[i] = apiv1.DeploymentEnvironment{
			Env: e.Env, Status: apiv1.DeploymentEnvironmentStatus(e.Status),
			SyncStatus: optional(e.SyncStatus), HealthStatus: optional(e.HealthStatus), Url: optional(e.URL), Message: optional(e.Message),
		}
	}
	return out
}

// optional is nil for "".
func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
