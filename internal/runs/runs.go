// Package runs executes migration-engine runs inside the platform API and
// fans their events out to subscribers.
package runs

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/engine"
	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"
)

// Engine is the part of *engine.Engine the runner needs.
type Engine interface {
	RunID() string
	Run(ctx context.Context) error
	State() engine.State
	Close()
}

// Factory builds an engine for a plan. Production uses engine.New.
type Factory func(p *plan.Plan, opts engine.Options) (Engine, error)

// EngineFactory adapts engine.New.
func EngineFactory(p *plan.Plan, opts engine.Options) (Engine, error) { return engine.New(p, opts) }

var (
	ErrNotFound = errors.New("run not found")
	// ErrEngineInit means the engine could not be built (e.g. unknown kube context).
	ErrEngineInit = errors.New("could not initialize the migration engine")
	// ErrBusy means another active run already uses one of the plan's clusters.
	ErrBusy = errors.New("another migration is active on the same cluster")
)

// SubscriberBuffer is how many events a slow subscriber may lag before it is dropped.
const SubscriberBuffer = 256

type run struct {
	eng      Engine
	clusters []string
	done     chan struct{}
	subs     map[chan engine.Event]struct{}
}

// Runner starts runs and tracks them for the lifetime of the process.
// TODO: reload finished runs from the state directory after a restart.
type Runner struct {
	stateDir string
	factory  Factory
	ctx      context.Context // runs outlive the HTTP request that started them

	mu   sync.Mutex
	runs map[string]*run
	wg   sync.WaitGroup
}

// New returns a Runner. ctx bounds every run; cancelling it lets the engine
// apply its own failure rules (roll back before promotion, halt after).
func New(ctx context.Context, stateDir string, factory Factory) *Runner {
	return &Runner{ctx: ctx, stateDir: stateDir, factory: factory, runs: map[string]*run{}}
}

func clusterKeys(p *plan.Plan) []string {
	k := func(e plan.Endpoint) string { return e.Context + "/" + e.Namespace + "/" + e.Cluster }
	return []string{k(p.Source), k(p.Target)}
}

// Start creates and launches a run. It returns once the run is accepted, not when it finishes.
func (r *Runner) Start(p *plan.Plan) (string, error) {
	keys := clusterKeys(p)
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, x := range r.runs {
		if !isDone(x) && overlaps(x.clusters, keys) {
			return "", fmt.Errorf("%w (run %s)", ErrBusy, id)
		}
	}
	x := &run{clusters: keys, done: make(chan struct{}), subs: map[chan engine.Event]struct{}{}}
	eng, err := r.factory(p, engine.Options{StateDir: r.stateDir, Emit: r.emit})
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrEngineInit, err)
	}
	x.eng = eng
	id := eng.RunID()
	r.runs[id] = x

	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		_ = eng.Run(r.ctx) // the outcome is in the engine state
		eng.Close()
		r.mu.Lock()
		close(x.done)
		for ch := range x.subs {
			close(ch)
		}
		x.subs = nil
		r.mu.Unlock()
	}()
	return id, nil
}

func (r *Runner) emit(ev engine.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	x, ok := r.runs[ev.RunID]
	if !ok {
		return
	}
	for ch := range x.subs {
		select {
		case ch <- ev:
		default: // too slow: drop; the client reconnects and gets a fresh snapshot
			delete(x.subs, ch)
			close(ch)
		}
	}
}

// Get returns a snapshot of a run's state.
func (r *Runner) Get(id string) (engine.State, error) {
	r.mu.Lock()
	x, ok := r.runs[id]
	r.mu.Unlock()
	if !ok {
		return engine.State{}, ErrNotFound
	}
	return x.eng.State(), nil
}

// List returns snapshots of all runs, newest first.
func (r *Runner) List() []engine.State {
	r.mu.Lock()
	engs := make([]Engine, 0, len(r.runs))
	for _, x := range r.runs {
		engs = append(engs, x.eng)
	}
	r.mu.Unlock()
	out := make([]engine.State, len(engs))
	for i, e := range engs {
		out[i] = e.State()
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

// Subscribe returns a channel of live events. The channel closes when the run
// finishes or the subscriber falls too far behind. Subscribe before reading a
// snapshot so no event is missed between the two.
func (r *Runner) Subscribe(id string) (<-chan engine.Event, func(), error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	x, ok := r.runs[id]
	if !ok {
		return nil, nil, ErrNotFound
	}
	ch := make(chan engine.Event, SubscriberBuffer)
	if isDone(x) {
		close(ch)
		return ch, func() {}, nil
	}
	x.subs[ch] = struct{}{}
	cancel := func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if _, ok := x.subs[ch]; ok {
			delete(x.subs, ch)
			close(ch)
		}
	}
	return ch, cancel, nil
}

// Wait blocks until every run has finished or ctx ends.
func (r *Runner) Wait(ctx context.Context) error {
	done := make(chan struct{})
	go func() { r.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Active reports how many runs are still executing.
func (r *Runner) Active() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, x := range r.runs {
		if !isDone(x) {
			n++
		}
	}
	return n
}

func isDone(x *run) bool {
	select {
	case <-x.done:
		return true
	default:
		return false
	}
}

func overlaps(a, b []string) bool {
	for _, x := range a {
		for _, y := range b {
			if x == y {
				return true
			}
		}
	}
	return false
}
