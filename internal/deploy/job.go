package deploy

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
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
}

func (j *job) deploy(ctx context.Context) error {
	if j.d.StartedAt == nil {
		now := j.e.now()
		j.d.StartedAt = &now
	}
	msg := "reading platform.yaml at " + short(j.d.Commit)
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
	spec, err := j.spec(ctx, app)
	if err != nil {
		return err
	}
	if err := j.build(ctx, app, spec); err != nil {
		return err
	}
	if err := j.phase(ctx, store.DeployCommitting, "writing "+AppDir(app.Name)+" for "+envNames(j.d.Environments)); err != nil {
		return err
	}
	commit, err := j.commit(ctx, app, spec)
	if err != nil {
		return err
	}
	if err := j.sync(ctx, app, commit); err != nil {
		return err
	}
	if err := j.register(ctx, app, spec); err != nil {
		return err
	}
	done := "every environment is synced and healthy"
	if _, ok := publicService(spec); ok {
		if u := AppURL(j.e.cfg.AppDomain, app.Name); u != "" {
			done += "; " + u
		}
	}
	return j.e.finish(ctx, &j.d, store.DeployHealthy, "", done)
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

// locked runs f under the app's lock, right after a write that proves the
// deployment is still unfinished.
func (j *job) locked(ctx context.Context, app string, f func() error) error {
	l := j.e.lock(app)
	l.Lock()
	defer l.Unlock()
	if err := j.save(ctx); err != nil {
		return err
	}
	return f()
}

// --- building ---

// spec reads and checks platform.yaml at the deployment's commit and makes it
// the app's spec when this is the app's newest deployment.
func (j *job) spec(ctx context.Context, app store.App) (appspec.Spec, error) {
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
	j.updateSpec(ctx, app, *spec)
	return *spec, nil
}

// updateSpec stores spec as the app's spec when the commit differs from the
// stored one and no newer deployment of the app exists. A failure is only
// logged: the deployment does not depend on it.
func (j *job) updateSpec(ctx context.Context, app store.App, spec appspec.Spec) {
	if app.SpecCommit == j.d.Commit {
		return
	}
	latest, err := j.e.cfg.Store.ListDeployments(ctx, app.Name, 1)
	if err != nil || len(latest) == 0 || latest[0].ID != j.d.ID {
		return
	}
	app.Spec, app.SpecCommit = specJSON(spec), j.d.Commit
	if err := j.e.cfg.Store.UpdateApp(ctx, app); err != nil {
		j.e.cfg.Log.Warn("store app spec", "app", app.Name, "err", err)
		return
	}
	j.note(ctx, "platform.yaml at "+short(j.d.Commit)+" is now the app's spec")
}

// build builds the images unless an earlier attempt of this deployment did.
func (j *job) build(ctx context.Context, app store.App, spec appspec.Spec) error {
	if imagesFor(j.d.Images, spec) != nil {
		j.note(ctx, "images were built before the restart; not building again")
		return nil
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

func (j *job) commit(ctx context.Context, app store.App, spec appspec.Spec) (string, error) {
	images := imagesFor(j.d.Images, spec)
	files := map[string][]byte{}
	for _, env := range j.d.Environments {
		b, err := Render(File{App: app.Name, Env: env.Env, Commit: j.d.Commit, Deployment: j.d.ID, Spec: spec, Images: images})
		if err != nil {
			return "", err
		}
		files[FilePath(app.Name, env.Env)] = b
	}
	msg := fmt.Sprintf("deploy %s %s to %s (%s)", app.Name, short(j.d.Commit), envNames(j.d.Environments), j.d.ID)
	var sha string
	var changed bool
	err := j.locked(ctx, app.Name, func() error {
		var err error
		sha, changed, err = j.e.cfg.GitOps.Write(ctx, files, msg)
		return err
	})
	if errors.Is(err, errStopped) {
		return "", err
	}
	if err != nil {
		return "", fmt.Errorf("commit to the gitops repository: %w", err)
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

func (j *job) sync(ctx context.Context, app store.App, commit string) error {
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
	ns := Namespace(app.Name)
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

	deadline := j.e.now().Add(j.e.cfg.SyncTimeout)
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

// register points the gateway at the app's public service in every
// environment of the deployment, keeping the gateway's other environments,
// write backend and read weights as they are.
func (j *job) register(ctx context.Context, app store.App, spec appspec.Spec) error {
	svc, ok := publicService(spec)
	if !ok || j.e.cfg.Gateway == nil {
		return nil
	}
	urls := map[string]string{}
	for i := range j.d.Environments {
		e := &j.d.Environments[i]
		u, err := j.backendURL(ctx, app.Name, e.Env, svc)
		if err != nil {
			return err
		}
		urls[e.Env], e.URL = u, u
	}
	var routed gateway.AppSpec
	var unchanged bool
	err := j.locked(ctx, app.Name, func() error {
		list, err := j.e.cfg.Gateway.Apps(ctx)
		if err != nil {
			return fmt.Errorf("%w: %w", ErrGateway, err)
		}
		var cur *gateway.AppInfo
		for i := range list.Apps {
			if list.Apps[i].Name == app.Name && !list.Apps[i].Default {
				cur = &list.Apps[i]
			}
		}
		if cur != nil && cur.WritesHeld {
			return ErrWritesHeld
		}
		routed = gatewaySpec(cur, app.Environments, urls)
		if cur != nil && maps.Equal(cur.Backends, routed.Backends) && cur.WriteBackend == routed.WriteBackend && sameWeights(cur.ReadWeights, routed.ReadWeights) {
			unchanged = true
			return nil
		}
		err = j.e.cfg.Gateway.PutApp(ctx, app.Name, routed)
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
		return err
	case errors.Is(err, ErrWritesHeld):
		return fmt.Errorf("the traffic gateway holds %s's writes (a migration is switching it), so its backends were not changed; deploy again once the migration is done", app.Name)
	case err != nil:
		return fmt.Errorf("register with the traffic gateway: %w", err)
	}
	where := app.Name
	if u := AppURL(j.e.cfg.AppDomain, app.Name); u != "" {
		where = u
	}
	if unchanged {
		j.note(ctx, "the traffic gateway already sends "+where+" to "+routed.WriteBackend)
	} else {
		j.note(ctx, "the traffic gateway sends "+where+" to "+routed.WriteBackend+" ("+strings.Join(sortedKeys(routed.Backends), ", ")+")")
	}
	return nil
}

// backendURL resolves an environment's URL, retrying until the sync
// deadline: a load balancer can still be getting its address.
func (j *job) backendURL(ctx context.Context, app, env string, svc appspec.Service) (string, error) {
	deadline := j.e.now().Add(j.e.cfg.SyncTimeout)
	for {
		u, err := j.e.cfg.Backends.URL(ctx, app, env, svc)
		if err == nil {
			return u, nil
		}
		if !j.e.now().Before(deadline) {
			return "", fmt.Errorf("find the address of %s in %s: %w", ServiceName(app, svc.Name), env, err)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(j.e.cfg.PollInterval):
		}
	}
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
	out.WriteBackend = sortedKeys(out.Backends)[0]
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
