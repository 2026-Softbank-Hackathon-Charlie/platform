// Package deploy runs the deployments of apps registered from GitHub (ADR
// 0011). A deployment goes through these phases:
//
//	queued      stored by the API, waiting for a worker
//	building    platform.yaml is read and checked at the commit, then one image per service is built and pushed (Builder)
//	committing  apps/<instance>/<env>.yaml of every target environment is written to the gitops repository in one commit (GitOps)
//	syncing     Argo CD applies the commit; the executor waits until every environment is Synced and Healthy (Argo)
//
// and ends healthy, failed or superseded. Once every environment is healthy,
// an instance with a public service is registered with the traffic gateway,
// as soon as that service answers at its backend address in the
// environments the gateway sends requests to. An environment already in the
// gateway at the same address is not probed; any other one that has not
// answered by then is left out of the gateway, with a message.
//
// Before committing, the executor seals what the instance gets for each
// environment's cluster (ADR 0016, ADR 0019): the key of the instance's
// bucket, made by the environment's storage driver, when platform.yaml has
// an s3 resource, and the app secrets. A redeploy repeats the instance's
// last healthy deployment with the current secrets: it has the images and
// the spec already, so it skips the build and GitHub.
//
// A deployment goes to one instance of its app (ADR 0015): the default
// instance, named after the app, or the instance of a branch added to it,
// named <app>--<slug>. The instance name is the gitops directory, the
// namespace, the Argo CD Applications <instance>-<env> and the gateway app;
// the app name stays the chart's app and the image repository. Instances are
// independent: a deployment supersedes only older deployments of its own
// instance. A redeploy never runs beside another deployment of its
// instance: a deployment that is not a redeploy supersedes every unfinished
// redeploy, older or newer, and a redeploy taken while such a deployment is
// unfinished supersedes itself, as that deployment uses the current secrets
// too.
//
// Everything is recorded in the store: the deployment through
// UpdateDeployment, every step as a DeploymentEvent. UpdateDeployment refuses
// to change a finished deployment, so the worker of a superseded deployment
// stops at its next write. Every side effect outside the store (a gitops
// commit, a gateway change) runs under the instance's lock right after such
// a write, so a superseded worker never writes after the deployment that
// superseded it.
package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/gateway"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/sealing"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/secrets"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/storage"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/appspec"
)

// GitHub reads app repositories through the GitHub App installation. The
// client in internal/github implements it. ResolveRef and ReadFile return an
// error matching fs.ErrNotExist when the ref or the file does not exist.
type GitHub interface {
	// Token returns a short-lived installation token, to clone a repository.
	Token(ctx context.Context) (string, error)
	// ResolveRef resolves a branch, tag or commit sha to a full commit sha.
	ResolveRef(ctx context.Context, repo, ref string) (string, error)
	// ReadFile returns a file of repo at ref.
	ReadFile(ctx context.Context, repo, ref, path string) ([]byte, error)
}

// Gateway is the part of the traffic gateway admin API the executor needs;
// *gateway.Client implements it.
type Gateway interface {
	Apps(ctx context.Context) (gateway.AppList, error)
	PutApp(ctx context.Context, name string, spec gateway.AppSpec) error
	DeleteApp(ctx context.Context, name string) error
}

var (
	// ErrWritesHeld means the traffic gateway holds the app's writes (a
	// migration is switching it), so the executor did not change its routing.
	ErrWritesHeld = errors.New("the traffic gateway holds the app's writes")
	// ErrGitOps wraps a failure to read or write the gitops repository.
	ErrGitOps = errors.New("gitops repository")
	// ErrGateway wraps a failure to reach the traffic gateway.
	ErrGateway = errors.New("traffic gateway")
)

// errStopped means the deployment was finished by someone else (superseded,
// or its app deleted): the worker stops without writing anything.
var errStopped = errors.New("the deployment was finished elsewhere")

// Defaults for Config.
const (
	DefaultWorkers         = 2
	DefaultSyncTimeout     = 10 * time.Minute
	DefaultPollInterval    = 5 * time.Second
	DefaultRefreshInterval = 30 * time.Second
	DefaultMaxLogLines     = 2000
	DefaultDegradedTimeout = time.Minute
)

// Config configures an Executor.
type Config struct {
	Store   Store
	GitHub  GitHub
	Builder Builder
	GitOps  GitOps
	Argo    Argo
	// Gateway registers apps that have a public service. Nil leaves the
	// gateway alone.
	Gateway Gateway
	// Backends resolves the gateway backend URLs. Required with Gateway.
	Backends Backends
	// Probe checks once whether the public service answers at a backend
	// URL, with its healthCheck path or "": the gateway is pointed there
	// only once it does. Nil uses ProbeBackend.
	Probe func(ctx context.Context, backendURL, healthCheck string) error
	// AppDomain names the instance URL https://<instance>.<AppDomain> in
	// the events.
	AppDomain string
	// GKEImageRegistry is the Artifact Registry prefix for gke images. Empty
	// keeps the builder's image addresses in every environment.
	GKEImageRegistry string
	// Workers is how many deployments run at once (default 2).
	Workers int
	// SyncTimeout bounds the wait from the commit until every environment
	// is healthy and the public service answers at its backend addresses
	// (default 10m).
	SyncTimeout time.Duration
	// PollInterval is how often Argo CD is read and a backend that did not
	// answer is probed again (default 5s).
	PollInterval time.Duration
	// RefreshInterval is how often a refresh is requested again for an
	// Application that has not seen the commit yet (default 30s).
	RefreshInterval time.Duration
	// DegradedTimeout is how long an environment may stay Synced and
	// Degraded at the deployment's commit before the deployment fails
	// without waiting for SyncTimeout (default 1m).
	DegradedTimeout time.Duration
	// MaxLogLines bounds the build log lines stored per deployment (default 2000).
	MaxLogLines int
	// Secrets decrypts the app secrets (PLATFORM_SECRETS_KEY); nil fails
	// the deployments of an app that has secrets.
	Secrets *secrets.Cipher
	// Sealer returns the sealer of an environment's cluster; nil (or an
	// error) fails a deployment that has something to seal there.
	Sealer func(env string) (*sealing.Sealer, error)
	// Storage returns an environment's storage driver; nil (or an error)
	// fails a deployment of an app with an s3 resource there, and turns off
	// the cleanup of deleted instances' buckets.
	Storage func(env string) (storage.Driver, error)
	// StorageDriver is the storage driver an environment's configuration
	// names ("" without a storage block), whether or not it could be built.
	// The default instance's postgres resource gets a backup bucket where it
	// is aws (ADR 0025); nil means nowhere.
	StorageDriver func(env string) string
	// CleanupInterval is how often the buckets and keys of deleted
	// instances are deleted again when an earlier try failed (default 5m).
	CleanupInterval time.Duration
	Log             *slog.Logger
}

// Executor runs deployments with a bounded pool of workers. It implements
// api.Deployer and api.AppRemover.
type Executor struct {
	cfg Config
	now func() time.Time

	mu      sync.Mutex
	queue   []string
	queued  map[string]bool
	running map[string]context.CancelFunc
	locks   map[string]*sync.Mutex // one per instance: serializes the side effects of its deployments
	signal  chan struct{}
	started bool
	wg      sync.WaitGroup

	cleanupNow chan struct{} // asks cleanupLoop to run
}

// New checks cfg and returns an executor. Call Start to run it.
func New(cfg Config) (*Executor, error) {
	switch {
	case cfg.Store == nil:
		return nil, errors.New("deploy: Store is required")
	case cfg.GitHub == nil:
		return nil, errors.New("deploy: GitHub is required")
	case cfg.Builder == nil:
		return nil, errors.New("deploy: Builder is required")
	case cfg.GitOps == nil:
		return nil, errors.New("deploy: GitOps is required")
	case cfg.Argo == nil:
		return nil, errors.New("deploy: Argo is required")
	case cfg.Gateway != nil && cfg.Backends == nil:
		return nil, errors.New("deploy: Backends is required with Gateway")
	}
	if cfg.Workers <= 0 {
		cfg.Workers = DefaultWorkers
	}
	if cfg.SyncTimeout <= 0 {
		cfg.SyncTimeout = DefaultSyncTimeout
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = DefaultPollInterval
	}
	if cfg.RefreshInterval <= 0 {
		cfg.RefreshInterval = DefaultRefreshInterval
	}
	if cfg.MaxLogLines <= 0 {
		cfg.MaxLogLines = DefaultMaxLogLines
	}
	if cfg.DegradedTimeout <= 0 {
		cfg.DegradedTimeout = DefaultDegradedTimeout
	}
	if cfg.CleanupInterval <= 0 {
		cfg.CleanupInterval = DefaultCleanupInterval
	}
	if cfg.Probe == nil {
		cfg.Probe = ProbeBackend
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	return &Executor{
		cfg:     cfg,
		now:     func() time.Time { return time.Now().UTC() },
		queued:  map[string]bool{},
		running: map[string]context.CancelFunc{},
		locks:   map[string]*sync.Mutex{},
		signal:  make(chan struct{}, 1),

		cleanupNow: make(chan struct{}, 1),
	}, nil
}

// Start starts the workers and queues the deployments a previous server left
// unfinished, oldest first, superseding the ones a newer one covers. The
// workers stop when ctx ends; a deployment they were running stays
// unfinished and is resumed by the next Start. With storage drivers it also
// starts deleting the buckets and keys of deleted instances, now and every
// CleanupInterval (CleanupStorage).
func (e *Executor) Start(ctx context.Context) error {
	e.mu.Lock()
	if e.started {
		e.mu.Unlock()
		return errors.New("deploy: executor already started")
	}
	e.started = true
	e.mu.Unlock()

	list, err := e.cfg.Store.ListUnfinishedDeployments(ctx)
	if err != nil {
		return fmt.Errorf("list unfinished deployments: %w", err)
	}
	for _, d := range list {
		if err := e.take(ctx, d.ID); err != nil {
			return err
		}
	}
	if len(list) > 0 {
		e.cfg.Log.Info("resuming deployments", "count", len(list))
	}
	for range e.cfg.Workers {
		e.wg.Go(func() { e.work(ctx) })
	}
	if e.cfg.Storage != nil {
		e.wg.Go(func() { e.cleanupLoop(ctx) })
	}
	return nil
}

// Enqueue takes a stored deployment: it supersedes the older unfinished
// deployments of its instance that it covers and queues it. It returns at
// once.
func (e *Executor) Enqueue(ctx context.Context, deploymentID string) error {
	return e.take(ctx, deploymentID)
}

// Wait blocks until every worker has stopped (after the Start context ended)
// or ctx ends.
func (e *Executor) Wait(ctx context.Context) error {
	done := make(chan struct{})
	go func() { e.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (e *Executor) take(ctx context.Context, id string) error {
	d, err := e.cfg.Store.GetDeployment(ctx, id)
	if err != nil {
		return fmt.Errorf("deployment %s: %w", id, err)
	}
	if store.DeploymentFinished(d.Status) {
		return nil
	}
	if superseded, err := e.supersede(ctx, d); err != nil || superseded {
		return err
	}
	e.mu.Lock()
	if !e.queued[id] && e.running[id] == nil {
		e.queued[id] = true
		e.queue = append(e.queue, id)
	}
	e.mu.Unlock()
	e.wake()
	return nil
}

// supersede finishes, as superseded, the unfinished deployments of d's
// instance that d replaces, and reports whether d itself was superseded
// instead. The deployments of the app's other instances go on.
//
//   - d is a redeploy and the instance has an unfinished deployment that is
//     not: d supersedes itself. That deployment seals the current secrets
//     too, and a redeploy beside it could commit the older version after it.
//   - d is not a redeploy: it supersedes every unfinished redeploy, created
//     before or after it, for the same reason.
//   - Otherwise d supersedes the deployments created before it whose
//     environments it all deploys to.
//
// Deployments are stored before they are taken, so of a redeploy and
// another deployment taken at the same time at least one sees the other.
func (e *Executor) supersede(ctx context.Context, d store.Deployment) (bool, error) {
	list, err := e.cfg.Store.ListInstanceDeployments(ctx, d.Instance, 0)
	if err != nil {
		return false, fmt.Errorf("list deployments of %s: %w", d.Instance, err)
	}
	list = slices.DeleteFunc(list, func(o store.Deployment) bool { return o.ID == d.ID || store.DeploymentFinished(o.Status) })
	// An environment deployment repeats the last healthy one too, so it
	// must never replace a newer commit either (ADR 0037).
	redeploy := func(o store.Deployment) bool { return o.Trigger == TriggerRedeploy || o.Trigger == TriggerEnvironment }
	if redeploy(d) {
		if i := slices.IndexFunc(list, func(o store.Deployment) bool { return !redeploy(o) }); i >= 0 {
			msg := fmt.Sprintf("superseded by deployment %s, which started meanwhile; it uses the current secrets", list[i].ID)
			if err := e.finish(ctx, &d, store.DeploySuperseded, "", msg); err != nil && !errors.Is(err, errStopped) {
				return false, err
			}
			e.stop(d.ID)
			return true, nil
		}
	}
	for _, o := range list {
		replaces := redeploy(o) && !redeploy(d) || createdBefore(o, d) && covers(d, o)
		if !replaces {
			continue
		}
		if err := e.finish(ctx, &o, store.DeploySuperseded, "", "superseded by deployment "+d.ID); err != nil && !errors.Is(err, errStopped) {
			return false, err
		}
		e.stop(o.ID)
	}
	return false, nil
}

func createdBefore(a, b store.Deployment) bool {
	if !a.CreatedAt.Equal(b.CreatedAt) {
		return a.CreatedAt.Before(b.CreatedAt)
	}
	return a.ID < b.ID
}

// covers reports whether d deploys to every environment of o.
func covers(d, o store.Deployment) bool {
	for _, oe := range o.Environments {
		if !slices.ContainsFunc(d.Environments, func(de store.DeploymentEnvironment) bool { return de.Env == oe.Env }) {
			return false
		}
	}
	return true
}

// stop cancels the worker of a deployment that someone else finished, so a
// long build does not run on for nothing. The worker writes nothing more:
// its next write would fail anyway.
func (e *Executor) stop(id string) {
	e.mu.Lock()
	cancel := e.running[id]
	e.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (e *Executor) wake() {
	select {
	case e.signal <- struct{}{}:
	default:
	}
}

func (e *Executor) work(ctx context.Context) {
	for {
		id, jobCtx, ok := e.next(ctx)
		if !ok {
			return
		}
		e.run(ctx, jobCtx, id)
		e.mu.Lock()
		e.running[id]()
		delete(e.running, id)
		e.mu.Unlock()
	}
}

// next takes the first queued deployment and marks it running, with a
// context that stop cancels.
func (e *Executor) next(ctx context.Context) (string, context.Context, bool) {
	for {
		if ctx.Err() != nil {
			return "", nil, false
		}
		e.mu.Lock()
		if len(e.queue) > 0 {
			id := e.queue[0]
			e.queue = e.queue[1:]
			delete(e.queued, id)
			jobCtx, cancel := context.WithCancel(ctx)
			e.running[id] = cancel
			more := len(e.queue) > 0
			e.mu.Unlock()
			if more {
				e.wake() // the signal holds one wake-up; pass it on to the next worker
			}
			return id, jobCtx, true
		}
		e.mu.Unlock()
		select {
		case <-ctx.Done():
			return "", nil, false
		case <-e.signal:
		}
	}
}

// lock returns the instance's lock.
func (e *Executor) lock(instance string) *sync.Mutex {
	e.mu.Lock()
	defer e.mu.Unlock()
	l, ok := e.locks[instance]
	if !ok {
		l = &sync.Mutex{}
		e.locks[instance] = l
	}
	return l
}

// run deploys one deployment. A failure is recorded as failed, unless the
// server is shutting down (the deployment is resumed after the restart) or
// someone else finished the deployment.
func (e *Executor) run(parent, ctx context.Context, id string) {
	d, err := e.cfg.Store.GetDeployment(ctx, id)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			e.cfg.Log.Error("read deployment", "deployment", id, "err", err)
		}
		return
	}
	if store.DeploymentFinished(d.Status) {
		return
	}
	j := &job{e: e, d: d, contains: map[string]bool{}, degradedSince: map[string]time.Time{}}
	err = j.deploy(ctx)
	switch {
	case err == nil:
	case errors.Is(err, errStopped):
		e.cfg.Log.Info("deployment stopped: finished elsewhere", "deployment", id)
	case parent.Err() != nil:
		e.cfg.Log.Info("deployment interrupted by shutdown; it resumes after the restart", "deployment", id, "status", j.d.Status)
	case ctx.Err() != nil:
		// Cancelled by stop: superseded or its app deleted.
	default:
		msg := err.Error()
		if err := e.finish(ctx, &j.d, store.DeployFailed, msg, msg); err != nil && !errors.Is(err, errStopped) {
			e.cfg.Log.Error("record failed deployment", "deployment", id, "err", err)
		}
	}
}

// finish records a final status and then the done event. errStopped means
// the deployment was already finished (or is gone) and nothing was written.
func (e *Executor) finish(ctx context.Context, d *store.Deployment, status, errMsg, doneMsg string) error {
	now := e.now()
	next := *d
	next.Status, next.Error, next.FinishedAt = status, errMsg, &now
	stored, err := e.update(ctx, next)
	if err != nil {
		return err
	}
	*d = stored
	e.event(ctx, store.DeploymentEvent{DeploymentID: d.ID, Type: EventDone, Phase: status, Message: doneMsg})
	return nil
}

// update writes d. errStopped when it is finished or gone.
func (e *Executor) update(ctx context.Context, d store.Deployment) (store.Deployment, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	stored, err := e.cfg.Store.UpdateDeployment(ctx, d)
	if errors.Is(err, store.ErrConflict) || errors.Is(err, store.ErrNotFound) {
		return store.Deployment{}, errStopped
	}
	if err != nil {
		return store.Deployment{}, fmt.Errorf("store deployment %s: %w", d.ID, err)
	}
	return stored, nil
}

// event stores an event; a failure is logged, not returned.
func (e *Executor) event(ctx context.Context, ev store.DeploymentEvent) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if ev.Time.IsZero() {
		ev.Time = e.now()
	}
	if _, err := e.cfg.Store.AppendDeploymentEvent(ctx, ev); err != nil && !errors.Is(err, store.ErrNotFound) {
		e.cfg.Log.Error("store deployment event", "deployment", ev.DeploymentID, "type", ev.Type, "err", err)
	}
}

// Deployment event types, mirrored from apiv1.DeploymentEventType.
const (
	EventPhase       = "phase"
	EventLog         = "log"
	EventEnvironment = "environment"
	EventDone        = "done"
)

// TriggerRedeploy is the trigger of a redeploy (apiv1.DeploymentTriggerRedeploy).
const TriggerRedeploy = "redeploy"

// TriggerEnvironment is the trigger of the deployment that takes the
// default instance to the app's environments after one was added or
// removed (apiv1.DeploymentTriggerEnvironment, ADR 0037). Like a redeploy
// it repeats the last healthy deployment's images without a build, and
// supersedes and is superseded as a redeploy: it never replaces a newer
// commit, and a push or manual deployment, which goes to all of the app's
// environments, replaces it.
const TriggerEnvironment = "environment"

// Environment statuses, mirrored from apiv1.DeploymentEnvironmentStatus.
const (
	EnvPending  = "pending"
	EnvSyncing  = "syncing"
	EnvHealthy  = "healthy"
	EnvDegraded = "degraded"
	EnvFailed   = "failed"
)

// removeAppAttempts bounds how often RemoveApp starts over because the
// app's branches changed while it removed their instances.
const removeAppAttempts = 3

// RemoveApp unregisters a registered app with all its instances: it
// supersedes the app's unfinished deployments, commits the removal of
// apps/<instance>/ of the default instance and of every added branch's
// instance from the gitops repository in one commit, removes every instance
// from the traffic gateway and deletes the app, with its branches and
// deployments, from the store. A branch added or removed meanwhile makes it
// start over, a few times at most. Each step can be repeated, so a failed
// removal is retried by calling RemoveApp again. A missing app is
// store.ErrNotFound; a gateway that holds the writes of one of the instances
// is ErrWritesHeld, and the app is then kept.
func (e *Executor) RemoveApp(ctx context.Context, name string) error {
	if _, err := e.cfg.Store.GetApp(ctx, name); err != nil {
		return err
	}
	for attempt := 1; ; attempt++ {
		instances, err := e.instances(ctx, name)
		if err != nil {
			return err
		}
		list, err := e.cfg.Store.ListDeployments(ctx, name, 0)
		if err != nil {
			return fmt.Errorf("list deployments of %s: %w", name, err)
		}
		if err := e.supersedeAll(ctx, list, "the app is being deleted"); err != nil {
			return err
		}
		changed := false
		err = e.removeInstances(ctx, instances, "delete app "+name, func() error {
			// An instance added meanwhile is not locked or removed yet.
			now, err := e.instances(ctx, name)
			if err != nil {
				return err
			}
			if changed = !slices.Equal(now, instances); changed {
				return nil
			}
			return e.cfg.Store.DeleteApp(ctx, name)
		})
		switch {
		case err != nil:
			return err
		case !changed:
			e.cfg.Log.Info("app deleted", "app", name, "instances", instances)
			e.wakeCleanup() // its buckets and keys
			return nil
		case attempt == removeAppAttempts:
			return fmt.Errorf("the branches of %s kept changing while it was being deleted; delete it again", name)
		}
	}
}

// instances lists an app's instances: the default one, then the added
// branches' in branch order.
func (e *Executor) instances(ctx context.Context, app string) ([]string, error) {
	branches, err := e.cfg.Store.ListAppBranches(ctx, app)
	if err != nil {
		return nil, fmt.Errorf("list the branches of %s: %w", app, err)
	}
	out := []string{app}
	for _, b := range branches {
		out = append(out, b.Instance)
	}
	return out, nil
}

// supersedeAll finishes the unfinished deployments of list as superseded,
// with msg, and stops their workers.
func (e *Executor) supersedeAll(ctx context.Context, list []store.Deployment, msg string) error {
	for _, d := range list {
		if store.DeploymentFinished(d.Status) {
			continue
		}
		if err := e.finish(ctx, &d, store.DeploySuperseded, "", msg); err != nil && !errors.Is(err, errStopped) {
			return err
		}
		e.stop(d.ID)
	}
	return nil
}

// removeInstances commits the removal of the instances' gitops directories
// in one commit, removes the instances from the traffic gateway and then
// calls forget to delete their records from the store. It holds every
// instance's lock until forget returns, so no deployment of the instances
// commits or registers after the removal: one superseded before the call
// stops at its next write, and one created meanwhile waits for the lock and
// then finds itself deleted. A gateway that holds an instance's writes is
// ErrWritesHeld, and forget is not called.
func (e *Executor) removeInstances(ctx context.Context, instances []string, message string, forget func() error) error {
	// One lock order for every caller, and each lock once.
	instances = slices.Compact(slices.Sorted(slices.Values(instances)))
	dirs := make([]string, len(instances))
	for i, in := range instances {
		l := e.lock(in)
		l.Lock()
		defer l.Unlock()
		dirs[i] = AppDir(in)
	}
	if _, _, err := e.cfg.GitOps.Delete(ctx, dirs, message); err != nil {
		return fmt.Errorf("%w: remove %s: %w", ErrGitOps, strings.Join(dirs, ", "), err)
	}
	// The ApplicationSet deletes the Applications once it reads the commit.
	// Without a refresh its repo server answers from a revision cache, which
	// leaves the instances running for minutes. A refused refresh only costs
	// time.
	if err := e.cfg.Argo.RefreshApplicationSet(ctx); err != nil && ctx.Err() == nil {
		e.cfg.Log.Warn("argo cd applicationset refresh after a removal", "instances", instances, "err", err)
	}
	if e.cfg.Gateway != nil {
		for _, in := range instances {
			err := e.cfg.Gateway.DeleteApp(ctx, in)
			var ae *gateway.APIError
			switch {
			case err == nil, errors.As(err, &ae) && ae.Status == 404:
			case errors.As(err, &ae) && ae.Status == 409:
				return fmt.Errorf("%w: %s", ErrWritesHeld, ae.Message)
			default:
				return fmt.Errorf("%w: delete app %s: %w", ErrGateway, in, err)
			}
		}
	}
	return forget()
}

var (
	// ErrDeploymentInProgress means a deployment of one of the app's
	// instances is not finished, so RemoveEnvironment changed nothing.
	ErrDeploymentInProgress = errors.New("a deployment of the app is not finished")
	// ErrEnvironmentInUse means the traffic gateway sends requests of one of
	// the app's instances to the environment, so RemoveEnvironment changed
	// nothing.
	ErrEnvironmentInUse = errors.New("the traffic gateway sends requests to the environment")
	// ErrNotInEnvironment means the app is not in the environment (any
	// more), so RemoveEnvironment changed nothing.
	ErrNotInEnvironment = errors.New("the app is not in the environment")
	// ErrPrimaryEnvironment means the environment is the app's first, where
	// its database was born primary, so RemoveEnvironment changed nothing.
	ErrPrimaryEnvironment = errors.New("the environment is the app's first")
	// ErrAppChanged means the app's branches or environments changed while
	// RemoveEnvironment waited for the locks or stored the result; reading
	// the app and asking again settles it.
	ErrAppChanged = errors.New("the app changed meanwhile")
)

// RemoveEnvironment takes env out of a registered app (ADR 0037). Under the
// locks of every instance of the app, and only while none of them has an
// unfinished deployment, it takes env's backend out of every instance's
// gateway app, commits the removal of apps/<instance>/<env>.yaml of every
// instance in one commit, counts up the database pair's PeerGeneration when
// env is its peer (a peer added back gets a new backup path), and then sets
// the app's environments to the others. The gateway goes first so that no
// request is sent to an environment whose workloads are going. The
// ApplicationSet deletes the Applications <instance>-<env> with what they
// created, as for a deleted app, and CleanupStorage deletes the instances'
// buckets and keys there. Each step can be repeated, so a failed removal is
// retried by calling it again; the environments change last. It refuses
// (ErrEnvironmentInUse) when the gateway sends an instance's writes or
// reads to env, and ErrWritesHeld when it holds an instance's writes. A
// missing app is store.ErrNotFound, an app without env ErrNotInEnvironment,
// env as the app's first environment ErrPrimaryEnvironment, and branches or
// environments that changed meanwhile ErrAppChanged; nothing is changed
// then. The caller checks the rest (migrations, which environment was ever
// written).
func (e *Executor) RemoveEnvironment(ctx context.Context, name, env string) error {
	app, err := e.cfg.Store.GetApp(ctx, name)
	if err != nil {
		return err
	}
	switch i := slices.Index(app.Environments, env); {
	case i < 0:
		return fmt.Errorf("%w: app %s is not in environment %s", ErrNotInEnvironment, name, env)
	case i == 0:
		return fmt.Errorf("%w: %s is app %s's first environment, where its database was born primary", ErrPrimaryEnvironment, env, name)
	}
	instances, err := e.instances(ctx, name)
	if err != nil {
		return err
	}
	locked := slices.Compact(slices.Sorted(slices.Values(instances)))
	for _, in := range locked {
		l := e.lock(in)
		l.Lock()
		defer l.Unlock()
	}
	// Read again under the locks: what was checked before them may have
	// changed, and nothing of these instances changes while they are held.
	if app, err = e.cfg.Store.GetApp(ctx, name); err != nil {
		return err
	}
	if now, err := e.instances(ctx, name); err != nil {
		return err
	} else if !slices.Equal(now, instances) {
		return fmt.Errorf("%w: the branches of %s changed; remove the environment again", ErrAppChanged, name)
	}
	if !slices.Contains(app.Environments, env) {
		return fmt.Errorf("%w: app %s is not in environment %s", ErrNotInEnvironment, name, env)
	}
	list, err := e.cfg.Store.ListDeployments(ctx, name, 0)
	if err != nil {
		return fmt.Errorf("list deployments of %s: %w", name, err)
	}
	for _, d := range list {
		if !store.DeploymentFinished(d.Status) {
			return fmt.Errorf("%w: deployment %s of %s is %s", ErrDeploymentInProgress, d.ID, d.Instance, d.Status)
		}
	}
	if e.cfg.Gateway != nil {
		all, err := e.cfg.Gateway.Apps(ctx)
		if err != nil {
			return fmt.Errorf("%w: %w", ErrGateway, err)
		}
		routes := map[string]gateway.AppInfo{}
		for _, a := range all.Apps {
			if !a.Default && slices.Contains(instances, a.Name) {
				routes[a.Name] = a
			}
		}
		for _, in := range instances {
			cur, ok := routes[in]
			switch {
			case !ok:
			case cur.WritesHeld:
				return fmt.Errorf("%w: %s", ErrWritesHeld, writesHeld(in))
			case cur.WriteBackend == env || cur.ReadWeights[env] > 0:
				return fmt.Errorf("%w: the traffic gateway sends the requests of %s to %s", ErrEnvironmentInUse, in, env)
			}
		}
		// The weight 0 backends go first: no request is sent to an
		// environment whose files are already gone.
		for _, in := range instances {
			cur, ok := routes[in]
			if !ok || cur.Backends[env] == "" {
				continue
			}
			if hidden := hiddenPasswords(&cur, map[string]string{env: ""}); len(hidden) > 0 {
				return fmt.Errorf("%w: the gateway hides the password in the URL of the %s backend of %s, and sending that URL back would replace the password; set it again without a password in its URL, then remove the environment again", ErrGateway, strings.Join(hidden, ", "), in)
			}
			next := gateway.AppSpec{Backends: maps.Clone(cur.Backends), WriteBackend: cur.WriteBackend}
			delete(next.Backends, env)
			if weights := positive(cur.ReadWeights); len(weights) > 0 {
				delete(weights, env)
				next.ReadWeights = weights
			}
			err := e.cfg.Gateway.PutApp(ctx, in, next)
			var ae *gateway.APIError
			switch {
			case errors.As(err, &ae) && ae.Status == 409:
				return fmt.Errorf("%w: %s", ErrWritesHeld, ae.Message)
			case err != nil:
				return fmt.Errorf("%w: take %s out of %s: %w", ErrGateway, env, in, err)
			}
		}
	}
	files := make([]string, len(instances))
	for i, in := range instances {
		files[i] = FilePath(in, env)
	}
	if _, _, err := e.cfg.GitOps.Delete(ctx, files, "remove environment "+env+" of app "+name); err != nil {
		return fmt.Errorf("%w: remove %s: %w", ErrGitOps, strings.Join(files, ", "), err)
	}
	if err := e.cfg.Argo.RefreshApplicationSet(ctx); err != nil && ctx.Err() == nil {
		e.cfg.Log.Warn("argo cd applicationset refresh after an environment removal", "app", name, "env", env, "err", err)
	}
	// The peer's database leaves its WAL in the backup bucket, so a peer
	// added back archives under a new path (ADR 0037).
	db, err := e.cfg.Store.GetAppDatabase(ctx, name)
	switch {
	case errors.Is(err, store.ErrNotFound):
	case err != nil:
		return fmt.Errorf("read the database pair of %s: %w", name, err)
	case db.PeerEnv == env:
		gen, err := e.cfg.Store.NextPeerGeneration(ctx, name)
		if err != nil {
			return fmt.Errorf("count up the peer generation of %s: %w", name, err)
		}
		e.cfg.Log.Info("database pair peer removed; a peer added back gets a new backup path", "app", name, "env", env, "peerGeneration", gen)
	}
	rest := slices.DeleteFunc(slices.Clone(app.Environments), func(e string) bool { return e == env })
	if err := e.cfg.Store.SetAppEnvironments(ctx, name, app.Environments, rest); errors.Is(err, store.ErrConflict) {
		return fmt.Errorf("%w: %w", ErrAppChanged, err)
	} else if err != nil {
		return fmt.Errorf("store the environments of %s: %w", name, err)
	}
	e.cfg.Log.Info("environment removed from app", "app", name, "env", env, "instances", instances)
	e.wakeCleanup() // the instances' buckets and keys there
	return nil
}

// ApplicationExists reports whether Argo CD still has the Application
// name, such as one an environment removal is deleting (ADR 0037).
func (e *Executor) ApplicationExists(ctx context.Context, name string) (bool, error) {
	st, err := e.cfg.Argo.Status(ctx, name)
	if err != nil {
		return false, err
	}
	return st.Exists, nil
}

// AppURL is where an instance with a public service answers through the
// gateway, or "" without an app domain. The default instance is named after
// the app.
func AppURL(domain, instance string) string {
	if domain == "" {
		return ""
	}
	return "https://" + instance + "." + domain
}

// publicService returns the spec's public service.
func publicService(s appspec.Spec) (appspec.Service, bool) {
	for _, sv := range s.Services {
		if sv.Public {
			return sv, true
		}
	}
	return appspec.Service{}, false
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

func specJSON(s appspec.Spec) []byte {
	b, _ := json.Marshal(s)
	return b
}

func envNames(list []store.DeploymentEnvironment) string {
	names := make([]string, len(list))
	for i, e := range list {
		names[i] = e.Env
	}
	return strings.Join(names, ", ")
}
