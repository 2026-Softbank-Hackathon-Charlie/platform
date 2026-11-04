package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"
	"sync"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/gateway"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/appspec"
)

// job is one deployment being run by a worker. Only that worker touches it.
type job struct {
	e *Executor
	d store.Deployment // as last stored

	logLines int
	contains map[string]bool // gitops revision -> whether it includes the commit

	syncDeadline  time.Time            // SyncTimeout after the commit, for syncing and the backends (addresses, then an answer)
	degradedSince map[string]time.Time // by environment: since when Argo CD reports it Synced and Degraded
	keys          []sealedKey          // the bucket keys this deployment sealed, by environment
}

func (j *job) deploy(ctx context.Context) error {
	if j.d.StartedAt == nil {
		now := j.e.now()
		j.d.StartedAt = &now
	}
	msg := "reading platform.yaml at " + short(j.d.Commit)
	if j.d.Trigger == TriggerRedeploy {
		msg = "redeploying " + short(j.d.Commit) + " with its images and the current secrets; no build"
	}
	if j.d.Status != store.DeployQueued {
		msg = "resumed after a restart (was " + j.d.Status + "); " + msg
	}
	if err := j.phase(ctx, store.DeployBuilding, msg); err != nil {
		return err
	}
	app, err := j.e.cfg.Store.GetApp(ctx, j.d.App)
	if errors.Is(err, store.ErrNotFound) {
		return errStopped // deleted: its deployments are gone too
	}
	if err != nil {
		return fmt.Errorf("read app %s: %w", j.d.App, err)
	}
	branch, err := j.addedBranch(ctx, app)
	if err != nil {
		return err
	}
	var spec appspec.Spec
	if len(j.d.Spec) > 0 {
		// A redeploy, or a deployment resumed after it read platform.yaml.
		if err := json.Unmarshal(j.d.Spec, &spec); err != nil {
			return fmt.Errorf("the recorded platform.yaml of %s does not decode: %w", j.d.ID, err)
		}
	} else {
		if spec, err = j.spec(ctx, app, branch); err != nil {
			return err
		}
		j.d.Spec = specJSON(spec) // stored with the next write
	}
	if err := j.build(ctx, app, spec); err != nil {
		return err
	}
	if err := j.phase(ctx, store.DeployCommitting, "writing "+AppDir(j.d.Instance)+" for "+envNames(j.d.Environments)); err != nil {
		return err
	}
	commit, err := j.commit(ctx, app, spec)
	if err != nil {
		return err
	}
	if err := j.sync(ctx, commit); err != nil {
		return err
	}
	left, err := j.register(ctx, app, spec)
	if err != nil {
		return err
	}
	j.deleteOldKeys(ctx)
	done := "every environment is synced and healthy"
	if _, ok := publicService(spec); ok {
		if u := AppURL(j.e.cfg.AppDomain, j.d.Instance); u != "" {
			done += "; " + u
		}
	}
	if len(left) > 0 {
		done += "; the traffic gateway leaves out " + strings.Join(left, ", ") + ", where the public service did not answer"
	}
	return j.e.finish(ctx, &j.d, store.DeployHealthy, "", done)
}

// addedBranch returns the added branch whose instance the deployment goes
// to, or nil for the app's default instance (the instance named after the
// app). A branch the app no longer has fails the deployment.
func (j *job) addedBranch(ctx context.Context, app store.App) (*store.AppBranch, error) {
	if j.d.Instance == app.Name {
		return nil, nil
	}
	list, err := j.e.cfg.Store.ListAppBranches(ctx, app.Name)
	if err != nil {
		return nil, fmt.Errorf("read the branches of %s: %w", app.Name, err)
	}
	for _, b := range list {
		if b.Branch == j.d.Branch && b.Instance == j.d.Instance {
			return &b, nil
		}
	}
	return nil, branchGone(app.Name, j.d.Branch)
}

func branchGone(app, branch string) error {
	return fmt.Errorf("branch %s is no longer deployed by app %s", branch, app)
}

// phase moves the deployment to status and records a phase event.
func (j *job) phase(ctx context.Context, status, msg string) error {
	j.d.Status = status
	if err := j.save(ctx); err != nil {
		return err
	}
	j.e.event(ctx, store.DeploymentEvent{DeploymentID: j.d.ID, Type: EventPhase, Phase: status, Message: msg})
	return nil
}

// save writes the deployment; errStopped when someone else finished it.
func (j *job) save(ctx context.Context) error {
	stored, err := j.e.update(ctx, j.d)
	if err != nil {
		return err
	}
	j.d = stored
	return nil
}

// log records one line of output in the current phase. Past MaxLogLines the
// rest is dropped, with one line saying so.
func (j *job) log(ctx context.Context, line string) {
	line = strings.TrimRight(line, "\r\n")
	j.logLines++
	switch {
	case j.logLines == j.e.cfg.MaxLogLines+1:
		line = fmt.Sprintf("... more output was dropped; a deployment keeps %d lines", j.e.cfg.MaxLogLines)
	case j.logLines > j.e.cfg.MaxLogLines:
		return
	}
	j.e.event(ctx, store.DeploymentEvent{DeploymentID: j.d.ID, Type: EventLog, Phase: j.d.Status, Message: line})
}

// note records a line of the platform's own that is not counted against
// MaxLogLines.
func (j *job) note(ctx context.Context, msg string) {
	j.e.event(ctx, store.DeploymentEvent{DeploymentID: j.d.ID, Type: EventLog, Phase: j.d.Status, Message: msg})
}

// locked runs f under the instance's lock, right after a write that proves
// the deployment is still unfinished.
func (j *job) locked(ctx context.Context, f func() error) error {
	l := j.e.lock(j.d.Instance)
	l.Lock()
	defer l.Unlock()
	if err := j.save(ctx); err != nil {
		return err
	}
	return f()
}

// --- building ---

// spec reads and checks platform.yaml at the deployment's commit and makes it
// the instance's spec when this is the instance's newest deployment.
func (j *job) spec(ctx context.Context, app store.App, branch *store.AppBranch) (appspec.Spec, error) {
	v, spec, err := CheckSpec(ctx, j.e.cfg.GitHub, app.Repo, j.d.Commit)
	if err != nil {
		return appspec.Spec{}, fmt.Errorf("read platform.yaml at %s: %w", short(j.d.Commit), err)
	}
	if !v.Valid {
		msgs := make([]string, len(v.Errors))
		for i, fe := range v.Errors {
			msgs[i] = appspec.FieldError{Path: fe.Path, Message: fe.Message}.Error()
			j.note(ctx, "platform.yaml: "+msgs[i])
		}
		return appspec.Spec{}, fmt.Errorf("platform.yaml at %s is invalid: %s", short(j.d.Commit), strings.Join(msgs, "; "))
	}
	if spec.Name != app.Name {
		return appspec.Spec{}, fmt.Errorf("platform.yaml at %s names the app %q, but it is registered as %q", short(j.d.Commit), spec.Name, app.Name)
	}
	if err := j.updateSpec(ctx, app, branch, *spec); err != nil {
		return appspec.Spec{}, err
	}
	return *spec, nil
}

// updateSpec stores spec as the spec of the deployment's instance, the app's
// for the default instance (branch nil) and the added branch's otherwise,
// when the commit differs from the stored one and no newer deployment of the
// instance exists. A failure to store it is only logged, as the deployment
// does not depend on it; an added branch that is gone fails the deployment.
func (j *job) updateSpec(ctx context.Context, app store.App, branch *store.AppBranch, spec appspec.Spec) error {
	stored := app.SpecCommit
	if branch != nil {
		stored = branch.SpecCommit
	}
	if stored == j.d.Commit {
		return nil
	}
	latest, err := j.e.cfg.Store.ListInstanceDeployments(ctx, j.d.Instance, 1)
	if err != nil || len(latest) == 0 || latest[0].ID != j.d.ID {
		return nil
	}
	whose := "the app's"
	if branch == nil {
		app.Spec, app.SpecCommit = specJSON(spec), j.d.Commit
		err = j.e.cfg.Store.UpdateApp(ctx, app)
	} else {
		b := *branch
		b.Spec, b.SpecCommit = specJSON(spec), j.d.Commit
		err = j.e.cfg.Store.UpdateAppBranch(ctx, b)
		if errors.Is(err, store.ErrNotFound) {
			return branchGone(app.Name, b.Branch)
		}
		whose = "branch " + b.Branch + "'s"
	}
	if err != nil {
		j.e.cfg.Log.Warn("store the spec", "app", app.Name, "instance", j.d.Instance, "err", err)
		return nil
	}
	j.note(ctx, "platform.yaml at "+short(j.d.Commit)+" is now "+whose+" spec")
	return nil
}

// build builds the images unless the deployment has them: a redeploy, or
// an earlier attempt of this deployment.
func (j *job) build(ctx context.Context, app store.App, spec appspec.Spec) error {
	if imagesFor(j.d.Images, spec) != nil {
		if j.d.Trigger == TriggerRedeploy {
			j.note(ctx, "using the images of the last healthy deployment: "+imageServices(j.d.Images))
		} else {
			j.note(ctx, "images were built before the restart; not building again")
		}
		return nil
	}
	if j.d.Trigger == TriggerRedeploy {
		return errors.New("a redeploy has no images for every service; deploy the branch instead")
	}
	token, err := j.e.cfg.GitHub.Token(ctx)
	if err != nil {
		return fmt.Errorf("get a GitHub token to clone %s: %w", app.Repo, err)
	}
	res, err := j.e.cfg.Builder.Build(ctx, BuildRequest{
		DeploymentID: j.d.ID,
		App:          app.Name,
		Repo:         app.Repo,
		Commit:       j.d.Commit,
		Services:     spec.Services,
		Token:        token,
	}, BuildHooks{
		Started: func(buildID string) error {
			j.d.BuildID = buildID
			if err := j.save(ctx); err != nil {
				return err
			}
			j.note(ctx, "build "+buildID+" started")
			return nil
		},
		Log: func(line string) { j.log(ctx, line) },
	})
	if errors.Is(err, errStopped) {
		return err
	}
	if err != nil {
		return fmt.Errorf("build: %w", err)
	}
	if imagesFor(res.Images, spec) == nil {
		return fmt.Errorf("build: the builder returned images for %s, not for every service", imageServices(res.Images))
	}
	if res.BuildID != "" {
		j.d.BuildID = res.BuildID
	}
	j.d.Images = res.Images
	if err := j.save(ctx); err != nil {
		return err
	}
	for _, im := range res.Images {
		j.note(ctx, "built "+im.Service+": "+im.Image)
	}
	return nil
}

// imagesFor maps every service of spec to its image, or returns nil when one
// is missing or not pinned by digest.
func imagesFor(images []store.DeploymentImage, spec appspec.Spec) map[string]string {
	out := map[string]string{}
	for _, im := range images {
		if strings.Contains(im.Image, "@sha256:") {
			out[im.Service] = im.Image
		}
	}
	for _, sv := range spec.Services {
		if out[sv.Name] == "" {
			return nil
		}
	}
	return out
}

func imageServices(images []store.DeploymentImage) string {
	if len(images) == 0 {
		return "no service"
	}
	names := make([]string, len(images))
	for i, im := range images {
		names[i] = im.Service
	}
	return strings.Join(names, ", ")
}

// --- committing ---

// commit seals what the instance gets and writes its gitops files, all
// under the instance's lock.
func (j *job) commit(ctx context.Context, app store.App, spec appspec.Spec) (string, error) {
	images := imagesFor(j.d.Images, spec)
	msg := fmt.Sprintf("deploy %s %s to %s (%s)", j.d.Instance, short(j.d.Commit), envNames(j.d.Environments), j.d.ID)
	var files map[string][]byte
	var sha string
	var changed bool
	err := j.locked(ctx, func() error {
		var err error
		if files, err = j.sealedFiles(ctx, app, spec, images); err != nil {
			return err
		}
		if sha, changed, err = j.e.cfg.GitOps.Write(ctx, files, msg); err != nil {
			return fmt.Errorf("commit to the gitops repository: %w", err)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if changed {
		j.note(ctx, "committed "+strings.Join(sortedKeys(files), ", ")+" as "+short(sha))
	} else {
		j.note(ctx, "the gitops repository already has these files at "+short(sha))
	}
	return sha, nil
}

// --- syncing ---

// envState is one environment's progress, as the deployment records it.
type envState struct {
	status, sync, health, message string
}

func (j *job) sync(ctx context.Context, commit string) error {
	for i := range j.d.Environments {
		e := &j.d.Environments[i]
		e.Status, e.SyncStatus, e.HealthStatus = EnvSyncing, "", ""
		e.Message = "committed " + short(commit) + "; waiting for Argo CD"
	}
	if err := j.phase(ctx, store.DeploySyncing, "waiting for Argo CD to apply "+short(commit)); err != nil {
		return err
	}
	argo := j.e.cfg.Argo
	// A new app has no Application until the ApplicationSet runs. It runs
	// on its own every few minutes, so a refused refresh only costs time.
	if err := argo.RefreshApplicationSet(ctx); err != nil && ctx.Err() == nil {
		j.note(ctx, "could not ask the Argo CD ApplicationSet to refresh, so a new Application appears on its next run: "+err.Error())
	}
	ns := Namespace(j.d.Instance)
	refreshed := map[string]time.Time{}
	refresh := func(env string) {
		name := ApplicationName(ns, env)
		refreshed[env] = j.e.now()
		if err := argo.Refresh(ctx, name); err != nil && ctx.Err() == nil {
			j.e.cfg.Log.Debug("argo cd refresh", "application", name, "err", err)
		}
	}
	for _, e := range j.d.Environments {
		refresh(e.Env)
	}

	j.syncDeadline = j.e.now().Add(j.e.cfg.SyncTimeout)
	deadline := j.syncDeadline
	tick := time.NewTicker(j.e.cfg.PollInterval)
	defer tick.Stop()
	for {
		healthy, failed := 0, []string{}
		var changed []string
		for i := range j.d.Environments {
			e := &j.d.Environments[i]
			if e.Status == EnvHealthy || e.Status == EnvFailed {
				if e.Status == EnvHealthy {
					healthy++
				} else {
					failed = append(failed, e.Env+": "+e.Message)
				}
				continue
			}
			st := j.envStatus(ctx, ApplicationName(ns, e.Env), commit)
			if st.waitingForCommit && j.e.now().Sub(refreshed[e.Env]) >= j.e.cfg.RefreshInterval {
				refresh(e.Env)
			}
			next := envState{status: st.status, sync: st.sync, health: st.health, message: st.message}
			if (envState{e.Status, e.SyncStatus, e.HealthStatus, e.Message}) != next {
				e.Status, e.SyncStatus, e.HealthStatus, e.Message = next.status, next.sync, next.health, next.message
				changed = append(changed, e.Env)
			}
			switch e.Status {
			case EnvHealthy:
				healthy++
			case EnvFailed:
				failed = append(failed, e.Env+": "+e.Message)
			}
			// Degraded at the commit for DegradedTimeout: it is not coming
			// back on its own (a crashing pod, a progress deadline, a
			// SealedSecret the controller cannot open), so fail now.
			if e.Status != EnvDegraded {
				delete(j.degradedSince, e.Env)
			} else if since, ok := j.degradedSince[e.Env]; !ok {
				j.degradedSince[e.Env] = j.e.now()
			} else if j.e.now().Sub(since) >= j.e.cfg.DegradedTimeout {
				failed = append(failed, fmt.Sprintf("%s: Degraded for %s: %s", e.Env, j.e.cfg.DegradedTimeout, e.Message))
			}
		}
		if len(changed) > 0 {
			if err := j.save(ctx); err != nil {
				return err
			}
			for _, name := range changed {
				j.envEvent(ctx, name)
			}
		}
		if healthy == len(j.d.Environments) {
			return nil
		}
		if len(failed) > 0 {
			return fmt.Errorf("Argo CD could not deploy %s", strings.Join(failed, "; "))
		}
		if !j.e.now().Before(deadline) {
			return j.timedOut(ctx)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}

func (j *job) envEvent(ctx context.Context, env string) {
	for _, e := range j.d.Environments {
		if e.Env != env {
			continue
		}
		msg := e.Status
		if e.SyncStatus != "" || e.HealthStatus != "" {
			msg = strings.Trim(e.SyncStatus+", "+e.HealthStatus, ", ")
		}
		if e.Message != "" {
			msg += ": " + e.Message
		}
		j.e.event(ctx, store.DeploymentEvent{DeploymentID: j.d.ID, Type: EventEnvironment, Phase: j.d.Status, Env: env, Message: msg})
	}
}

// timedOut fails the environments that are not healthy (a degraded one stays
// degraded) and returns the deployment's error.
func (j *job) timedOut(ctx context.Context) error {
	var waiting []string
	var changed []string
	for i := range j.d.Environments {
		e := &j.d.Environments[i]
		if e.Status == EnvHealthy {
			continue
		}
		detail := strings.Trim(e.SyncStatus+", "+e.HealthStatus, ", ")
		if detail == "" {
			detail = e.Message
		}
		waiting = append(waiting, e.Env+" ("+detail+")")
		if e.Status != EnvDegraded {
			e.Status = EnvFailed
			e.Message = fmt.Sprintf("not healthy within %s: %s", j.e.cfg.SyncTimeout, e.Message)
			changed = append(changed, e.Env)
		}
	}
	if len(changed) > 0 {
		if err := j.save(ctx); err != nil {
			return err
		}
		for _, name := range changed {
			j.envEvent(ctx, name)
		}
	}
	return fmt.Errorf("Argo CD did not report every environment healthy within %s: %s", j.e.cfg.SyncTimeout, strings.Join(waiting, "; "))
}

type argoView struct {
	status, sync, health, message string
	waitingForCommit              bool
}

// envStatus reads the Application and decides the environment's state.
func (j *job) envStatus(ctx context.Context, name, commit string) argoView {
	st, err := j.e.cfg.Argo.Status(ctx, name)
	if err != nil {
		return argoView{status: EnvSyncing, message: "could not read Application " + name + ": " + err.Error(), waitingForCommit: true}
	}
	if !st.Exists {
		return argoView{status: EnvSyncing, message: "waiting for Argo CD to create Application " + name, waitingForCommit: true}
	}
	v := argoView{sync: st.Sync, health: st.Health}
	if st.OperationFailed() && j.includesAny(ctx, st.OperationRevisions, commit) {
		v.status, v.message = EnvFailed, "sync failed: "+st.OperationMessage
		return v
	}
	if !j.includesAny(ctx, st.Revisions, commit) {
		v.status, v.waitingForCommit = EnvSyncing, true
		v.message = "Argo CD is at revision " + shortAll(st.Revisions) + "; waiting for " + short(commit)
		if len(st.Revisions) == 0 {
			v.message = "Argo CD has not synced yet; waiting for " + short(commit)
		}
		return v
	}
	switch {
	case st.Sync == SyncSynced && st.Health == HealthHealthy:
		v.status = EnvHealthy
	case st.Sync == SyncSynced && st.Health == HealthDegraded:
		v.status, v.message = EnvDegraded, st.HealthMessage
	default:
		v.status, v.message = EnvSyncing, st.HealthMessage
	}
	return v
}

// includesAny reports whether one of an Application's revisions (in any
// order: a multi-source Application lists the chart's commit too) contains
// the gitops commit.
func (j *job) includesAny(ctx context.Context, revs []string, commit string) bool {
	for _, r := range revs {
		if j.includes(ctx, r, commit) {
			return true
		}
	}
	return false
}

func shortAll(revs []string) string {
	out := make([]string, len(revs))
	for i, r := range revs {
		out[i] = short(r)
	}
	return strings.Join(out, ", ")
}

// includes reports whether gitops revision rev contains commit. A later
// commit of another app moves every Application to that revision, so equal
// is not required.
func (j *job) includes(ctx context.Context, rev, commit string) bool {
	if rev == "" {
		return false
	}
	if rev == commit {
		return true
	}
	if ok, seen := j.contains[rev]; seen {
		return ok
	}
	ok, err := j.e.cfg.GitOps.Contains(ctx, rev, commit)
	if err != nil {
		return false // asked again on the next poll
	}
	j.contains[rev] = ok
	return ok
}

// --- the gateway ---

// register points the gateway app named after the instance at the public
// service in the environments of the deployment, keeping the gateway's
// other environments, write backend and read weights as they are. A backend
// is added or changed only once the service answers there; one the gateway
// already has is not probed again. If an environment the gateway sends
// requests to does not answer in time, the deployment fails and the gateway
// is not changed. Any other environment that has not answered by then is
// left out (a route it had stays) and returned.
func (j *job) register(ctx context.Context, app store.App, spec appspec.Spec) ([]string, error) {
	svc, ok := publicService(spec)
	if !ok || j.e.cfg.Gateway == nil {
		return nil, nil
	}
	instance := j.d.Instance
	urls := map[string]string{}
	for _, e := range j.d.Environments {
		u, err := j.backendURL(ctx, app.Name, instance, e.Env, svc)
		if err != nil {
			return nil, err
		}
		urls[e.Env] = u
	}
	cur, err := j.gatewayApp(ctx)
	if err != nil {
		return nil, fmt.Errorf("register with the traffic gateway: %w", err)
	}
	if cur != nil && cur.WritesHeld {
		return nil, writesHeld(instance)
	}
	traffic := receivers(gatewaySpec(cur, app.Environments, urls))
	var probe []string
	for _, e := range j.d.Environments {
		if cur == nil || cur.Backends[e.Env] != urls[e.Env] {
			probe = append(probe, e.Env)
		}
	}
	silent, err := j.waitForBackends(ctx, probe, traffic, urls, svc)
	if err != nil {
		return nil, err
	}
	var dead, left []string
	var changed []string
	for i := range j.d.Environments {
		e := &j.d.Environments[i]
		reason, ok := silent[e.Env]
		if !ok {
			e.URL = urls[e.Env]
			continue
		}
		e.URL = ""
		where := fmt.Sprintf("at %s in %s (%s)", urls[e.Env]+svc.HealthCheck, e.Env, reason)
		if contains(traffic, e.Env) {
			dead = append(dead, where)
			e.Status = EnvFailed
			e.Message = fmt.Sprintf("the public service %s did not answer at %s within %s of the commit: %s", svc.Name, urls[e.Env]+svc.HealthCheck, j.e.cfg.SyncTimeout, reason)
		} else {
			left = append(left, e.Env)
			e.Message = fmt.Sprintf("the public service %s had not answered at %s (%s) when the environments that get requests had, so the traffic gateway was not pointed at it; the next deployment probes it again", svc.Name, urls[e.Env]+svc.HealthCheck, reason)
		}
		changed = append(changed, e.Env)
	}
	if len(changed) > 0 {
		if err := j.save(ctx); err != nil {
			return nil, err
		}
		for _, name := range changed {
			j.envEvent(ctx, name)
		}
	}
	if len(dead) > 0 {
		return nil, fmt.Errorf("the public service %s did not answer within %s of the commit %s, so the traffic gateway was not changed", svc.Name, j.e.cfg.SyncTimeout, strings.Join(dead, "; "))
	}
	for env := range silent {
		delete(urls, env)
	}
	var routed gateway.AppSpec
	var unchanged bool
	err = j.locked(ctx, func() error {
		cur, err := j.gatewayApp(ctx)
		if err != nil {
			return err
		}
		if cur != nil && cur.WritesHeld {
			return ErrWritesHeld
		}
		routed = gatewaySpec(cur, app.Environments, urls)
		if len(routed.Backends) == 0 {
			return fmt.Errorf("no environment of %s answered", instance)
		}
		// A migration may have moved requests while the backends were probed.
		for _, env := range receivers(routed) {
			if _, ok := silent[env]; ok {
				return fmt.Errorf("it now sends requests to %s, where the public service did not answer, so it was not changed", env)
			}
		}
		if cur != nil && maps.Equal(cur.Backends, routed.Backends) && cur.WriteBackend == routed.WriteBackend && sameWeights(cur.ReadWeights, routed.ReadWeights) {
			unchanged = true
			return nil
		}
		err = j.e.cfg.Gateway.PutApp(ctx, instance, routed)
		var ae *gateway.APIError
		if errors.As(err, &ae) && ae.Status == 409 {
			return ErrWritesHeld
		}
		if err != nil {
			return fmt.Errorf("%w: %w", ErrGateway, err)
		}
		return nil
	})
	switch {
	case errors.Is(err, errStopped):
		return nil, err
	case errors.Is(err, ErrWritesHeld):
		return nil, writesHeld(instance)
	case err != nil:
		return nil, fmt.Errorf("register with the traffic gateway: %w", err)
	}
	where := instance
	if u := AppURL(j.e.cfg.AppDomain, instance); u != "" {
		where = u
	}
	if unchanged {
		j.note(ctx, "the traffic gateway already sends "+where+" to "+routed.WriteBackend)
	} else {
		j.note(ctx, "the traffic gateway sends "+where+" to "+routed.WriteBackend+" ("+strings.Join(sortedKeys(routed.Backends), ", ")+")")
	}
	if len(left) > 0 {
		j.note(ctx, "left out of the traffic gateway, as the public service did not answer there: "+strings.Join(left, ", "))
	}
	return left, nil
}

func writesHeld(instance string) error {
	return fmt.Errorf("the traffic gateway holds %s's writes (a migration is switching it), so its backends were not changed; deploy again once the migration is done", instance)
}

// gatewayApp returns the gateway's app named after the instance, or nil
// when the gateway has none.
func (j *job) gatewayApp(ctx context.Context) (*gateway.AppInfo, error) {
	list, err := j.e.cfg.Gateway.Apps(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrGateway, err)
	}
	for i := range list.Apps {
		if list.Apps[i].Name == j.d.Instance && !list.Apps[i].Default {
			return &list.Apps[i], nil
		}
	}
	return nil, nil
}

// receivers lists the backends a gateway spec sends requests to: the write
// backend and those with a read weight.
func receivers(s gateway.AppSpec) []string {
	out := []string{s.WriteBackend}
	for name, w := range s.ReadWeights {
		if w > 0 && name != s.WriteBackend {
			out = append(out, name)
		}
	}
	return out
}

// backendURL resolves an environment's URL, retrying until the sync
// deadline set at the commit (a load balancer can still be getting its
// address), so syncing and this wait together take at most SyncTimeout.
func (j *job) backendURL(ctx context.Context, app, instance, env string, svc appspec.Service) (string, error) {
	deadline := j.syncDeadline
	for {
		u, err := j.e.cfg.Backends.URL(ctx, app, instance, env, svc)
		if err == nil {
			return u, nil
		}
		if !j.e.now().Before(deadline) {
			return "", fmt.Errorf("find the address of %s/%s in %s: %w", Namespace(instance), ServiceName(app, svc.Name), env, err)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(j.e.cfg.PollInterval):
		}
	}
}

// waitForBackends probes the public service at the backend URLs of envs,
// all at once, every PollInterval until each answers or the sync deadline
// set at the commit passes. Argo CD reports a LoadBalancer Service healthy
// once it has an address, but AWS can take minutes more to provision the
// load balancer behind it, and the gateway answers 502 until it does. The
// environments of need (those that get requests) are waited for until the
// deadline; once none of them is left, the others get two more
// PollIntervals at most. Each is probed at least once, and the last round
// can end up to one probe after the deadline. It returns why each
// environment that had not answered did not. A reason is logged the first
// time an environment gives it, and a line once an environment answers
// after waiting.
func (j *job) waitForBackends(ctx context.Context, envs, need []string, urls map[string]string, svc appspec.Service) (map[string]string, error) {
	start := j.e.now()
	stop := j.syncDeadline
	last := map[string]string{}
	seen := map[[2]string]bool{} // env, reason
	for len(envs) > 0 {
		errs := make([]error, len(envs))
		var wg sync.WaitGroup
		for i, env := range envs {
			wg.Go(func() { errs[i] = j.e.cfg.Probe(ctx, urls[env], svc.HealthCheck) })
		}
		wg.Wait()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		var waiting []string
		needed := false
		for i, env := range envs {
			target := urls[env] + svc.HealthCheck
			if errs[i] == nil {
				if last[env] != "" {
					j.note(ctx, fmt.Sprintf("the %s backend %s answered after %s", env, target, j.e.now().Sub(start).Round(time.Second)))
				}
				delete(last, env)
				continue
			}
			reason := errs[i].Error()
			last[env] = reason
			if !seen[[2]string{env, reason}] {
				seen[[2]string{env, reason}] = true
				j.note(ctx, "waiting for the "+env+" backend "+target+" to answer: "+reason)
			}
			waiting = append(waiting, env)
			needed = needed || contains(need, env)
		}
		envs = waiting
		if grace := j.e.now().Add(2 * j.e.cfg.PollInterval); !needed && grace.Before(stop) {
			stop = grace
		}
		left := stop.Sub(j.e.now())
		if len(envs) == 0 || left <= 0 {
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(min(left, j.e.cfg.PollInterval)):
		}
	}
	return last, nil
}

// gatewaySpec merges the deployment's backend URLs into the app's current
// gateway routing. Backends of environments the app no longer has are
// dropped. The write backend and read weights stay when they still name
// backends, so a deployment never moves traffic a migration moved; a new app
// writes to the first of its environments.
func gatewaySpec(cur *gateway.AppInfo, appEnvs []string, urls map[string]string) gateway.AppSpec {
	out := gateway.AppSpec{Backends: map[string]string{}}
	if cur != nil {
		for name, u := range cur.Backends {
			if contains(appEnvs, name) {
				out.Backends[name] = u
			}
		}
	}
	maps.Copy(out.Backends, urls)
	if cur != nil && out.Backends[cur.WriteBackend] != "" {
		out.WriteBackend = cur.WriteBackend
		weights := positive(cur.ReadWeights)
		for name := range weights {
			if out.Backends[name] == "" {
				return out // reads went to a backend that is gone: all reads to the write backend
			}
		}
		if len(weights) > 0 {
			out.ReadWeights = weights
		}
		return out
	}
	for _, name := range appEnvs {
		if out.Backends[name] != "" {
			out.WriteBackend = name
			return out
		}
	}
	if len(out.Backends) > 0 {
		out.WriteBackend = sortedKeys(out.Backends)[0]
	}
	return out
}

// sameWeights compares read weights. The gateway lists every backend, with
// 0 for the ones that get no reads.
func sameWeights(cur, next map[string]int) bool {
	return maps.Equal(positive(cur), positive(next))
}

func positive(m map[string]int) map[string]int {
	out := map[string]int{}
	for k, v := range m {
		if v > 0 {
			out[k] = v
		}
	}
	return out
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
