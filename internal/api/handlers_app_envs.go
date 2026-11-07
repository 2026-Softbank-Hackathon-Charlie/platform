package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/deploy"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/appspec"
)

// Adding and removing an environment of a registered app (ADR 0037).

// activeRunStatuses are the statuses of a migration run that may still move
// the app's traffic or database: running, waiting for its switch, halted
// after the target was asked to promote, or interrupted by a restart.
var activeRunStatuses = []apiv1.RunStatus{apiv1.RunStatusRunning, apiv1.RunStatusAwaitingSwitch, apiv1.RunStatusHalted, apiv1.RunStatusInterrupted}

// POST /v1/apps/{name}/environments
func (s *Server) addAppEnvironment(w http.ResponseWriter, r *http.Request) {
	var req apiv1.AppEnvironmentAddRequest
	if !s.decode(w, r, &req, true) {
		return
	}
	name, env := r.PathValue("name"), req.Environment
	noteAudit(r, "app", name)
	noteAudit(r, "environment", env)
	if env == "" {
		s.fail(w, r, http.StatusBadRequest, "invalid_request", "environment must name an environment")
		return
	}
	app, ok := s.registeredApp(w, r, name)
	if !ok {
		return
	}
	known := s.d.Envs.Names()
	if !slices.Contains(known, env) {
		s.fail(w, r, http.StatusNotFound, "env_not_found", fmt.Sprintf("unknown environment %q; this server has %s", env, strings.Join(known, ", ")))
		return
	}

	// From here on the writes must finish even if the client goes away.
	ctx := context.WithoutCancel(r.Context())
	list, err := s.d.Store.ListInstanceDeployments(ctx, app.Name, 0)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	present := slices.Contains(app.Environments, env)
	if d := newestTo(list, env); present && d != nil && d.Status == store.DeployHealthy {
		s.fail(w, r, http.StatusConflict, "environment_exists", fmt.Sprintf("app %s is already in %s, and deployment %s went there healthy", app.Name, env, d.ID))
		return
	}
	if i := slices.IndexFunc(list, func(d store.Deployment) bool { return !store.DeploymentFinished(d.Status) }); i >= 0 {
		noteAudit(r, "inProgress", list[i].ID)
		s.fail(w, r, http.StatusConflict, "deployment_in_progress", fmt.Sprintf("deployment %s of %s is %s; add the environment once it is done", list[i].ID, app.Name, list[i].Status))
		return
	}
	busy, err := s.migrationBusy(ctx, app.Name, []string{app.Name})
	if err != nil {
		s.internal(w, r, err)
		return
	}
	if busy != "" {
		s.fail(w, r, http.StatusConflict, "migration_in_progress", busy+"; add the environment once the migration is done")
		return
	}
	last := lastHealthy(list)
	if last == nil {
		s.fail(w, r, http.StatusConflict, "nothing_to_redeploy", fmt.Sprintf("%s has no healthy deployment to take to %s; deploy the branch first", app.Name, env))
		return
	}
	noteAudit(r, "of", last.ID)
	specJSON := last.Spec
	if len(specJSON) == 0 && app.SpecCommit == last.Commit {
		specJSON = app.Spec // deployed before deployments recorded their spec
	}
	var spec appspec.Spec
	if err := json.Unmarshal(specJSON, &spec); err != nil {
		s.internal(w, r, fmt.Errorf("the recorded platform.yaml of %s: %w", last.ID, err))
		return
	}
	hasPG := slices.ContainsFunc(spec.Resources, func(res appspec.Resource) bool { return res.Type == appspec.TypePostgres })
	if hasPG {
		db, err := s.d.Store.GetAppDatabase(ctx, app.Name)
		switch {
		case errors.Is(err, store.ErrNotFound):
			s.fail(w, r, http.StatusConflict, "db_pair_unavailable", fmt.Sprintf("%s has a postgres resource, but app %s has no database pair, so its database in %s would be a primary of its own that can never be migrated to; delete the app and register it again to get one", appspec.FileName, app.Name, env))
			return
		case err != nil:
			s.internal(w, r, err)
			return
		case db.PeerEnv != env && db.PrimaryEnv != env:
			s.fail(w, r, http.StatusConflict, "db_pair_unavailable", fmt.Sprintf("the database pair of %s is between %s and %s, so its database in %s would be a primary of its own that can never be migrated to", app.Name, db.PrimaryEnv, db.PeerEnv, env))
			return
		case s.isFileApp(app.Name):
			s.fail(w, r, http.StatusConflict, "db_pair_unavailable", fmt.Sprintf("app %s is also in PLATFORM_APPS_FILE, whose entry is what migrations use, so its database pair cannot be migrated", app.Name))
			return
		}
	}
	if !s.storageReady(w, r, spec, []string{env}) {
		return
	}
	secretRows, err := s.d.Store.ListAppSecrets(ctx, app.Name)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	hasS3 := slices.ContainsFunc(spec.Resources, func(res appspec.Resource) bool { return res.Type == appspec.TypeS3 })
	if (hasS3 || hasPG || len(secretRows) > 0) && (s.d.SealingConfigured == nil || !s.d.SealingConfigured(env)) {
		s.fail(w, r, http.StatusServiceUnavailable, "sealing_not_configured", fmt.Sprintf("the deployment of %s seals values (bucket keys, app secrets or the database pair), but environment %s has no usable sealed-secrets certificate (sealedSecretsCert in PLATFORM_ENVS_FILE)", app.Name, env))
		return
	}

	if !present {
		// After the ones it has: the first stays the one the app was
		// registered in first, where its database is born primary and
		// which is never removed.
		to := append(slices.Clone(app.Environments), env)
		if err := s.d.Store.SetAppEnvironments(ctx, app.Name, app.Environments, to); err != nil {
			switch {
			case errors.Is(err, store.ErrNotFound):
				s.fail(w, r, http.StatusNotFound, "app_not_found", "app not found")
			case errors.Is(err, store.ErrConflict):
				s.fail(w, r, http.StatusConflict, "environment_exists", fmt.Sprintf("the environments of %s changed meanwhile; read the app and try again", app.Name))
			default:
				s.internal(w, r, err)
			}
			return
		}
		app.Environments = to
	}
	noteAudit(r, "environments", app.Environments)
	d, ok := s.environmentDeployment(ctx, w, r, app, *last, specJSON)
	if !ok {
		return
	}
	if hasPG && s.d.DBRoutes != nil {
		s.d.DBRoutes.Changed(app.Name)
	}
	view, ok := s.appViewNow(ctx, w, r, app)
	if !ok {
		return
	}
	w.Header().Set("Location", "/v1/apps/"+app.Name)
	writeJSON(w, http.StatusAccepted, apiv1.AppEnvironmentAdded{App: view, Deployment: deploymentJSON(d)})
}

// DELETE /v1/apps/{name}/environments/{env}
func (s *Server) removeAppEnvironment(w http.ResponseWriter, r *http.Request) {
	name, env := r.PathValue("name"), r.PathValue("env")
	noteAudit(r, "app", name)
	noteAudit(r, "environment", env)
	app, ok := s.registeredApp(w, r, name)
	if !ok {
		return
	}
	if !slices.Contains(app.Environments, env) {
		s.fail(w, r, http.StatusNotFound, "env_not_found", fmt.Sprintf("app %s is not in environment %q (it is in %s)", name, env, strings.Join(app.Environments, ", ")))
		return
	}
	ctx := context.WithoutCancel(r.Context())
	db, err := s.d.Store.GetAppDatabase(ctx, app.Name)
	switch {
	case errors.Is(err, store.ErrNotFound):
	case err != nil:
		s.internal(w, r, err)
		return
	}
	if app.Environments[0] == env || db.PrimaryEnv == env {
		s.fail(w, r, http.StatusConflict, "primary_environment", fmt.Sprintf("%s is app %s's first environment, where its database was born primary; it goes only with the app (DELETE /v1/apps/%s)", env, name, name))
		return
	}
	instances, err := s.instanceNames(ctx, app)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	runs, err := s.appRuns(ctx, instances)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	for _, run := range runs {
		if run.Status == apiv1.RunStatusFailed || run.Status == apiv1.RunStatusAborted {
			continue // stopped before anything irreversible
		}
		if run.Source.Env == env || run.Target.Env == env {
			s.fail(w, r, http.StatusConflict, "write_backend", fmt.Sprintf("migration run %s of %s moved writes between %s and %s, so %s holds data written there; delete the app instead", run.RunId, name, run.Source.Env, run.Target.Env, env))
			return
		}
	}
	all, err := s.d.Store.ListDeployments(ctx, app.Name, 0)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	if i := slices.IndexFunc(all, func(d store.Deployment) bool { return !store.DeploymentFinished(d.Status) }); i >= 0 {
		s.fail(w, r, http.StatusConflict, "deployment_in_progress", fmt.Sprintf("deployment %s of %s is %s; remove the environment once it is done", all[i].ID, all[i].Instance, all[i].Status))
		return
	}
	busy, err := s.migrationBusy(ctx, app.Name, instances)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	if busy != "" {
		s.fail(w, r, http.StatusConflict, "migration_in_progress", busy+"; remove the environment once the migration is done")
		return
	}
	if s.d.Remover == nil {
		s.fail(w, r, http.StatusServiceUnavailable, "github_not_configured", "this server has no deployer (PLATFORM_DEPLOY_BUILDER), so it cannot remove the environment's files from the gitops repository")
		return
	}
	err = s.d.Remover.RemoveEnvironment(ctx, app.Name, env)
	switch {
	case err == nil:
	case errors.Is(err, store.ErrNotFound):
		s.fail(w, r, http.StatusNotFound, "app_not_found", "app not found")
		return
	case errors.Is(err, store.ErrConflict):
		s.fail(w, r, http.StatusConflict, "deployment_in_progress", "the environments of "+app.Name+" changed meanwhile; read the app and try again")
		return
	case errors.Is(err, deploy.ErrDeploymentInProgress):
		s.fail(w, r, http.StatusConflict, "deployment_in_progress", err.Error())
		return
	case errors.Is(err, deploy.ErrEnvironmentInUse):
		s.fail(w, r, http.StatusConflict, "write_backend", err.Error()+"; migrate the app away from it first, or delete the app")
		return
	case errors.Is(err, deploy.ErrWritesHeld):
		s.fail(w, r, http.StatusConflict, "migration_in_progress", "the traffic gateway holds the app's writes (a migration is switching it); remove the environment once the migration is done")
		return
	case errors.Is(err, deploy.ErrGitOps):
		s.githubUnreachable(w, r, err)
		return
	case errors.Is(err, deploy.ErrGateway):
		s.cfg.Log.Warn("gateway", "err", err)
		s.fail(w, r, http.StatusBadGateway, "gateway_unreachable", "could not take the environment out of the traffic gateway; its gitops files are already removed, so try again")
		return
	default:
		s.internal(w, r, err)
		return
	}
	if s.d.DBRoutes != nil {
		s.d.DBRoutes.Changed(app.Name)
	}
	app, err = s.d.Store.GetApp(ctx, app.Name)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	noteAudit(r, "environments", app.Environments)
	out := apiv1.AppEnvironmentRemoved{}
	// The primary's file gets peerPresent false again with a deployment.
	list, err := s.d.Store.ListInstanceDeployments(ctx, app.Name, 0)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	if last := lastHealthy(list); last != nil {
		specJSON := last.Spec
		if len(specJSON) == 0 && app.SpecCommit == last.Commit {
			specJSON = app.Spec
		}
		d, ok := s.environmentDeployment(ctx, w, r, app, *last, specJSON)
		if !ok {
			return
		}
		dj := deploymentJSON(d)
		out.Deployment = &dj
	}
	if out.App, ok = s.appViewNow(ctx, w, r, app); !ok {
		return
	}
	writeJSON(w, http.StatusAccepted, out)
}

// environmentDeployment stores and queues a deployment of app's default
// instance to all of its environments that repeats last (trigger
// environment, no build), and answers the error itself when it cannot.
func (s *Server) environmentDeployment(ctx context.Context, w http.ResponseWriter, r *http.Request, app store.App, last store.Deployment, spec []byte) (store.Deployment, bool) {
	d := newDeployment(app, defaultInstance(app), last.Ref, last.Commit, apiv1.DeploymentTriggerEnvironment, actor(r), storeTime())
	d.Images, d.Spec = last.Images, spec
	if err := s.d.Store.CreateDeployment(ctx, d); err != nil {
		switch {
		case errors.Is(err, store.ErrNotFound):
			s.fail(w, r, http.StatusNotFound, "app_not_found", "app not found")
		default:
			s.internal(w, r, err)
		}
		return store.Deployment{}, false
	}
	noteAudit(r, "deployment", d.ID)
	s.enqueue(ctx, d.ID)
	return d, true
}

// appViewNow is the app as GET /v1/apps/{name} shows it, without readiness,
// read now; it answers the error itself.
func (s *Server) appViewNow(ctx context.Context, w http.ResponseWriter, r *http.Request, app store.App) (apiv1.App, bool) {
	branches, deps, db, err := s.appRecords(ctx, app)
	if err == nil {
		var view apiv1.App
		if view, err = s.appView(ctx, app, branches, deps, db); err == nil {
			return view, true
		}
	}
	s.internal(w, r, err)
	return apiv1.App{}, false
}

// newestTo is the newest deployment of list (newest first) that lists env,
// or nil.
func newestTo(list []store.Deployment, env string) *store.Deployment {
	for i, d := range list {
		if slices.ContainsFunc(d.Environments, func(e store.DeploymentEnvironment) bool { return e.Env == env }) {
			return &list[i]
		}
	}
	return nil
}

// lastHealthy is the newest healthy deployment of list (newest first), or nil.
func lastHealthy(list []store.Deployment) *store.Deployment {
	for i, d := range list {
		if d.Status == store.DeployHealthy {
			return &list[i]
		}
	}
	return nil
}

// instanceNames lists the app's instances: the default one, then the added
// branches'.
func (s *Server) instanceNames(ctx context.Context, app store.App) ([]string, error) {
	branches, err := s.d.Store.ListAppBranches(ctx, app.Name)
	if err != nil {
		return nil, err
	}
	out := []string{app.Name}
	for _, b := range branches {
		out = append(out, b.Instance)
	}
	return out, nil
}

// appRuns returns the migration runs whose source or target is the
// namespace of one of instances, newest first.
func (s *Server) appRuns(ctx context.Context, instances []string) ([]apiv1.Run, error) {
	rows, err := s.d.Store.ListRuns(ctx)
	if err != nil {
		return nil, err
	}
	var out []apiv1.Run
	for _, row := range rows {
		var run apiv1.Run
		if err := json.Unmarshal(row.Doc, &run); err != nil {
			return nil, fmt.Errorf("run %s: %w", row.RunID, err)
		}
		if slices.Contains(instances, run.Source.Namespace) || slices.Contains(instances, run.Target.Namespace) {
			out = append(out, run)
		}
	}
	return out, nil
}

// migrationBusy says why a migration of app is in progress, or "": a run of
// one of instances that is running, awaiting its switch, halted or
// interrupted, or the traffic gateway holding an instance's writes. A
// gateway that cannot be read is only logged: a deployment that reaches it
// refuses held writes again.
func (s *Server) migrationBusy(ctx context.Context, app string, instances []string) (string, error) {
	runs, err := s.appRuns(ctx, instances)
	if err != nil {
		return "", err
	}
	for _, run := range runs {
		if slices.Contains(activeRunStatuses, run.Status) {
			return fmt.Sprintf("migration run %s of %s is %s", run.RunId, app, run.Status), nil
		}
	}
	if s.d.Gateway == nil {
		return "", nil
	}
	list, err := s.d.Gateway.Apps(ctx)
	if err != nil {
		s.cfg.Log.Warn("gateway: could not read whether it holds the app's writes", "app", app, "err", err)
		return "", nil
	}
	for _, a := range list.Apps {
		if !a.Default && a.WritesHeld && slices.Contains(instances, a.Name) {
			return fmt.Sprintf("the traffic gateway holds the writes of %s", a.Name), nil
		}
	}
	return "", nil
}
