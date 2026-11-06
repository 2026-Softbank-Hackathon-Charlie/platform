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
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/github"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/appspec"
)

// reservedAppNames are served by the platform itself under the app domain
// (the API, the console and the gateway's default app, ADR 0012, and the
// Headlamp ingress of infra), so no registered app may take them.
var reservedAppNames = []string{"api", "console", "app", "headlamp"}

// reservedNamespaces are the namespaces of the app clusters that the app
// ApplicationSet refuses (infra helm/charts/hopcharlie-apps deniedNamespaces,
// with traefik, the shared app entry of infra ADR 0058). An app named after
// one would be committed and then never sync, so it is refused here. Keep
// the two lists the same.
var reservedNamespaces = []string{
	"argocd", "cert-manager", "charlie-apps", "charlie-db", "charlie-platform", "charlie-system", "cnpg-system",
	"default", "headlamp", "hopcharlie-db-network", "kube-node-lease", "kube-public", "kube-system", "observability", "traefik",
}

// appStatusDeployments is how many newest deployments an instance's
// per-environment status is read from. Each instance is read on its own, so
// a busy instance never pushes another's deployments out.
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
	if slices.Contains(reservedNamespaces, spec.Name) {
		s.fail(w, r, http.StatusConflict, "app_exists", fmt.Sprintf("%s is a namespace the app clusters keep for the platform and infra; rename the app in %s", spec.Name, appspec.FileName))
		return
	}
	if !s.storageCleaned(ctx, w, r, spec.Name) || !s.storageReady(w, r, *spec, targets) {
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
	db, err := s.newAppDatabase(app)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	if db != nil {
		noteAudit(r, "dbPrimary", db.PrimaryEnv)
		err = s.d.Store.CreateAppWithDatabase(ctx, app, *db)
	} else {
		err = s.d.Store.CreateApp(ctx, app)
	}
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			s.fail(w, r, http.StatusConflict, "app_exists", fmt.Sprintf("an app named %s is already registered", spec.Name))
			return
		}
		s.internal(w, r, err)
		return
	}
	d := newDeployment(app, defaultInstance(app), branch, commit, apiv1.DeploymentTriggerRegister, actor(r), now)
	if err := s.d.Store.CreateDeployment(ctx, d); err != nil {
		s.internal(w, r, err)
		return
	}
	noteAudit(r, "deployment", d.ID)
	s.enqueue(ctx, d.ID)
	w.Header().Set("Location", "/v1/apps/"+app.Name)
	writeJSON(w, http.StatusCreated, apiv1.AppCreated{App: s.appView(app, nil, map[string][]store.Deployment{app.Name: {d}}, db), Deployment: deploymentJSON(d)})
}

// newAppDatabase makes the database pair of an app being registered (ADR
// 0030), or returns nil when it gets none: the setting is off, the app is
// not in exactly two environments, or there is no secrets key to store the
// pair with.
func (s *Server) newAppDatabase(app store.App) (*store.AppDatabase, error) {
	if !s.cfg.AppDBTopology || len(app.Environments) != 2 {
		return nil, nil
	}
	if s.d.Secrets == nil {
		s.cfg.Log.Warn("PLATFORM_APP_DB_TOPOLOGY is on, but there is no PLATFORM_SECRETS_KEY: registering the app without a database pair", "app", app.Name)
		return nil, nil
	}
	db, err := deploy.NewAppDatabase(s.d.Secrets, app.Name, app.Environments, app.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("make the database pair of %s: %w", app.Name, err)
	}
	return &db, nil
}

// POST /v1/apps/{name}/instances: add a branch of a registered app as its
// own instance and queue the instance's first deployment (ADR 0015).
func (s *Server) createAppInstance(w http.ResponseWriter, r *http.Request) {
	var req apiv1.AppInstanceCreateRequest
	if !s.decode(w, r, &req, true) {
		return
	}
	name, branch := r.PathValue("name"), req.Branch
	autoDeploy := req.AutoDeploy == nil || *req.AutoDeploy
	noteAudit(r, "app", name)
	noteAudit(r, "branch", branch)
	app, ok := s.registeredApp(w, r, name)
	if !ok {
		return
	}
	if !validBranch(branch) {
		s.fail(w, r, http.StatusBadRequest, "invalid_request", "branch must be a git branch name of letters, digits and . _ / -")
		return
	}
	if branch == app.Branch {
		s.fail(w, r, http.StatusConflict, "branch_exists", fmt.Sprintf("branch %s is app %s's own branch, deployed as its default instance", branch, name))
		return
	}
	instance, err := appspec.InstanceName(app.Name, branch)
	if err != nil {
		s.fail(w, r, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	noteAudit(r, "instance", instance)
	if s.d.GitHub == nil {
		s.githubNotConfigured(w, r)
		return
	}

	// The conflicts the store would refuse, checked before the round trips
	// to GitHub. CreateAppBranch checks them again atomically.
	ctx := r.Context()
	branches, err := s.d.Store.ListAppBranches(ctx, app.Name)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	switch {
	case slices.ContainsFunc(branches, func(b store.AppBranch) bool { return b.Branch == branch }):
		err = store.ErrBranchExists
	case slices.ContainsFunc(branches, func(b store.AppBranch) bool { return b.Instance == instance }):
		err = store.ErrInstanceExists
	case len(branches) >= appspec.MaxInstances-1:
		err = store.ErrInstanceLimit
	}
	if err != nil {
		s.instanceConflict(w, r, err, app.Name, branch, instance)
		return
	}
	if !s.storageCleaned(ctx, w, r, instance) {
		return
	}

	commit, err := s.d.GitHub.ResolveRef(ctx, app.Repo, branch)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		s.appSpecInvalid(w, r, refNotFound(app.Repo, branch))
		return
	case err != nil:
		s.githubUnreachable(w, r, err)
		return
	}
	noteAudit(r, "commit", commit)
	spec := s.appSpecAt(ctx, w, r, app, commit)
	if spec == nil || !s.storageReady(w, r, *spec, app.Environments) {
		return
	}
	specJSON, err := json.Marshal(spec)
	if err != nil {
		s.internal(w, r, err)
		return
	}

	// Added means deployed: finish even if the caller goes away.
	ctx = context.WithoutCancel(ctx)
	now := storeTime()
	in := store.AppBranch{
		App: app.Name, Branch: branch, Instance: instance, AutoDeploy: autoDeploy,
		Spec: specJSON, SpecCommit: commit, CreatedBy: actor(r), CreatedAt: now,
	}
	if err := s.d.Store.CreateAppBranch(ctx, in, appspec.MaxInstances-1); err != nil {
		switch {
		case s.instanceConflict(w, r, err, app.Name, branch, instance):
		case errors.Is(err, store.ErrNotFound):
			s.fail(w, r, http.StatusNotFound, "app_not_found", "app not found")
		default:
			s.internal(w, r, err)
		}
		return
	}
	d := newDeployment(app, in, branch, commit, apiv1.DeploymentTriggerRegister, actor(r), now)
	if err := s.d.Store.CreateDeployment(ctx, d); err != nil {
		switch {
		case errors.Is(err, store.ErrInstanceNotFound):
			// DELETE of the instance came first.
			s.fail(w, r, http.StatusNotFound, "instance_not_found", fmt.Sprintf("app %s has no instance %s", name, instance))
		case errors.Is(err, store.ErrNotFound):
			// DELETE of the app came first.
			s.fail(w, r, http.StatusNotFound, "app_not_found", "app not found")
		default:
			// Without its first deployment the branch would only make a
			// retry fail with branch_exists, so it goes too.
			if derr := s.d.Store.DeleteAppBranch(ctx, app.Name, branch); derr != nil && !errors.Is(derr, store.ErrNotFound) {
				s.cfg.Log.Error("delete a branch whose first deployment was not stored", "app", app.Name, "branch", branch, "err", derr)
			}
			s.internal(w, r, err)
		}
		return
	}
	noteAudit(r, "deployment", d.ID)
	s.enqueue(ctx, d.ID)
	w.Header().Set("Location", "/v1/apps/"+app.Name)
	writeJSON(w, http.StatusCreated, apiv1.AppInstanceCreated{Instance: s.appInstance(app, in, []store.Deployment{d}), Deployment: deploymentJSON(d)})
}

// instanceConflict answers 409 when err is one of the conflicts of adding
// branch to app, found by createAppInstance's checks or by the store, and
// reports whether it answered.
func (s *Server) instanceConflict(w http.ResponseWriter, r *http.Request, err error, app, branch, instance string) bool {
	switch {
	case errors.Is(err, store.ErrBranchExists):
		s.fail(w, r, http.StatusConflict, "branch_exists", fmt.Sprintf("app %s already deploys branch %s", app, branch))
	case errors.Is(err, store.ErrInstanceExists):
		s.fail(w, r, http.StatusConflict, "instance_exists", fmt.Sprintf("the instance name %s of branch %s is already taken by another branch or app", instance, branch))
	case errors.Is(err, store.ErrInstanceLimit):
		s.fail(w, r, http.StatusConflict, "instance_limit", fmt.Sprintf("app %s already has %d instances, the default one included; delete one first", app, appspec.MaxInstances))
	default:
		return false
	}
	return true
}

// storageCleaned answers 409 storage_cleanup_pending when buckets and keys
// made for an earlier instance with this name are still waiting to be
// deleted, and reports whether there are none. Ownership of buckets and
// keys goes by name, so a new app or instance must not take them over. The
// rows of an instance that still exists are not pending: the store's own
// conflict (app_exists, instance_exists) answers that case.
func (s *Server) storageCleaned(ctx context.Context, w http.ResponseWriter, r *http.Request, instance string) bool {
	rows, err := s.d.Store.ListInstanceStorage(ctx, "", instance)
	if err != nil {
		s.internal(w, r, err)
		return false
	}
	for _, row := range rows {
		live, err := s.instanceLive(ctx, row.App, row.Instance)
		if err != nil {
			s.internal(w, r, err)
			return false
		}
		if !live {
			s.fail(w, r, http.StatusConflict, "storage_cleanup_pending", fmt.Sprintf("the bucket and key of a deleted app or instance named %s are still being deleted; try again in a few minutes", instance))
			return false
		}
	}
	return true
}

// storageReady answers 503 storage_not_configured when spec has an s3
// resource and one of envs has no storage driver to make its bucket (ADR
// 0016), and reports whether all have one.
func (s *Server) storageReady(w http.ResponseWriter, r *http.Request, spec appspec.Spec, envs []string) bool {
	if !slices.ContainsFunc(spec.Resources, func(res appspec.Resource) bool { return res.Type == appspec.TypeS3 }) {
		return true
	}
	for _, env := range envs {
		if s.d.StorageConfigured == nil || !s.d.StorageConfigured(env) {
			s.fail(w, r, http.StatusServiceUnavailable, "storage_not_configured", fmt.Sprintf("%s has an s3 resource, but environment %s has no storage driver to make its bucket (storage in PLATFORM_ENVS_FILE)", appspec.FileName, env))
			return false
		}
	}
	return true
}

// instanceLive reports whether app is registered and has the instance.
func (s *Server) instanceLive(ctx context.Context, app, instance string) (bool, error) {
	a, err := s.d.Store.GetApp(ctx, app)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return s.isInstance(ctx, a, instance)
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
		branches, deps, db, err := s.appRecords(r.Context(), a)
		if err != nil {
			s.internal(w, r, err)
			return
		}
		deploy := s.appDeploy(a, branches, deps, db)
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
	branches, deps, db, err := s.appRecords(r.Context(), a)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, s.appView(a, branches, deps, db))
}

// appRecords reads what a registered app's deploy section shows besides
// the app: its added branches, by instance each instance's newest
// deployments, newest first, and its database pair (nil without one). That
// is one read for the branches, one for each instance, appspec.MaxInstances
// at most, and one for the pair.
func (s *Server) appRecords(ctx context.Context, a store.App) ([]store.AppBranch, map[string][]store.Deployment, *store.AppDatabase, error) {
	branches, err := s.d.Store.ListAppBranches(ctx, a.Name)
	if err != nil {
		return nil, nil, nil, err
	}
	instances := []string{a.Name}
	for _, b := range branches {
		instances = append(instances, b.Instance)
	}
	deps := map[string][]store.Deployment{}
	for _, in := range instances {
		if deps[in], err = s.d.Store.ListInstanceDeployments(ctx, in, appStatusDeployments); err != nil {
			return nil, nil, nil, err
		}
	}
	db, err := s.d.Store.GetAppDatabase(ctx, a.Name)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return branches, deps, nil, nil
	case err != nil:
		return nil, nil, nil, err
	}
	return branches, deps, &db, nil
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
// when the file has the name, otherwise the defaults, with deploy set from
// branches, deps and db as appDeploy takes them.
func (s *Server) appView(a store.App, branches []store.AppBranch, deps map[string][]store.Deployment, db *store.AppDatabase) apiv1.App {
	deploy := s.appDeploy(a, branches, deps, db)
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

// appDeploy is a registered app's deploy section. branches are its added
// branches, by branch name; deps are each instance's newest deployments, by
// instance, newest first; db is its database pair or nil. Its branch,
// autoDeploy, spec, specCommit, url, status and lastDeployment are the
// default instance's.
func (s *Server) appDeploy(a store.App, branches []store.AppBranch, deps map[string][]store.Deployment, db *store.AppDatabase) apiv1.AppDeploy {
	def := s.appInstance(a, defaultInstance(a), deps[a.Name])
	out := apiv1.AppDeploy{
		Repo: a.Repo, Branch: def.Branch, Environments: a.Environments, AutoDeploy: def.AutoDeploy,
		Spec: def.Spec, SpecCommit: def.SpecCommit, Url: def.Url, CreatedAt: a.CreatedAt, CreatedBy: a.CreatedBy,
		LastDeployment: def.LastDeployment, Status: def.Status,
		Instances: make([]apiv1.AppInstance, 0, 1+len(branches)),
	}
	out.Instances = append(out.Instances, def)
	if db != nil {
		out.DbTopology = &apiv1.AppDBTopology{Primary: db.PrimaryEnv, Replica: db.PeerEnv}
	}
	for _, b := range branches {
		out.Instances = append(out.Instances, s.appInstance(a, b, deps[b.Instance]))
	}
	return out
}

// defaultInstance is the app's own branch as an AppBranch: the default
// instance, named after the app. It is never stored as an AppBranch.
func defaultInstance(a store.App) store.AppBranch {
	return store.AppBranch{
		App: a.Name, Branch: a.Branch, Instance: a.Name, AutoDeploy: a.AutoDeploy,
		Spec: a.Spec, SpecCommit: a.SpecCommit, CreatedBy: a.CreatedBy, CreatedAt: a.CreatedAt,
	}
}

// appInstance is one instance of a registered app: the default one
// (defaultInstance) or an added branch. deps are the instance's newest
// deployments, newest first; each environment of the app shows the newest
// one that reached it.
func (s *Server) appInstance(a store.App, in store.AppBranch, deps []store.Deployment) apiv1.AppInstance {
	var spec apiv1.AppSpec
	if err := json.Unmarshal(in.Spec, &spec); err != nil {
		s.cfg.Log.Error("stored app spec does not decode", "app", a.Name, "instance", in.Instance, "err", err)
	}
	if spec.Services == nil {
		spec.Services = []apiv1.AppSpecService{}
	}
	out := apiv1.AppInstance{
		Name: in.Instance, Branch: in.Branch, Default: in.Instance == a.Name, AutoDeploy: in.AutoDeploy,
		Spec: spec, SpecCommit: in.SpecCommit, CreatedAt: in.CreatedAt, CreatedBy: in.CreatedBy,
		Status: make([]apiv1.AppEnvironmentStatus, 0, len(a.Environments)),
	}
	if slices.ContainsFunc(spec.Services, func(sv apiv1.AppSpecService) bool { return sv.Public }) {
		u := deploy.AppURL(s.cfg.AppDomain, in.Instance)
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

// appSpecAt checks platform.yaml of app's repository at commit as
// registration does, and that its name is still the app's. It answers 502
// or 422 itself and returns nil then.
func (s *Server) appSpecAt(ctx context.Context, w http.ResponseWriter, r *http.Request, app store.App, commit string) *appspec.Spec {
	v, spec, err := deploy.CheckSpec(ctx, s.d.GitHub, app.Repo, commit)
	if err != nil {
		s.githubUnreachable(w, r, err)
		return nil
	}
	if v.Valid && spec.Name != app.Name {
		v.Valid = false
		v.Errors = append(v.Errors, apiv1.AppSpecFieldError{Path: "name", Message: fmt.Sprintf("is %q, but the app is registered as %q; register the new name as a new app", spec.Name, app.Name)})
	}
	if !v.Valid {
		s.appSpecInvalid(w, r, v)
		return nil
	}
	return spec
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

// pushTokenRoute writes its own audit entry before it answers with a token.
const pushTokenRoute = "POST /v1/github/push-tokens"

// POST /v1/github/push-tokens: a token that may push to one repository
// scripts/import-repo.sh imported, for an hour (ADR 0023). The token is only
// in the answer: the audit entry records the repository and the expiry, and
// is written before the answer; when it cannot be, the token is revoked and
// not returned.
func (s *Server) createPushToken(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store") // the answer holds a token
	var req apiv1.PushTokenRequest
	if !s.decode(w, r, &req, true) {
		return
	}
	if !validPushRepo(req.Repo) {
		s.fail(w, r, http.StatusBadRequest, "invalid_request", "repo must be owner/name, without .git")
		return
	}
	noteAudit(r, "repo", req.Repo)
	if s.d.GitHub == nil {
		s.githubNotConfigured(w, r)
		return
	}
	// The name asked for is checked before any GitHub call, and the name
	// GitHub answers with (after a rename or transfer) again after.
	if msg := s.pushRefusal(req.Repo); msg != "" {
		s.fail(w, r, http.StatusForbidden, "repo_not_imported", msg)
		return
	}
	ctx := r.Context()
	repo, err := s.d.GitHub.ReadRepo(ctx, req.Repo)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		s.fail(w, r, http.StatusNotFound, "repo_not_found", fmt.Sprintf("repository %s does not exist, or the GitHub App installation cannot see it", req.Repo))
		return
	case err != nil:
		s.githubUnreachable(w, r, err)
		return
	}
	if repo.FullName != req.Repo {
		noteAudit(r, "repo", repo.FullName)
		noteAudit(r, "requested", req.Repo)
	}
	if msg := s.pushRefusal(repo.FullName); msg != "" {
		s.fail(w, r, http.StatusForbidden, "repo_not_imported", msg)
		return
	}
	if !slices.Contains(repo.Topics, github.ImportedTopic) {
		s.fail(w, r, http.StatusForbidden, "repo_not_imported", fmt.Sprintf("%s has no topic %s; push tokens are only for copies imported with scripts/import-repo.sh", repo.FullName, github.ImportedTopic))
		return
	}
	tok, err := s.d.GitHub.PushToken(ctx, repo)
	if err != nil {
		s.pushTokenFailed(w, r, repo.FullName, err)
		return
	}
	noteAudit(r, "expiresAt", tok.ExpiresAt)
	if err := s.auditBeforeAnswer(r, pushTokenRoute, http.StatusCreated); err != nil {
		s.revokePushToken(r, repo.FullName, tok.Token)
		s.fail(w, r, http.StatusServiceUnavailable, "audit_unavailable", "the audit log could not be written, so the token was revoked and is not returned; try again")
		return
	}
	writeJSON(w, http.StatusCreated, apiv1.PushToken{Repo: repo.FullName, Token: tok.Token, ExpiresAt: tok.ExpiresAt})
}

// validPushRepo accepts owner/name as the GitHub client does, except names
// GitHub never gives a repository: "." and "..", and a ".git" suffix, which
// belongs to a clone URL.
func validPushRepo(repo string) bool {
	_, name, _ := strings.Cut(repo, "/")
	return repoPattern.MatchString(repo) && name != "." && name != ".." && !strings.HasSuffix(name, ".git")
}

// pushRefusal says why repo (owner/name) never gets a push token, or ""
// when only its topic is left to check.
func (s *Server) pushRefusal(repo string) string {
	owner, _, _ := strings.Cut(repo, "/")
	switch org := s.d.GitHub.Org(); {
	case !strings.EqualFold(owner, org):
		return fmt.Sprintf("%s is not in the platform's GitHub organization %s; push tokens are only for copies imported with scripts/import-repo.sh", repo, org)
	case strings.EqualFold(repo, s.cfg.GitOpsRepo):
		return fmt.Sprintf("%s is the gitops repository; only the platform's deployments write it, so it never gets a push token", repo)
	}
	return ""
}

// pushTokenFailed answers an error of GitHub.PushToken.
func (s *Server) pushTokenFailed(w http.ResponseWriter, r *http.Request, repo string, err error) {
	switch {
	case errors.Is(err, github.ErrNotInOrg): // pushRefusal already checked; the client checks again
		s.fail(w, r, http.StatusForbidden, "repo_not_imported", fmt.Sprintf("%s is not in the platform's GitHub organization %s", repo, s.d.GitHub.Org()))
	case errors.Is(err, github.ErrNotInInstallation):
		s.cfg.Log.Warn("github", "err", err, "path", r.URL.Path)
		s.fail(w, r, http.StatusNotFound, "repo_not_found", fmt.Sprintf("the GitHub App installation cannot grant a token for %s; add the repository to the installation (scripts/import-repo.sh does)", repo))
	case errors.Is(err, github.ErrUnexpectedScope):
		s.cfg.Log.Error("GitHub answered a push token request with a wider token; it was revoked", "repo", repo, "err", err)
		s.fail(w, r, http.StatusBadGateway, "github_unreachable", fmt.Sprintf("GitHub answered with a token that is not limited to %s with contents write, so it was revoked and is not returned; see the server logs", repo))
	default:
		s.cfg.Log.Warn("github", "err", err, "path", r.URL.Path)
		s.fail(w, r, http.StatusBadGateway, "github_unreachable", "could not create the push token through the GitHub App; see the server logs")
	}
}

// revokePushToken revokes a token the caller will not get. A failure is
// logged without the token; GitHub expires the token within the hour.
func (s *Server) revokePushToken(r *http.Request, repo, token string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 10*time.Second)
	defer cancel()
	if err := s.d.GitHub.RevokeToken(ctx, token); err != nil {
		s.cfg.Log.Error("could not revoke a push token that was not handed out; it expires within the hour", "repo", repo, "err", err)
	}
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
	// The delivery is recorded, so a redelivery would deploy nothing: an app
	// that fails is logged and the others still deploy.
	var failed error
	for _, a := range apps {
		if !strings.EqualFold(a.Repo, push.Repository.FullName) {
			continue
		}
		// The app's own branch deploys the default instance and an added
		// branch its own instance, each when its autoDeploy is on.
		in, tracked, err := s.instanceOf(ctx, a, branch)
		if err != nil {
			s.cfg.Log.Error("webhook: read the branches of an app", "app", a.Name, "delivery", delivery, "err", err)
			failed = err
			continue
		}
		if !tracked || !in.AutoDeploy {
			continue
		}
		d := newDeployment(a, in, branch, push.After, apiv1.DeploymentTriggerPush, by, now)
		if err := s.d.Store.CreateDeployment(ctx, d); err != nil {
			if !errors.Is(err, store.ErrNotFound) { // not unregistered or removed meanwhile
				s.cfg.Log.Error("webhook: store a deployment", "app", a.Name, "instance", in.Instance, "delivery", delivery, "err", err)
				failed = err
			}
			continue
		}
		s.enqueue(ctx, d.ID)
		out.Deployments = append(out.Deployments, deploymentJSON(d))
		ids = append(ids, d.ID)
	}
	switch {
	case len(ids) == 0 && failed != nil:
		s.internal(w, r, failed)
		return http.StatusInternalServerError
	case len(ids) == 0:
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

// newDeployment is a queued deployment of commit into the instance in of a
// (defaultInstance or an added branch), to all of a's environments.
func newDeployment(a store.App, in store.AppBranch, ref, commit string, trigger apiv1.DeploymentTrigger, by string, now time.Time) store.Deployment {
	envs := make([]store.DeploymentEnvironment, len(a.Environments))
	for i, e := range a.Environments {
		envs[i] = store.DeploymentEnvironment{Env: e, Status: string(apiv1.DeploymentEnvironmentStatusPending)}
	}
	return store.Deployment{
		ID: newDeploymentID(), App: a.Name, Branch: in.Branch, Instance: in.Instance, Ref: ref, Commit: commit, Trigger: string(trigger), TriggeredBy: by,
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
