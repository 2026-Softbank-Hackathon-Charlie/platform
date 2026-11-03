// Package deploy runs the deployments of apps registered from GitHub (ADR
// 0011). A deployment goes through these phases:
//
//	queued      stored by the API, waiting for a worker
//	building    platform.yaml is read and checked at the commit, then one image per service is built and pushed (Builder)
//	committing  apps/<app>/<env>.yaml of every target environment is written to the gitops repository in one commit (GitOps)
//	syncing     Argo CD applies the commit; the executor waits until every environment is Synced and Healthy (Argo)
//
// and ends healthy, failed or superseded. Once every environment is healthy,
// an app with a public service is registered with the traffic gateway.
//
// Everything is recorded in the store: the deployment through
// UpdateDeployment, every step as a DeploymentEvent. UpdateDeployment refuses
// to change a finished deployment, so the worker of a superseded deployment
// stops at its next write. Every side effect outside the store (a gitops
// commit, a gateway change) runs under the app's lock right after such a
// write, so a superseded worker never writes after the deployment that
// superseded it.
package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/gateway"
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
)

// Config configures an Executor.
type Config struct {
	Store   store.DeployStore
	GitHub  GitHub
	Builder Builder
	GitOps  GitOps
	Argo    Argo
	// Gateway registers apps that have a public service. Nil leaves the
	// gateway alone.
	Gateway Gateway
	// Backends resolves the gateway backend URLs. Required with Gateway.
	Backends Backends
	// AppDomain names the app URL https://<app>.<AppDomain> in the events.
	AppDomain string
	// Workers is how many deployments run at once (default 2).
	Workers int
	// SyncTimeout bounds the wait for Argo CD, from the commit until every
	// environment is healthy (default 10m).
	SyncTimeout time.Duration
	// PollInterval is how often Argo CD is read (default 5s).
	PollInterval time.Duration
	// RefreshInterval is how often a refresh is requested again for an
	// Application that has not seen the commit yet (default 30s).
	RefreshInterval time.Duration
	// MaxLogLines bounds the build log lines stored per deployment (default 2000).
	MaxLogLines int
	Log         *slog.Logger
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
	locks   map[string]*sync.Mutex // one per app: serializes the side effects of its deployments
	signal  chan struct{}
	started bool
	wg      sync.WaitGroup
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
	}, nil
}

// Start starts the workers and queues the deployments a previous server left
// unfinished, oldest first, superseding the ones a newer one covers. The
// workers stop when ctx ends; a deployment they were running stays
// unfinished and is resumed by the next Start.
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
	return nil
}

// Enqueue takes a stored deployment: it supersedes the app's older
// unfinished deployments that it covers and queues it. It returns at once.
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
	if err := e.supersede(ctx, d); err != nil {
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

// supersede finishes, as superseded, every unfinished deployment of d's app
// that was created before d and whose environments d all deploys to.
func (e *Executor) supersede(ctx context.Context, d store.Deployment) error {
	list, err := e.cfg.Store.ListDeployments(ctx, d.App, 0)
	if err != nil {
		return fmt.Errorf("list deployments of %s: %w", d.App, err)
	}
	for _, o := range list {
		if o.ID == d.ID || store.DeploymentFinished(o.Status) || !createdBefore(o, d) || !covers(d, o) {
			continue
		}
		if err := e.finish(ctx, &o, store.DeploySuperseded, "", "superseded by deployment "+d.ID); err != nil && !errors.Is(err, errStopped) {
			return err
		}
		e.stop(o.ID)
	}
	return nil
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

// lock returns the app's lock.
func (e *Executor) lock(app string) *sync.Mutex {
	e.mu.Lock()
	defer e.mu.Unlock()
	l, ok := e.locks[app]
	if !ok {
		l = &sync.Mutex{}
		e.locks[app] = l
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
	j := &job{e: e, d: d, contains: map[string]bool{}}
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

// Environment statuses, mirrored from apiv1.DeploymentEnvironmentStatus.
const (
	EnvPending  = "pending"
	EnvSyncing  = "syncing"
	EnvHealthy  = "healthy"
	EnvDegraded = "degraded"
	EnvFailed   = "failed"
)

// RemoveApp unregisters a registered app: it supersedes the app's unfinished
// deployments, commits the removal of apps/<app>/ from the gitops
// repository, removes the app from the traffic gateway and deletes it from
// the store. Each step can be repeated, so a failed removal is retried by
// calling RemoveApp again. A missing app is store.ErrNotFound; a gateway that
// holds the app's writes is ErrWritesHeld, and the app is then kept.
func (e *Executor) RemoveApp(ctx context.Context, name string) error {
	if _, err := e.cfg.Store.GetApp(ctx, name); err != nil {
		return err
	}
	list, err := e.cfg.Store.ListDeployments(ctx, name, 0)
	if err != nil {
		return fmt.Errorf("list deployments of %s: %w", name, err)
	}
	for _, d := range list {
		if store.DeploymentFinished(d.Status) {
			continue
		}
		if err := e.finish(ctx, &d, store.DeploySuperseded, "", "the app is being deleted"); err != nil && !errors.Is(err, errStopped) {
			return err
		}
		e.stop(d.ID)
	}

	l := e.lock(name)
	l.Lock()
	defer l.Unlock()
	if _, _, err := e.cfg.GitOps.Delete(ctx, AppDir(name), "delete app "+name); err != nil {
		return fmt.Errorf("%w: remove %s: %w", ErrGitOps, AppDir(name), err)
	}
	if e.cfg.Gateway != nil {
		err := e.cfg.Gateway.DeleteApp(ctx, name)
		var ae *gateway.APIError
		switch {
		case err == nil, errors.As(err, &ae) && ae.Status == 404:
		case errors.As(err, &ae) && ae.Status == 409:
			return fmt.Errorf("%w: %s", ErrWritesHeld, ae.Message)
		default:
			return fmt.Errorf("%w: delete app %s: %w", ErrGateway, name, err)
		}
	}
	if err := e.cfg.Store.DeleteApp(ctx, name); err != nil {
		return err
	}
	e.cfg.Log.Info("app deleted", "app", name)
	return nil
}

// AppURL is where an app with a public service answers through the gateway,
// or "" without an app domain.
func AppURL(domain, app string) string {
	if domain == "" {
		return ""
	}
	return "https://" + app + "." + domain
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
