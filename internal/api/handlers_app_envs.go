package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/cnpg"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/deploy"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/runs"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/appspec"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// Adding and removing an environment of a registered app (ADR 0043).

// runActive reports whether a migration run is in progress, with the rule
// POST /v1/migrations uses to refuse another run on its clusters
// (runs.Runner.IsActive): a run that ended, halted or interrupted included,
// is not.
func (s *Server) runActive(run apiv1.Run) bool {
	if s.d.Runs != nil {
		return s.d.Runs.IsActive(run)
	}
	return runs.StoredActive(run)
}

// CleanupCheckTimeout bounds the reads that tell whether what an earlier
// removal of an environment deletes is gone (ADR 0043).
var CleanupCheckTimeout = 5 * time.Second

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
	if d := newestTo(list, env); present && d != nil && envStatus(*d, env) == deploy.EnvHealthy {
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
		case !slices.Contains(s.cfg.AppDBEnvs, env):
			s.fail(w, r, http.StatusConflict, "db_pair_unavailable", fmt.Sprintf("environment %s has no registered database host template", env))
			return
		case !present && len(app.Environments) >= 3:
			s.fail(w, r, http.StatusConflict, "db_group_full", "a registered database group supports at most three environments")
			return
		case s.isFileApp(app.Name):
			s.fail(w, r, http.StatusConflict, "db_pair_unavailable", fmt.Sprintf("app %s is also in PLATFORM_APPS_FILE, whose entry is what migrations use, so its database pair cannot be migrated", app.Name))
			return
		}
		if db.PrimaryEnv != env && db.PeerEnv != env {
			if (!present && len(app.Environments) != 2) || !slices.Contains(app.Environments, db.PrimaryEnv) || !slices.Contains(app.Environments, db.PeerEnv) {
				s.fail(w, r, http.StatusConflict, "db_pair_unavailable", "add the original database peer before extending the group")
				return
			}
			// A new member bootstraps from the birth primary. Never create it
			// with a stale primary identity after a previous migration.
			if s.d.DBCluster == nil {
				s.fail(w, r, http.StatusServiceUnavailable, "db_primary_unknown", "cannot confirm the bootstrap primary without workload cluster reads")
				return
			}
			i := slices.IndexFunc(spec.Resources, func(res appspec.Resource) bool { return res.Type == appspec.TypePostgres })
			cluster := deploy.ClusterName(app.Name, spec.Resources[i].Name)
			cctx, cancel := context.WithTimeout(ctx, DBClusterTimeout)
			st, readErr := s.d.DBCluster(cctx, db.PrimaryEnv, deploy.Namespace(app.Name), cluster)
			cancel()
			self := cluster + "-" + db.PrimaryEnv
			if readErr != nil || st == nil || !st.Healthy() || st.Self != self || st.ReplicaPrimary != self {
				s.fail(w, r, http.StatusConflict, "db_bootstrap_primary_required", fmt.Sprintf("migrate to %s and verify its primary before adding %s", db.PrimaryEnv, env))
				return
			}
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
		// An environment removed before must be gone there first: the new
		// Application would otherwise meet the old namespace, database and
		// tailnet devices while Argo CD deletes them.
		if !s.environmentCleanedUp(ctx, w, r, app, env) {
			return
		}
		// After the ones it has: the first stays the one the app was
		// registered in first, where its database is born primary and
		// which is never removed.
		to := append(slices.Clone(app.Environments), env)
		if err := s.d.Store.SetAppEnvironments(ctx, app.Name, app.Environments, to); err != nil {
			switch {
			case errors.Is(err, store.ErrNotFound):
				s.fail(w, r, http.StatusNotFound, "app_not_found", "app not found")
			case errors.Is(err, store.ErrConflict):
				s.appChanged(w, r, app.Name)
			default:
				s.internal(w, r, err)
			}
			return
		}
		app.Environments = to
	}
	noteAudit(r, "environments", app.Environments)
	d, err := s.queueEnvironmentDeployment(ctx, r, app, *last, specJSON)
	switch {
	case errors.Is(err, store.ErrNotFound):
		s.fail(w, r, http.StatusNotFound, "app_not_found", "app not found")
		return
	case err != nil:
		// The environment is in the app now; sending the request again
		// queues the deployment that did not get queued.
		s.cfg.Log.Error("queue the deployment of an added environment", "app", app.Name, "env", env, "err", err)
		s.fail(w, r, http.StatusInternalServerError, "deployment_not_queued", fmt.Sprintf("%s is one of the environments of %s now, but its deployment could not be queued; send the same request again to queue it", env, app.Name))
		return
	}
	if hasPG && s.d.DBRoutes != nil {
		s.d.DBRoutes.Changed(app.Name)
	}
	w.Header().Set("Location", "/v1/apps/"+app.Name)
	writeJSON(w, http.StatusAccepted, apiv1.AppEnvironmentAdded{App: s.appViewAfter(ctx, app), Deployment: deploymentJSON(d)})
}

// environmentCleanedUp answers 409 environment_cleanup_pending, and returns
// false, while what an earlier removal of env from app deletes is still
// there: for each of the app's instances the Argo CD Application
// <instance>-<env> in the management cluster, and any Service or Pod in the
// instance's namespace in env. The Application keeps its finalizer until
// Argo CD deleted the namespace, and a Service of the Tailscale operator
// keeps its own until the operator deleted the proxy and its tailnet device,
// so with neither left the new database gets the old hostname on the
// tailnet again (ADR 0043). They are read whenever this server can read
// them, so an app deleted and registered again under the same name, whose
// deployments are gone with the old registration, still waits. A server
// that cannot read them lets an app never deployed to env through (by its
// deployments), and answers 503 environment_cleanup_unknown otherwise; a
// read that fails answers 502 environment_cleanup_unknown. The namespace
// itself is not read: the platform's role in the workload clusters may
// list Services and Pods but not get Namespaces (infra
// hopcharlie-platform-observer).
func (s *Server) environmentCleanedUp(ctx context.Context, w http.ResponseWriter, r *http.Request, app store.App, env string) bool {
	if s.d.Remover == nil || s.d.WorkloadResidue == nil {
		all, err := s.d.Store.ListDeployments(ctx, app.Name, 0)
		if err != nil {
			s.internal(w, r, err)
			return false
		}
		if !slices.ContainsFunc(all, func(d store.Deployment) bool { return envStatus(d, env) != "" }) {
			return true // never deployed there
		}
		s.fail(w, r, http.StatusServiceUnavailable, "environment_cleanup_unknown", fmt.Sprintf("app %s was in %s before, and this server cannot read whether Argo CD finished deleting it there (no deployer or no workload cluster reads)", app.Name, env))
		return false
	}
	instances, err := s.instanceNames(ctx, app)
	if err != nil {
		s.internal(w, r, err)
		return false
	}
	cctx, cancel := context.WithTimeout(ctx, CleanupCheckTimeout)
	defer cancel()
	unknown := func(what string, err error) bool {
		s.cfg.Log.Warn("environment cleanup check", "app", app.Name, "env", env, "what", what, "err", err)
		s.fail(w, r, http.StatusBadGateway, "environment_cleanup_unknown", fmt.Sprintf("could not read whether %s is gone, which the earlier removal of %s from %s deletes; try again", what, env, app.Name))
		return false
	}
	for _, in := range instances {
		ns := deploy.Namespace(in)
		appName := deploy.ApplicationName(ns, env)
		exists, err := s.d.Remover.ApplicationExists(cctx, appName)
		if err != nil {
			return unknown("Argo CD Application "+appName, err)
		}
		if exists {
			s.fail(w, r, http.StatusConflict, "environment_cleanup_pending", fmt.Sprintf("Argo CD is still deleting Application %s of the earlier removal of %s; add the environment once it is gone", appName, env))
			return false
		}
		left, err := s.d.WorkloadResidue(cctx, env, ns)
		if err != nil {
			return unknown("namespace "+ns+" in "+env, err)
		}
		if left != "" {
			s.fail(w, r, http.StatusConflict, "environment_cleanup_pending", fmt.Sprintf("%s is still in namespace %s in %s, which the earlier removal of %s deletes; add the environment once it is gone", left, ns, env, env))
			return false
		}
	}
	return true
}

// What the removal reads again under the instance locks found changed.
var (
	errRemovalMigrating = errors.New("a migration of the app started")
	errRemovalPrimary   = errors.New("the removed member's database is a primary")
	errRewireChanged    = errors.New("the database group changed")
	errRewireDenied     = errors.New("the workload cluster refused the change")
	errRewireFailed     = errors.New("the change of the primary's replica.source failed")
)

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
		s.envNotInApp(w, r, app, env)
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
		s.primaryEnvironment(w, r, app, env)
		return
	}
	// A member of a three-environment database group (ADR 0064): the two
	// that remain hold every write, which prepareGroupRemoval reads from
	// the live replication topology, so a member that was a migration's
	// source or target before may go too.
	group := db.App != "" && len(app.Environments) > 2
	var plan *groupPlan // a group member's removal, carried out under the locks
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
		if group || run.Status == apiv1.RunStatusFailed || run.Status == apiv1.RunStatusAborted {
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
	if d := filesWithoutGateway(app, all, env); d != nil {
		s.fail(w, r, http.StatusConflict, "environment_has_files", fmt.Sprintf("deployment %s of %s went to %s healthy with an s3 resource and no public service, so nothing tells whether the app wrote files to its bucket there, which removing the environment deletes; delete the app instead", d.ID, d.Instance, env))
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
	if group {
		if db.SourceKind == store.DBSourceExternal {
			s.fail(w, r, http.StatusConflict, "migration_in_progress", fmt.Sprintf("the onboarding of %s has not cut over to its database yet; remove the environment once it is done", app.Name))
			return
		}
		var ok bool
		if plan, ok = s.prepareGroupRemoval(ctx, w, r, app, env); !ok {
			return
		}
	}
	// Under the instance locks, after the executor's own refusals (branches,
	// deployments, the gateway's routing) and before it changes anything: a
	// migration that started since the checks above, or the removed member's
	// database promoted meanwhile, stops the removal; then the group's plan
	// changes the primary's replica.source, the first live change (ADR 0064).
	recheck := func(cctx context.Context) error {
		busy, err := s.migrationBusy(cctx, app.Name, instances)
		if err != nil {
			return err
		}
		if busy != "" {
			return fmt.Errorf("%w: %s", errRemovalMigrating, busy)
		}
		if plan == nil {
			return nil
		}
		rctx, cancel := context.WithTimeout(cctx, DBClusterTimeout)
		st, err := s.d.DBCluster(rctx, env, deploy.Namespace(app.Name), plan.cluster)
		cancel()
		if err == nil && st != nil && st.IsPrimary() {
			return fmt.Errorf("%w: the database of %s in %s is a primary now", errRemovalPrimary, app.Name, env)
		}
		if err := plan.rewire(cctx, s, app.Name); err != nil {
			return err
		}
		noteAudit(r, "replicaSource", plan.standby)
		return nil
	}
	err = s.d.Remover.RemoveEnvironment(ctx, app.Name, env, recheck)
	switch {
	case err == nil:
	case errors.Is(err, errRemovalMigrating):
		s.fail(w, r, http.StatusConflict, "migration_in_progress", err.Error()+"; remove the environment once the migration is done")
		return
	case errors.Is(err, errRemovalPrimary):
		s.fail(w, r, http.StatusConflict, "write_backend", err.Error()+"; migrate it to another environment first")
		return
	case errors.Is(err, errRewireChanged):
		s.fail(w, r, http.StatusConflict, "db_topology_changed", err.Error()+"; nothing was changed, try again")
		return
	case errors.Is(err, errRewireDenied):
		s.fail(w, r, http.StatusBadGateway, "db_topology_denied", err.Error()+"; nothing was changed")
		return
	case errors.Is(err, errRewireFailed):
		s.fail(w, r, http.StatusBadGateway, "db_topology_unreachable", err.Error()+"; nothing else was changed, try again")
		return
	case errors.Is(err, store.ErrNotFound):
		s.fail(w, r, http.StatusNotFound, "app_not_found", "app not found")
		return
	case errors.Is(err, deploy.ErrNotInEnvironment):
		s.envNotInApp(w, r, app, env)
		return
	case errors.Is(err, deploy.ErrPrimaryEnvironment):
		s.primaryEnvironment(w, r, app, env)
		return
	case errors.Is(err, deploy.ErrAppChanged), errors.Is(err, store.ErrConflict):
		s.appChanged(w, r, app.Name)
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
		s.cfg.Log.Warn("gitops", "err", err)
		s.fail(w, r, http.StatusBadGateway, "github_unreachable", "could not remove the environment's files from the gitops repository; it is out of the traffic gateway already, so try again")
		return
	case errors.Is(err, deploy.ErrGateway):
		s.cfg.Log.Warn("gateway", "err", err)
		s.fail(w, r, http.StatusBadGateway, "gateway_unreachable", "could not take the environment out of the traffic gateway; the environment is still in the app (for a database group member the primary may already follow the remaining member once demoted, which changes nothing now), so try again")
		return
	default:
		s.internal(w, r, err)
		return
	}

	// The environment is removed. What follows only adds to the answer:
	// a failure is logged and the answer is still 202.
	if s.d.DBRoutes != nil {
		s.d.DBRoutes.Changed(app.Name)
	}
	if fresh, err := s.d.Store.GetApp(ctx, app.Name); err == nil {
		app = fresh
	} else {
		s.cfg.Log.Warn("read the app after removing an environment", "app", app.Name, "err", err)
		app.Environments = slices.DeleteFunc(slices.Clone(app.Environments), func(e string) bool { return e == env })
	}
	noteAudit(r, "environments", app.Environments)
	out := apiv1.AppEnvironmentRemoved{}
	// The primary's file gets peerPresent false again with a deployment.
	if list, err := s.d.Store.ListInstanceDeployments(ctx, app.Name, 0); err != nil {
		s.cfg.Log.Error("list deployments after removing an environment; the next deployment rewrites the files", "app", app.Name, "err", err)
	} else if last := lastHealthy(list); last != nil {
		specJSON := last.Spec
		if len(specJSON) == 0 && app.SpecCommit == last.Commit {
			specJSON = app.Spec
		}
		if d, err := s.queueEnvironmentDeployment(ctx, r, app, *last, specJSON); err != nil {
			s.cfg.Log.Error("queue the deployment after removing an environment; the next deployment rewrites the files", "app", app.Name, "err", err)
		} else {
			dj := deploymentJSON(d)
			out.Deployment = &dj
		}
	}
	out.App = s.appViewAfter(ctx, app)
	writeJSON(w, http.StatusAccepted, out)
}

// groupPlan is what removing a member of a three-environment database
// group changes in a workload cluster (ADR 0064): the primary's
// replica.source, which it follows once demoted and which the engine
// requires to be the target of the next migration, becomes the remaining
// replica standby when it is not that already. It runs under the instance
// locks after every refusal, before any file changes: while the Cluster
// keeps the three-member label the workload admission policy lets the
// platform change that field (infra ADR 0078), and CNPG refuses an
// externalClusters list without the replica.source, which the re-rendered
// file would otherwise leave.
type groupPlan struct {
	cluster, primaryEnv, standby string
}

// rewire reads the primary's Cluster again and sets its replica.source to
// the standby when needed. Its errors wrap errRewireChanged (the Cluster
// is no longer the designated healthy primary, or a test of the
// conditional patch failed), errRewireDenied (admission policy or RBAC)
// or errRewireFailed.
func (g *groupPlan) rewire(ctx context.Context, s *Server, app string) error {
	ns := deploy.Namespace(app)
	rctx, cancel := context.WithTimeout(ctx, DBClusterTimeout)
	p, err := s.d.DBCluster(rctx, g.primaryEnv, ns, g.cluster)
	cancel()
	switch {
	case err != nil || p == nil:
		return fmt.Errorf("%w: read Cluster %s in %s: %v", errRewireFailed, g.cluster, g.primaryEnv, err)
	case !p.Healthy() || p.ReplicaPrimary != p.Self:
		return fmt.Errorf("%w: Cluster %s in %s is no longer the healthy designated primary", errRewireChanged, g.cluster, g.primaryEnv)
	case p.ReplicaSource == g.standby:
		return nil
	case !p.HasExternalCluster(g.standby):
		return fmt.Errorf("%w: the primary %s has no externalCluster %s", errRewireChanged, p.Self, g.standby)
	}
	wctx, cancel := context.WithTimeout(ctx, DBClusterTimeout)
	defer cancel()
	if err := s.d.DBRewire(wctx, g.primaryEnv, ns, g.cluster, p, g.standby); err != nil {
		s.cfg.Log.Warn("database group removal: set the primary's replica.source", "app", app, "env", g.primaryEnv, "source", g.standby, "err", err)
		switch {
		case strings.Contains(err.Error(), "ValidatingAdmissionPolicy") || apierrors.IsForbidden(err):
			return fmt.Errorf("%w: the workload cluster of %s refused to set replica.source of the primary to %s", errRewireDenied, g.primaryEnv, g.standby)
		case apierrors.IsConflict(err) || apierrors.IsInvalid(err) || apierrors.IsNotFound(err):
			return fmt.Errorf("%w: Cluster %s in %s changed while the removal read it", errRewireChanged, g.cluster, g.primaryEnv)
		default:
			return fmt.Errorf("%w: could not set replica.source of the primary in %s to %s", errRewireFailed, g.primaryEnv, g.standby)
		}
	}
	s.cfg.Log.Info("database group removal: the primary follows the remaining replica once demoted", "app", app, "env", g.primaryEnv, "from", p.ReplicaSource, "to", g.standby)
	return nil
}

// prepareGroupRemoval checks the replication of the members of app's
// three-environment database group that remain without env, and makes them
// the pair the files render after the removal (ADR 0064). It answers and
// returns false unless each remaining Cluster is read and healthy, one of
// them is the designated primary (replica.primary is its self), the other
// streams directly from it (its replica.primary and replica.source are the
// primary), and env's Cluster, when it can be read, is not a primary. An
// env whose Cluster cannot be read is no reason to refuse: the gateway
// sends it nothing (the executor checks that), and its database is
// deleted. It changes nothing: the plan it returns is carried out by
// groupPlan.rewire under the instance locks, after every other refusal.
func (s *Server) prepareGroupRemoval(ctx context.Context, w http.ResponseWriter, r *http.Request, app store.App, env string) (*groupPlan, bool) {
	if s.d.DBCluster == nil || s.d.DBRewire == nil {
		s.fail(w, r, http.StatusServiceUnavailable, "db_topology_unknown", "this server cannot read or change the database Clusters in the workload clusters, so it cannot check the replication of the members that remain")
		return nil, false
	}
	list, err := s.d.Store.ListInstanceDeployments(ctx, app.Name, 0)
	if err != nil {
		s.internal(w, r, err)
		return nil, false
	}
	cluster := ""
	if last := lastHealthy(list); last != nil {
		specJSON := last.Spec
		if len(specJSON) == 0 && app.SpecCommit == last.Commit {
			specJSON = app.Spec
		}
		var spec appspec.Spec
		if err := json.Unmarshal(specJSON, &spec); err != nil {
			s.internal(w, r, fmt.Errorf("the recorded platform.yaml of %s: %w", last.ID, err))
			return nil, false
		}
		if i := slices.IndexFunc(spec.Resources, func(res appspec.Resource) bool { return res.Type == appspec.TypePostgres }); i >= 0 {
			cluster = deploy.ClusterName(app.Name, spec.Resources[i].Name)
		}
	}
	unready := func(msg string) (*groupPlan, bool) {
		s.fail(w, r, http.StatusConflict, "db_topology_unready", msg+"; nothing was changed")
		return nil, false
	}
	if cluster == "" {
		return unready(fmt.Sprintf("%s has no healthy deployment with a postgres resource, so the database Clusters of its group are unknown", app.Name))
	}
	ns := deploy.Namespace(app.Name)
	members := slices.Clone(app.Environments)
	status := make([]*cnpg.Status, len(members))
	errs := make([]error, len(members))
	var wg sync.WaitGroup
	for i, m := range members {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, DBClusterTimeout)
			defer cancel()
			status[i], errs[i] = s.d.DBCluster(cctx, m, ns, cluster)
		}()
	}
	wg.Wait()
	primary, standby := -1, -1
	for i, m := range members {
		st := status[i]
		switch {
		case m == env:
			if errs[i] == nil && st != nil && st.IsPrimary() {
				s.fail(w, r, http.StatusConflict, "write_backend", fmt.Sprintf("the database of %s in %s is a primary; migrate it to another environment first", app.Name, env))
				return nil, false
			}
		case errs[i] != nil || st == nil:
			s.cfg.Log.Warn("database group removal: read a Cluster", "app", app.Name, "env", m, "err", errs[i])
			s.fail(w, r, http.StatusBadGateway, "db_topology_unknown", fmt.Sprintf("could not read Cluster %s/%s in %s, which remains in the group; nothing was changed, try again", ns, cluster, m))
			return nil, false
		case !st.Healthy():
			return unready(fmt.Sprintf("Cluster %s in %s is not healthy (%s, %d of %d instances ready)", cluster, m, st.Phase, st.ReadyInstances, st.Instances))
		// Remaining members count as the primary only when designated
		// explicitly (replica.primary is self): every member of a group is
		// drawn with replica.primary. The removed member is refused for
		// IsPrimary, which also takes an empty replica.primary for one.
		case st.ReplicaPrimary == st.Self:
			if primary >= 0 {
				return unready(fmt.Sprintf("both %s and %s are database primaries", members[primary], m))
			}
			primary = i
		default:
			standby = i
		}
	}
	if primary < 0 || standby < 0 {
		return unready(fmt.Sprintf("the members of %s that remain without %s are not one designated primary and one replica", app.Name, env))
	}
	p, rp := status[primary], status[standby]
	if rp.ReplicaPrimary != p.Self || rp.ReplicaSource != p.Self {
		return unready(fmt.Sprintf("the database in %s follows %s through %s, not the primary %s directly", members[standby], rp.ReplicaPrimary, rp.ReplicaSource, p.Self))
	}
	if p.ReplicaSource != rp.Self && !p.HasExternalCluster(rp.Self) {
		return unready(fmt.Sprintf("the primary %s has no externalCluster %s to follow once demoted", p.Self, rp.Self))
	}
	return &groupPlan{cluster: cluster, primaryEnv: members[primary], standby: rp.Self}, true
}

// filesWithoutGateway returns a deployment of the app (all, any instance)
// that went to env healthy with an s3 resource and no public service, or
// nil. Such an instance may have written files to its bucket in env without
// the traffic gateway ever sending it a request, so removing env, which
// deletes that bucket, is refused (ADR 0043). A deployment that recorded no
// platform.yaml is judged by the app's.
func filesWithoutGateway(app store.App, all []store.Deployment, env string) *store.Deployment {
	for i, d := range all {
		if envStatus(d, env) != deploy.EnvHealthy {
			continue
		}
		raw := cmpOrBytes(d.Spec, app.Spec)
		var spec appspec.Spec
		if json.Unmarshal(raw, &spec) != nil {
			continue
		}
		_, public := publicSpecService(spec)
		if !public && slices.ContainsFunc(spec.Resources, func(res appspec.Resource) bool { return res.Type == appspec.TypeS3 }) {
			return &all[i]
		}
	}
	return nil
}

// cmpOrBytes returns a, or b when a is empty.
func cmpOrBytes(a, b []byte) []byte {
	if len(a) > 0 {
		return a
	}
	return b
}

// envNotInApp answers 404 env_not_found for an environment the app is not in.
func (s *Server) envNotInApp(w http.ResponseWriter, r *http.Request, app store.App, env string) {
	s.fail(w, r, http.StatusNotFound, "env_not_found", fmt.Sprintf("app %s is not in environment %q (it is in %s)", app.Name, env, strings.Join(app.Environments, ", ")))
}

// primaryEnvironment answers 409 primary_environment.
func (s *Server) primaryEnvironment(w http.ResponseWriter, r *http.Request, app store.App, env string) {
	s.fail(w, r, http.StatusConflict, "primary_environment", fmt.Sprintf("%s is app %s's first environment, where its database was born primary; it goes only with the app (DELETE /v1/apps/%s)", env, app.Name, app.Name))
}

// appChanged answers 409 app_changed: the app's environments or branches
// changed while the request was handled, and nothing was changed.
func (s *Server) appChanged(w http.ResponseWriter, r *http.Request, app string) {
	s.fail(w, r, http.StatusConflict, "app_changed", fmt.Sprintf("the environments or branches of %s changed meanwhile; read the app and try again", app))
}

// queueEnvironmentDeployment stores and queues a deployment of app's
// default instance to all of its environments that repeats last (trigger
// environment, no build).
func (s *Server) queueEnvironmentDeployment(ctx context.Context, r *http.Request, app store.App, last store.Deployment, spec []byte) (store.Deployment, error) {
	d := newDeployment(app, defaultInstance(app), last.Ref, last.Commit, apiv1.DeploymentTriggerEnvironment, actor(r), storeTime())
	d.Images, d.Spec = last.Images, spec
	if err := s.d.Store.CreateDeployment(ctx, d); err != nil {
		return store.Deployment{}, err
	}
	noteAudit(r, "deployment", d.ID)
	s.enqueue(ctx, d.ID)
	return d, nil
}

// appViewAfter is the app as GET /v1/apps/{name} shows it, without
// readiness, read now, for the answer to a change that already happened:
// when its records cannot be read it logs that and returns the app with
// what the store row says.
func (s *Server) appViewAfter(ctx context.Context, app store.App) apiv1.App {
	branches, deps, db, err := s.appRecords(ctx, app)
	if err == nil {
		var view apiv1.App
		if view, err = s.appView(ctx, app, branches, deps, db); err == nil {
			return view
		}
	}
	s.cfg.Log.Warn("read the app for an answer; answering with less", "app", app.Name, "err", err)
	deploy := s.appDeploy(app, nil, nil, nil)
	return registeredOnly(app.Name, s.specResources(app), &deploy)
}

// newestTo is the newest deployment of list (newest first) that is going
// to env or went there, or nil: an unfinished one that lists env, or a
// finished one that got to env (its status there is not pending). A
// finished deployment that stopped before it wrote env's files, such as a
// failed build, says nothing about env and is passed over.
func newestTo(list []store.Deployment, env string) *store.Deployment {
	for i, d := range list {
		st := envStatus(d, env)
		if st == "" {
			continue
		}
		if !store.DeploymentFinished(d.Status) || st != deploy.EnvPending {
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
// namespace of one of instances, or that move one of them without a
// database (Run.app, ADR 0049), newest first.
func (s *Server) appRuns(ctx context.Context, instances []string) ([]apiv1.Run, error) {
	rows, err := s.d.Store.ListRuns(ctx)
	if err != nil {
		return nil, err
	}
	var out []apiv1.Run
	for _, row := range rows {
		var run apiv1.Run
		if err := json.Unmarshal(row.Doc, &run); err != nil {
			// One bad document must not hold every app's readiness or
			// environments; the runs it could be of are passed over.
			s.cfg.Log.Warn("skip a migration run whose document cannot be read", "run", row.RunID, "err", err)
			continue
		}
		if slices.Contains(instances, run.Source.Namespace) || slices.Contains(instances, run.Target.Namespace) ||
			(run.App != nil && slices.Contains(instances, *run.App)) {
			out = append(out, run)
		}
	}
	return out, nil
}

// migrationBusy says why a migration of app is in progress, or "": an
// active run (runActive) of one of instances, or the traffic gateway
// holding an instance's writes. A gateway that cannot be read is only
// logged: a deployment that reaches it refuses held writes again.
func (s *Server) migrationBusy(ctx context.Context, app string, instances []string) (string, error) {
	runs, err := s.appRuns(ctx, instances)
	if err != nil {
		return "", err
	}
	for _, run := range runs {
		if s.runActive(run) {
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
