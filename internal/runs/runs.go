// Package runs executes migration-engine runs inside the platform API.
//
// A run goes through four phases:
//
//	sync             precheck, prepare, presync, delta   (users keep using the source)
//	canary           optional: a share of reads goes to the target, gated on its error rate and p95
//	awaiting_switch  data is in sync; an operator approves the cutover (unless autoSwitch)
//	cutover, verify  fence ... finalize, executed by the engine with its rollback boundary
//	observe          optional: the target is watched against the gate; a breach can start the rollback (observe.go)
//
// The engine decides what a failure may undo. The runner only decides when to
// call it, and records everything (run document and numbered events) in the
// store so lists, event replay and the result matrix survive a restart.
package runs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/engine"
	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

// Engine is the part of *engine.Engine the runner needs.
//
// The runner calls RunStep for every step, including the cutover steps, and
// reads State only from the goroutine that calls RunStep, between steps. The
// engine's State returns a shallow copy whose step records the engine keeps
// writing while a step runs, so reading it from anywhere else is a data race.
type Engine interface {
	RunID() string
	RunStep(ctx context.Context, name string) error
	// ResumeHalted checks that a halted run's promotion went through and lets
	// RunStep continue it (ADR 0013 of the engine).
	ResumeHalted(ctx context.Context) error
	State() engine.State
	Close()
}

// Factory builds an engine for a plan. Production uses EngineFactory.
type Factory func(p *plan.Plan, opts engine.Options) (Engine, error)

// EngineFactory adapts engine.New.
func EngineFactory(p *plan.Plan, opts engine.Options) (Engine, error) { return engine.New(p, opts) }

// Traffic is what the runner needs from the traffic gateway admin API.
type Traffic interface {
	State(ctx context.Context, window time.Duration) (apiv1.TrafficState, error)
	SetReadWeights(ctx context.Context, weights map[string]int) error
}

var (
	ErrNotFound = errors.New("run not found")
	// ErrEngineInit means the engine could not be built (e.g. unknown kube context).
	ErrEngineInit = errors.New("could not initialize the migration engine")
	// ErrBusy means another active run already uses one of the plan's clusters.
	ErrBusy = errors.New("another migration is active on the same cluster")
	// ErrWrongStatus means the run is not in a status that allows the request.
	ErrWrongStatus = errors.New("run is not in the required status")
	// ErrInvalid means the start options cannot be used with this plan or server.
	ErrInvalid = errors.New("invalid migration options")
)

// SubscriberBuffer is how many events a slow subscriber may lag before it is
// dropped. A dropped client reconnects with Last-Event-ID and replays from the store.
const SubscriberBuffer = 256

// Platform event kinds, next to the engine's own kinds.
const (
	EventStatusChanged = "status_changed"
	EventCanaryStep    = "canary_step"
	EventGateResult    = "gate_result"
)

var phaseRank = map[apiv1.RunPhase]int{
	apiv1.RunPhaseSync: 1, apiv1.RunPhaseCanary: 2, apiv1.RunPhaseAwaitingSwitch: 3,
	apiv1.RunPhaseCutover: 4, apiv1.RunPhaseVerify: 5, apiv1.RunPhaseObserve: 6, apiv1.RunPhaseDone: 7,
}

var syncSteps = []string{engine.StepPrecheck, engine.StepPrepare, engine.StepPresync, engine.StepDelta}

// Config configures a Runner.
type Config struct {
	StateDir string
	Store    store.Store
	Factory  Factory
	// Traffic is the gateway admin client. Nil means no gateway: canary is
	// refused and request counts are not measured.
	Traffic Traffic
	Log     *slog.Logger
	// PollInterval is how often live request counts are refreshed (default 2s).
	PollInterval time.Duration
	// MinCanaryStep is the shortest allowed canary step (default 5s; tests lower it).
	MinCanaryStep time.Duration
	// TrafficAt returns the admin client for a plan's gateway.url, which the
	// plan policy allows to be the gateway root or one app's scope under it
	// (ADR 0012). A run with a gateway.mode http plan uses it, so its canary,
	// request counts and observe act on the app the engine holds and routes.
	// Nil uses Traffic for every run.
	TrafficAt func(gatewayURL string) Traffic
}

// StartOptions describe a new run.
type StartOptions struct {
	PlanRef string
	Plan    *plan.Plan
	// Source and Target are the plan's endpoints as its summary shows them
	// (plans.Reserved): built from the plan as submitted, never from Plan,
	// whose DSNs hold expanded secrets.
	Source, Target apiv1.PlanEndpoint
	Actor          string
	AutoSwitch     bool
	Canary         *apiv1.CanaryOptions
	Observe        *apiv1.ObserveOptions
	RollbackOf     string // the run this run rolls back, if any
}

// Runner starts runs and tracks the active ones.
type Runner struct {
	cfg Config
	ctx context.Context // parent of every run; runs outlive the HTTP request that started them
	now func() time.Time

	mu   sync.Mutex
	live map[string]*live
	wg   sync.WaitGroup

	stopOnce sync.Once
	stopCh   chan struct{} // closed by InterruptWaiting: runs stop waiting (canary, switch approval, observe)

	autoRollback AutoRollbackFunc // guarded by mu; see SetAutoRollback
}

// New returns a Runner. Cancelling ctx stops every run; the engine then applies
// its own failure rules (roll back before promotion, halt after).
func New(ctx context.Context, cfg Config) *Runner {
	if cfg.Factory == nil {
		cfg.Factory = EngineFactory
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 2 * time.Second
	}
	if cfg.MinCanaryStep <= 0 {
		cfg.MinCanaryStep = 5 * time.Second
	}
	return &Runner{cfg: cfg, ctx: ctx, now: func() time.Time { return time.Now().UTC() }, live: map[string]*live{}, stopCh: make(chan struct{})}
}

// live is an active run.
type live struct {
	id       string
	plan     *plan.Plan
	eng      Engine
	clusters []string
	stream   string // the written-records stream verify reads, if any (written.go)
	cancel   context.CancelFunc
	done     chan struct{} // closed once everything is persisted and subscribers are closed
	traffic  Traffic       // the gateway scope of this run (Runner.traffic); nil = no gateway
	// resumeHalted: the run halted after promotion and an operator resumed
	// it; drive asks the engine to confirm the promotion first.
	resumeHalted bool

	mu          sync.Mutex // guards the fields below
	doc         apiv1.Run
	subs        map[chan apiv1.RunEvent]struct{}
	switchCh    chan struct{} // closed by Switch
	switched    bool
	abortReq    bool
	interrupt   bool                           // stopped by a server shutdown, not by an operator
	readsMoved  bool                           // the canary changed read weights; put them back unless traffic moved to the target
	trafficBase map[string]apiv1.BackendTotals // gateway totals when this attempt started; nil = not measured
	prevReq     int64                          // counts from earlier attempts of a resumed run
	prevFail    int64

	q queue
}

// --- starting ---

// Start validates the options, creates the engine and the run record, and
// launches the run in the background.
func (r *Runner) Start(ctx context.Context, o StartOptions) (apiv1.Run, error) {
	canary, err := r.canaryOptions(o.Plan, o.Canary)
	if err != nil {
		return apiv1.Run{}, err
	}
	observe, err := r.observeOptions(o.Plan, o.Observe)
	if err != nil {
		return apiv1.Run{}, err
	}
	now := r.now()
	doc := apiv1.Run{
		PlanRef:    o.PlanRef,
		PlanId:     o.Plan.ID,
		Source:     o.Source,
		Target:     o.Target,
		Status:     apiv1.RunStatusRunning,
		Phase:      apiv1.RunPhaseSync,
		AutoSwitch: o.AutoSwitch,
		StartedBy:  o.Actor,
		StartedAt:  now,
		UpdatedAt:  now,
	}
	if canary != nil {
		doc.Canary = &apiv1.CanaryResult{Options: *canary, Steps: []apiv1.CanaryStep{}}
	}
	if observe != nil {
		doc.Observe = &apiv1.ObserveResult{Options: *observe, Steps: []apiv1.ObserveStep{}}
	}
	if o.RollbackOf != "" {
		doc.RollbackOf = &o.RollbackOf
	}
	return r.launch(ctx, o.Plan, "", doc, false)
}

// launch creates the engine (new run when runID is empty, resumed otherwise),
// stores the run document and starts the background goroutines.
func (r *Runner) launch(ctx context.Context, p *plan.Plan, runID string, doc apiv1.Run, resumeHalted bool) (apiv1.Run, error) {
	if err := r.requireWritten(ctx, p); err != nil {
		return apiv1.Run{}, err
	}
	keys := clusterKeys(p)
	l := &live{
		plan:     p,
		clusters: keys,
		traffic:  r.traffic(p),
		done:     make(chan struct{}),
		subs:     map[chan apiv1.RunEvent]struct{}{},
		switchCh: make(chan struct{}),
	}
	l.q.init()
	l.resumeHalted = resumeHalted

	r.mu.Lock()
	defer r.mu.Unlock()
	for id, x := range r.live {
		if overlaps(x.clusters, keys) {
			return apiv1.Run{}, fmt.Errorf("%w (run %s)", ErrBusy, id)
		}
	}
	eng, err := r.cfg.Factory(p, engine.Options{StateDir: r.cfg.StateDir, RunID: runID, Emit: l.emitEngine})
	if err != nil {
		return apiv1.Run{}, fmt.Errorf("%w: %w", ErrEngineInit, err)
	}
	l.eng, l.id = eng, eng.RunID()
	r.useWrittenStream(l)
	doc.RunId = l.id
	if doc.Result.Requests != nil {
		l.prevReq = *doc.Result.Requests
	}
	if doc.Result.FailedRequests != nil {
		l.prevFail = *doc.Result.FailedRequests
	}
	doc.Engine = engineState(eng.State())
	l.doc = doc

	row, err := runRow(doc)
	if err == nil {
		if runID == "" {
			err = r.cfg.Store.CreateRun(ctx, row)
		} else {
			err = r.cfg.Store.UpdateRun(ctx, row)
		}
	}
	if err != nil {
		eng.Close()
		return apiv1.Run{}, err
	}
	r.live[l.id] = l

	runCtx, cancel := context.WithCancel(r.ctx)
	l.cancel = cancel
	r.wg.Add(2)
	go func() { defer r.wg.Done(); r.writer(l) }()
	go func() { defer r.wg.Done(); r.drive(runCtx, l) }()
	return l.snapshot(), nil
}

// traffic is the gateway client a run of p uses: the scope its gateway.url
// names when the plan uses the gateway, otherwise Traffic.
func (r *Runner) traffic(p *plan.Plan) Traffic {
	if r.cfg.Traffic == nil {
		return nil
	}
	if p.Gateway.Mode == "http" && r.cfg.TrafficAt != nil {
		return r.cfg.TrafficAt(p.Gateway.URL)
	}
	return r.cfg.Traffic
}

func (r *Runner) canaryOptions(p *plan.Plan, in *apiv1.CanaryOptions) (*apiv1.CanaryOptions, error) {
	if in == nil {
		return nil, nil
	}
	if r.cfg.Traffic == nil || p.Gateway.Mode != "http" {
		return nil, fmt.Errorf("%w: canary needs the platform gateway (gateway.mode http)", ErrInvalid)
	}
	if p.Gateway.SourceBackend == p.Gateway.TargetBackend {
		return nil, fmt.Errorf("%w: canary needs different source and target backends", ErrInvalid)
	}
	out := *in
	if len(out.Weights) == 0 {
		return nil, fmt.Errorf("%w: canary.weights must not be empty", ErrInvalid)
	}
	for i, w := range out.Weights {
		if w < 1 || w > 100 || (i > 0 && w <= out.Weights[i-1]) {
			return nil, fmt.Errorf("%w: canary.weights must increase within 1..100", ErrInvalid)
		}
	}
	step := 20 * time.Second
	if out.StepDuration != nil {
		d, err := time.ParseDuration(*out.StepDuration)
		if err != nil || d < r.cfg.MinCanaryStep || d > 5*time.Minute {
			return nil, fmt.Errorf("%w: canary.stepDuration must be a duration between %s and 5m", ErrInvalid, r.cfg.MinCanaryStep)
		}
		step = d
	}
	s := step.String()
	out.StepDuration = &s
	g, err := gateWithDefaults(out.Gate)
	if err != nil {
		return nil, fmt.Errorf("%w: canary.gate %v", ErrInvalid, err)
	}
	out.Gate = &g
	return &out, nil
}

// --- driving ---

func (r *Runner) drive(ctx context.Context, l *live) {
	defer func() {
		r.finish(l)
		r.maybeAutoRollback(l)
	}()

	// The engine's own state decides: after a restart that came between
	// ResumeHalted and the next save of the run, the engine is no longer halted.
	if l.resumeHalted && l.eng.State().Outcome == engine.OutcomeHalted {
		if err := l.eng.ResumeHalted(ctx); err != nil {
			// The engine keeps the run halted; so does the run.
			r.ended(l, fmt.Errorf("resume after halt refused: %w", err))
			return
		}
		l.syncEngine()
	}

	if l.traffic != nil {
		r.captureTrafficBase(l)
		stop := make(chan struct{})
		defer close(stop)
		go r.refreshTraffic(l, stop)
	}

	r.setStatus(l, apiv1.RunStatusRunning, apiv1.RunPhaseSync, "")
	if err := r.runSteps(ctx, l, syncSteps); err != nil {
		r.ended(l, err)
		return
	}

	if l.needsCanary() {
		ok, reason := r.canary(ctx, l)
		if !ok {
			if ctx.Err() != nil || l.interrupted() {
				r.ended(l, errors.New(reason))
				return
			}
			r.setStatus(l, apiv1.RunStatusRolledBack, apiv1.RunPhaseCanary, "canary gate failed: "+reason+"; all reads are back on the source")
			return
		}
	}

	if l.needsSwitchApproval() {
		r.setStatus(l, apiv1.RunStatusAwaitingSwitch, apiv1.RunPhaseAwaitingSwitch, "data is in sync; waiting for an operator to approve the cutover")
		select {
		case <-l.switchCh:
		case <-ctx.Done():
			r.ended(l, ctx.Err())
			return
		case <-r.stopCh:
			l.markInterrupted()
			r.ended(l, errors.New("the platform is shutting down"))
			return
		}
	}

	r.setStatus(l, apiv1.RunStatusRunning, apiv1.RunPhaseCutover, "")
	if err := r.runSteps(ctx, l, engine.Steps); err != nil {
		r.ended(l, err)
		return
	}
	l.emitPlatform(engine.EventDone, "", "migration succeeded", map[string]any{"downtimeMillis": l.eng.State().DowntimeMillis})
	if l.needsObserve() {
		reason := r.observe(ctx, l)
		r.ended(l, nil)
		if reason != "" {
			r.setStatus(l, apiv1.RunStatusSucceeded, apiv1.RunPhaseObserve, reason)
		}
		return
	}
	r.ended(l, nil)
}

// runSteps runs every step in steps that has not succeeded yet, one RunStep
// at a time, and copies the engine state into the run document after each.
// This goroutine is the only one that touches the engine.
func (r *Runner) runSteps(ctx context.Context, l *live, steps []string) error {
	for _, step := range steps {
		if stepSucceeded(l.eng, step) {
			continue
		}
		if step == engine.StepVerify {
			r.writeWritten(ctx, l)
		}
		err := l.eng.RunStep(ctx, step)
		l.syncEngine()
		if err != nil {
			return err
		}
	}
	return nil
}

// syncEngine copies the engine state into the run document and asks the
// writer to save it. Call it only from the drive goroutine.
func (l *live) syncEngine() {
	st := engineState(l.eng.State())
	l.mu.Lock()
	l.doc.Engine = st
	l.mu.Unlock()
	l.q.push(item{doc: true})
}

func (l *live) needsCanary() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.doc.Canary != nil && (l.doc.Canary.Passed == nil || !*l.doc.Canary.Passed)
}

func (l *live) needsSwitchApproval() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return !l.doc.AutoSwitch && l.doc.SwitchApprovedAt == nil
}

func (l *live) markInterrupted() {
	l.mu.Lock()
	l.interrupt = true
	l.mu.Unlock()
}

func (l *live) aborted() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.abortReq
}

func (l *live) interrupted() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.interrupt
}

// ended maps the engine outcome (and an operator abort or a shutdown) to the
// run status. Call it only from the drive goroutine.
func (r *Runner) ended(l *live, err error) {
	st := l.eng.State()
	l.mu.Lock()
	aborted, interrupted := l.abortReq, l.interrupt
	l.mu.Unlock()

	reason := ""
	if err != nil {
		reason = err.Error()
	}
	var status apiv1.RunStatus
	switch st.Outcome {
	case engine.OutcomeSucceeded:
		status, reason = apiv1.RunStatusSucceeded, ""
	case engine.OutcomeRolledBack:
		status = apiv1.RunStatusRolledBack
	case engine.OutcomeHalted:
		status = apiv1.RunStatusHalted
	default: // failed, or still running: nothing irreversible happened
		switch {
		case aborted:
			status = apiv1.RunStatusAborted
		case interrupted || r.ctx.Err() != nil:
			status, reason = apiv1.RunStatusInterrupted, "the platform shut down during the run; resume it to continue"
		default:
			status = apiv1.RunStatusFailed
		}
	}
	l.mu.Lock()
	phase := l.doc.Phase
	l.mu.Unlock()
	if p, ok := stepPhase[st.FailureStep]; ok && status != apiv1.RunStatusSucceeded {
		phase = p // the writer may not have applied the failed step's events yet
	}
	r.setStatus(l, status, phase, reason)
}

var stepPhase = map[string]apiv1.RunPhase{
	engine.StepPrecheck: apiv1.RunPhaseSync, engine.StepPrepare: apiv1.RunPhaseSync,
	engine.StepPresync: apiv1.RunPhaseSync, engine.StepDelta: apiv1.RunPhaseSync,
	engine.StepFence: apiv1.RunPhaseCutover, engine.StepSwitchover: apiv1.RunPhaseCutover,
	engine.StepStartTarget: apiv1.RunPhaseCutover, engine.StepCutover: apiv1.RunPhaseCutover, engine.StepVerify: apiv1.RunPhaseVerify,
	engine.StepFinalize: apiv1.RunPhaseVerify,
}

// finish records the final numbers, puts canary reads back if traffic did not
// move, flushes events, closes subscribers and forgets the run.
func (r *Runner) finish(l *live) {
	if l.traffic != nil {
		r.updateTraffic(l)
	}
	st := engineState(l.eng.State())
	now := r.now()
	l.mu.Lock()
	l.doc.Engine = st
	if l.doc.Status == apiv1.RunStatusRunning || l.doc.Status == apiv1.RunStatusAwaitingSwitch {
		// The run context was cancelled by a shutdown while waiting.
		l.doc.Status = apiv1.RunStatusInterrupted
	}
	status := l.doc.Status
	restoreReads := l.readsMoved && !st.TrafficOnTarget && status != apiv1.RunStatusHalted && status != apiv1.RunStatusSucceeded
	l.mu.Unlock()
	if restoreReads {
		r.resetReads(l)
	}

	l.mu.Lock()
	fillResult(&l.doc.Result, st, l.doc.StartedAt, now)
	l.doc.Phase = apiv1.RunPhaseDone
	l.doc.EndedAt = &now
	reason := l.doc.StatusReason
	l.mu.Unlock()
	l.emitPlatform(EventStatusChanged, "", "run ended: "+string(status), map[string]any{"status": status, "phase": apiv1.RunPhaseDone, "reason": deref(reason)})

	l.q.close()
	<-l.q.drained
	l.eng.Close()
	l.cancel()

	r.mu.Lock()
	delete(r.live, l.id)
	r.mu.Unlock()
	l.mu.Lock()
	for ch := range l.subs {
		close(ch)
	}
	l.subs = nil
	l.mu.Unlock()
	close(l.done)
}

func (r *Runner) setStatus(l *live, status apiv1.RunStatus, phase apiv1.RunPhase, reason string) {
	l.mu.Lock()
	changed := l.doc.Status != status || l.doc.Phase != phase
	l.doc.Status, l.doc.Phase = status, phase
	l.doc.StatusReason = nil
	if reason != "" {
		l.doc.StatusReason = &reason
	}
	l.mu.Unlock()
	if changed {
		l.emitPlatform(EventStatusChanged, "", string(status)+" ("+string(phase)+")", map[string]any{"status": status, "phase": phase, "reason": reason})
	}
}

// --- canary ---

func (r *Runner) canary(ctx context.Context, l *live) (bool, string) {
	l.mu.Lock()
	opts := l.doc.Canary.Options
	l.doc.Canary.Steps = l.doc.Canary.Steps[:0]
	l.mu.Unlock()
	r.setStatus(l, apiv1.RunStatusRunning, apiv1.RunPhaseCanary, "")

	src, dst := l.plan.Gateway.SourceBackend, l.plan.Gateway.TargetBackend
	stepDur, _ := time.ParseDuration(*opts.StepDuration)
	for i, w := range opts.Weights {
		last := i == len(opts.Weights)-1
		weights := map[string]int{dst: w}
		if w < 100 {
			weights[src] = 100 - w
		}
		step := apiv1.CanaryStep{Weight: w, StartedAt: r.now()}
		l.mu.Lock()
		l.readsMoved = true // before the call: a timeout may still have applied it
		l.mu.Unlock()
		if err := r.withTimeout(func(c context.Context) error { return l.traffic.SetReadWeights(c, weights) }); err != nil {
			return r.canaryFail(l, step, "could not set read weights: "+err.Error())
		}
		l.emitPlatform(EventCanaryStep, "", fmt.Sprintf("%d%% of reads to %s", w, dst), map[string]any{"weight": w, "readWeights": weights})

		select {
		case <-ctx.Done():
			return false, "aborted during the canary"
		case <-r.stopCh:
			l.markInterrupted()
			return false, "the platform is shutting down"
		case <-time.After(stepDur):
		}

		var ts apiv1.TrafficState
		err := r.withTimeout(func(c context.Context) error {
			var err error
			ts, err = l.traffic.State(c, stepDur)
			return err
		})
		var gate apiv1.GateResult
		verdict := apiv1.CanaryStepVerdictFailed
		if err != nil {
			// No data is a stop, not a pass.
			gate = apiv1.GateResult{Passed: false, Reason: "gateway unreachable: " + err.Error()}
		} else {
			s := ts.Stats[dst]
			gate = evaluateGate(s, *opts.Gate)
			few := s.Requests < int64(*opts.Gate.MinRequests)
			switch {
			case gate.Passed:
				verdict = apiv1.CanaryStepVerdictOk
			case few && s.ErrorRate > *opts.Gate.MaxErrorRate:
				// Errors already over the limit fail even a small sample: going
				// back costs nothing before cutover. p95 of a few requests is
				// too noisy to fail on.
				gate.Reason = fmt.Sprintf("error rate %.2f%% over only %d requests is above %.2f%%",
					s.ErrorRate*100, s.Requests, *opts.Gate.MaxErrorRate*100)
			case few && !last:
				// Small first steps (1%) of a quiet app see few requests. Too few
				// to judge is not a failure; the next, larger step decides. The
				// last step must judge with enough requests.
				verdict = apiv1.CanaryStepVerdictInconclusive
				gate.Reason += "; continuing to the next step"
			case few:
				gate.Reason += " at the last step; the target was never judged with enough requests"
			}
		}
		end := r.now()
		step.EndedAt, step.Gate, step.Verdict = &end, &gate, &verdict
		l.mu.Lock()
		l.doc.Canary.Steps = append(l.doc.Canary.Steps, step)
		l.mu.Unlock()
		l.emitPlatform(EventGateResult, "", gateMessage(w, verdict, gate), map[string]any{"weight": w, "verdict": verdict, "gate": gate})
		if verdict == apiv1.CanaryStepVerdictFailed {
			l.setCanaryPassed(false)
			return false, gate.Reason
		}
	}
	l.setCanaryPassed(true)
	return true, ""
}

func (r *Runner) canaryFail(l *live, step apiv1.CanaryStep, reason string) (bool, string) {
	end := r.now()
	gate := apiv1.GateResult{Passed: false, Reason: reason}
	verdict := apiv1.CanaryStepVerdictFailed
	step.EndedAt, step.Gate, step.Verdict = &end, &gate, &verdict
	l.mu.Lock()
	l.doc.Canary.Steps = append(l.doc.Canary.Steps, step)
	l.mu.Unlock()
	l.setCanaryPassed(false)
	l.emitPlatform(EventGateResult, "", gateMessage(step.Weight, verdict, gate), map[string]any{"weight": step.Weight, "verdict": verdict, "gate": gate})
	return false, reason
}

func (l *live) setCanaryPassed(v bool) {
	l.mu.Lock()
	l.doc.Canary.Passed = &v
	l.mu.Unlock()
}

// evaluateGate decides one canary step from the target's requests over the step.
func evaluateGate(s apiv1.BackendStats, g apiv1.GateThresholds) apiv1.GateResult {
	res := apiv1.GateResult{Requests: s.Requests, ErrorRate: s.ErrorRate, P95Millis: s.P95Millis}
	switch {
	case s.Requests < int64(*g.MinRequests):
		res.Reason = fmt.Sprintf("only %d requests reached the target (need %d)", s.Requests, *g.MinRequests)
	case s.ErrorRate > *g.MaxErrorRate:
		res.Reason = fmt.Sprintf("error rate %.2f%% is above %.2f%%", s.ErrorRate*100, *g.MaxErrorRate*100)
	case s.P95Millis > float32(*g.MaxP95Millis):
		res.Reason = fmt.Sprintf("p95 %.0fms is above %dms", s.P95Millis, *g.MaxP95Millis)
	default:
		res.Passed = true
		res.Reason = fmt.Sprintf("%d requests, error rate %.2f%%, p95 %.0fms", s.Requests, s.ErrorRate*100, s.P95Millis)
	}
	return res
}

func gateMessage(w int, v apiv1.CanaryStepVerdict, g apiv1.GateResult) string {
	word := map[apiv1.CanaryStepVerdict]string{
		apiv1.CanaryStepVerdictOk: "passed", apiv1.CanaryStepVerdictFailed: "failed", apiv1.CanaryStepVerdictInconclusive: "inconclusive",
	}[v]
	return fmt.Sprintf("gate at %d%% %s: %s", w, word, g.Reason)
}

// resetReads sends every read back to the source after a canary that did not
// end in a cutover.
func (r *Runner) resetReads(l *live) {
	if l.traffic == nil {
		return
	}
	src := l.plan.Gateway.SourceBackend
	err := r.withTimeout(func(c context.Context) error {
		return l.traffic.SetReadWeights(c, map[string]int{src: 100})
	})
	if err != nil {
		l.emitPlatform(engine.EventLog, "", "WARNING: could not move reads back to the source: "+err.Error(), nil)
		return
	}
	l.emitPlatform(EventCanaryStep, "", "all reads back on "+src, map[string]any{"weight": 0, "readWeights": map[string]int{src: 100}})
}

func (r *Runner) withTimeout(f func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return f(ctx)
}

// --- traffic numbers ---

func (r *Runner) captureTrafficBase(l *live) {
	var ts apiv1.TrafficState
	err := r.withTimeout(func(c context.Context) error {
		var err error
		ts, err = l.traffic.State(c, 10*time.Second)
		return err
	})
	if err != nil {
		l.emitPlatform(engine.EventLog, "", "WARNING: gateway unreachable; request counts for this run are not measured: "+err.Error(), nil)
		return
	}
	l.mu.Lock()
	l.trafficBase = ts.Totals
	l.mu.Unlock()
}

func (r *Runner) refreshTraffic(l *live, stop <-chan struct{}) {
	t := time.NewTicker(r.cfg.PollInterval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			if r.updateTraffic(l) {
				l.q.push(item{doc: true})
			}
		}
	}
}

// updateTraffic sets requests and failed requests since the run started.
func (r *Runner) updateTraffic(l *live) bool {
	l.mu.Lock()
	base := l.trafficBase
	l.mu.Unlock()
	if base == nil {
		return false
	}
	var ts apiv1.TrafficState
	if err := r.withTimeout(func(c context.Context) error {
		var err error
		ts, err = l.traffic.State(c, 10*time.Second)
		return err
	}); err != nil {
		return false
	}
	l.mu.Lock()
	req, fail := l.prevReq, l.prevFail
	l.mu.Unlock()
	for name, t := range ts.Totals {
		b := base[name]
		req += t.Requests - b.Requests
		fail += t.Errors - b.Errors
	}
	l.mu.Lock()
	l.doc.Result.Requests, l.doc.Result.FailedRequests = &req, &fail
	l.mu.Unlock()
	return true
}

// --- operator actions ---

// Switch approves the cutover of a run waiting in awaiting_switch.
func (r *Runner) Switch(ctx context.Context, id, actor string) (apiv1.Run, error) {
	l, err := r.liveOrStatus(ctx, id)
	if err != nil {
		return apiv1.Run{}, err
	}
	l.mu.Lock()
	if l.switched {
		l.mu.Unlock()
		return l.snapshot(), nil
	}
	if l.doc.Status != apiv1.RunStatusAwaitingSwitch {
		st := l.doc.Status
		l.mu.Unlock()
		return apiv1.Run{}, fmt.Errorf("%w: run is %s, not awaiting_switch", ErrWrongStatus, st)
	}
	now := r.now()
	l.doc.SwitchApprovedBy, l.doc.SwitchApprovedAt = &actor, &now
	l.switched = true
	l.mu.Unlock()
	// Queue the approval before the drive goroutine can emit cutover events.
	l.emitPlatform(engine.EventLog, "", "cutover approved by "+actor, map[string]any{"actor": actor})
	close(l.switchCh)
	return l.snapshot(), nil
}

// Abort cancels an active run. The engine's boundary decides the outcome.
func (r *Runner) Abort(ctx context.Context, id, actor, reason string) (apiv1.Run, error) {
	l, err := r.liveOrStatus(ctx, id)
	if err != nil {
		return apiv1.Run{}, err
	}
	l.mu.Lock()
	if !l.abortReq {
		l.abortReq = true
		l.doc.AbortedBy = &actor
		if reason != "" {
			l.doc.AbortReason = &reason
		}
	}
	l.mu.Unlock()
	l.emitPlatform(engine.EventLog, "", "abort requested by "+actor, map[string]any{"actor": actor, "reason": reason})
	l.cancel()
	return l.snapshot(), nil
}

// Resume continues an interrupted, failed or aborted run with a fresh engine
// that loads the run's state file. Steps that already succeeded are skipped.
// A halted run resumes only after the engine confirms the target's promotion
// went through and it is the only primary (engine.ResumeHalted); otherwise it
// ends halted again with the engine's reason.
func (r *Runner) Resume(ctx context.Context, id, actor string, p *plan.Plan) (apiv1.Run, error) {
	r.mu.Lock()
	_, active := r.live[id]
	r.mu.Unlock()
	if active {
		return apiv1.Run{}, fmt.Errorf("%w: run is active", ErrWrongStatus)
	}
	doc, err := r.stored(ctx, id)
	if err != nil {
		return apiv1.Run{}, err
	}
	// A halted run whose resume was cut short by a restart is interrupted
	// (Recover) while its engine is still halted: it resumes the same way.
	halted := doc.Status == apiv1.RunStatusHalted || doc.Engine.Outcome == apiv1.EngineOutcomeHalted
	resumable := []apiv1.RunStatus{apiv1.RunStatusInterrupted, apiv1.RunStatusFailed, apiv1.RunStatusAborted}
	if !halted && !slices.Contains(resumable, doc.Status) {
		return apiv1.Run{}, fmt.Errorf("%w: run is %s; only interrupted, failed, aborted or halted runs resume", ErrWrongStatus, doc.Status)
	}
	if o := doc.Engine.Outcome; halted && o != apiv1.EngineOutcomeHalted {
		return apiv1.Run{}, fmt.Errorf("%w: engine outcome is %s", ErrWrongStatus, o)
	} else if !halted && o != apiv1.EngineOutcomeRunning && o != apiv1.EngineOutcomeFailed {
		return apiv1.Run{}, fmt.Errorf("%w: engine outcome is %s", ErrWrongStatus, o)
	}
	reason := "resumed by " + actor
	if halted {
		reason = "resumed after halt by " + actor
	}
	doc.Status, doc.StatusReason = apiv1.RunStatusRunning, &reason
	doc.EndedAt, doc.AbortedBy, doc.AbortReason = nil, nil, nil
	doc.UpdatedAt = r.now()
	return r.launch(ctx, p, id, doc, halted)
}

// InterruptWaiting makes every run stop waiting, now and later: runs in
// awaiting_switch or in a canary step end as interrupted (nothing is in
// flight there) and can be resumed after the restart; runs in observe stop
// observing and end as succeeded, since the data already moved. Runs executing an
// engine step are not touched; Wait lets them finish. Call it when the server
// shuts down. It returns how many runs were waiting at the time of the call.
func (r *Runner) InterruptWaiting() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	// Count before closing stopCh: a run that sees it closed ends at once,
	// its phase becomes done and finish removes it from r.live, so counting
	// afterwards could miss it.
	n := 0
	for _, l := range r.live {
		l.mu.Lock()
		if l.doc.Phase == apiv1.RunPhaseAwaitingSwitch || l.doc.Phase == apiv1.RunPhaseCanary || l.doc.Phase == apiv1.RunPhaseObserve {
			n++
		}
		l.mu.Unlock()
	}
	r.stopOnce.Do(func() { close(r.stopCh) })
	return n
}

// Recover marks runs that were active when the server stopped as interrupted.
// Call once at startup, before serving requests.
func (r *Runner) Recover(ctx context.Context) (int, error) {
	rows, err := r.cfg.Store.ListRunsByStatus(ctx, string(apiv1.RunStatusRunning), string(apiv1.RunStatusAwaitingSwitch))
	if err != nil {
		return 0, err
	}
	n := 0
	for _, row := range rows {
		r.mu.Lock()
		_, active := r.live[row.RunID]
		r.mu.Unlock()
		if active {
			continue
		}
		var doc apiv1.Run
		if err := json.Unmarshal(row.Doc, &doc); err != nil {
			return n, fmt.Errorf("decode run %s: %w", row.RunID, err)
		}
		reason := "the platform restarted while this run was active; resume it to continue"
		doc.Status = apiv1.RunStatusInterrupted
		if doc.Engine.Outcome == apiv1.EngineOutcomeSucceeded {
			// Stopped while observing: the data already moved, only the watch was cut short.
			reason = "the cutover succeeded but the observation is incomplete: the platform restarted"
			doc.Status = apiv1.RunStatusSucceeded
			if doc.Observe != nil && doc.Observe.Passed == nil {
				stopped := "the platform restarted"
				doc.Observe.StoppedReason = &stopped
			}
		}
		doc.StatusReason, doc.UpdatedAt = &reason, r.now()
		nr, err := runRow(doc)
		if err != nil {
			return n, err
		}
		if err := r.cfg.Store.UpdateRun(ctx, nr); err != nil {
			return n, err
		}
		data, _ := json.Marshal(map[string]any{"status": doc.Status, "phase": doc.Phase, "reason": reason})
		_, _ = r.cfg.Store.AppendEvent(ctx, store.Event{RunID: row.RunID, Time: r.now(), Kind: EventStatusChanged, Message: string(doc.Status), Data: data})
		n++
	}
	return n, nil
}

func (r *Runner) liveOrStatus(ctx context.Context, id string) (*live, error) {
	r.mu.Lock()
	l, ok := r.live[id]
	r.mu.Unlock()
	if ok {
		return l, nil
	}
	doc, err := r.stored(ctx, id)
	if err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("%w: run is %s", ErrWrongStatus, doc.Status)
}

// --- reading ---

// Get returns a run, live if active.
func (r *Runner) Get(ctx context.Context, id string) (apiv1.Run, error) {
	r.mu.Lock()
	l, ok := r.live[id]
	r.mu.Unlock()
	if ok {
		return l.snapshot(), nil
	}
	return r.stored(ctx, id)
}

// List returns all runs, newest first.
func (r *Runner) List(ctx context.Context) ([]apiv1.Run, error) {
	rows, err := r.cfg.Store.ListRuns(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]apiv1.Run, 0, len(rows))
	for _, row := range rows {
		r.mu.Lock()
		l, ok := r.live[row.RunID]
		r.mu.Unlock()
		if ok {
			out = append(out, l.snapshot())
			continue
		}
		var doc apiv1.Run
		if err := json.Unmarshal(row.Doc, &doc); err != nil {
			return nil, fmt.Errorf("decode run %s: %w", row.RunID, err)
		}
		out = append(out, doc)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].StartedAt.After(out[j].StartedAt) })
	return out, nil
}

// Events returns stored events with seq > after.
func (r *Runner) Events(ctx context.Context, id string, after int64) ([]apiv1.RunEvent, error) {
	rows, err := r.cfg.Store.ListEvents(ctx, id, after, 0)
	if err != nil {
		return nil, err
	}
	out := make([]apiv1.RunEvent, len(rows))
	for i, e := range rows {
		out[i] = apiEvent(e)
	}
	return out, nil
}

// Subscribe returns live events of an active run. The channel closes when the
// run ends (after its last event) or when the subscriber falls behind. For a
// run that is not active it returns a closed channel. Subscribe before reading
// stored events so nothing falls between the two; drop duplicates by seq.
func (r *Runner) Subscribe(ctx context.Context, id string) (<-chan apiv1.RunEvent, func(), error) {
	r.mu.Lock()
	l, ok := r.live[id]
	r.mu.Unlock()
	ch := make(chan apiv1.RunEvent, SubscriberBuffer)
	if !ok {
		if _, err := r.stored(ctx, id); err != nil {
			return nil, nil, err
		}
		close(ch)
		return ch, func() {}, nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.subs == nil { // finished in between
		close(ch)
		return ch, func() {}, nil
	}
	l.subs[ch] = struct{}{}
	cancel := func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		if _, ok := l.subs[ch]; ok {
			delete(l.subs, ch)
			close(ch)
		}
	}
	return ch, cancel, nil
}

func (r *Runner) stored(ctx context.Context, id string) (apiv1.Run, error) {
	row, err := r.cfg.Store.GetRun(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return apiv1.Run{}, ErrNotFound
	}
	if err != nil {
		return apiv1.Run{}, err
	}
	var doc apiv1.Run
	if err := json.Unmarshal(row.Doc, &doc); err != nil {
		return apiv1.Run{}, fmt.Errorf("decode run %s: %w", id, err)
	}
	return doc, nil
}

// Wait blocks until every run goroutine has finished or ctx ends.
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

// Active reports how many runs are active.
func (r *Runner) Active() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.live)
}

// snapshot returns a deep copy: the document holds slices and pointers that
// the run goroutines keep changing while callers encode the copy.
func (l *live) snapshot() apiv1.Run {
	l.mu.Lock()
	b, err := json.Marshal(l.doc)
	l.mu.Unlock()
	var doc apiv1.Run
	if err == nil {
		err = json.Unmarshal(b, &doc)
	}
	if err != nil {
		panic("runs: run document does not round-trip through JSON: " + err.Error())
	}
	return doc
}

// --- events: engine goroutine -> queue -> writer -> store + subscribers ---

type item struct {
	ev  store.Event
	doc bool // only persist the run document
}

// queue is an unbounded FIFO. The engine calls Emit synchronously, sometimes
// while holding its own lock, so emitting must never block or call back into
// the engine.
type queue struct {
	mu      sync.Mutex
	items   []item
	closed  bool
	signal  chan struct{}
	drained chan struct{}
}

func (q *queue) init() {
	q.signal = make(chan struct{}, 1)
	q.drained = make(chan struct{})
}

func (q *queue) push(it item) {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return
	}
	q.items = append(q.items, it)
	q.mu.Unlock()
	select {
	case q.signal <- struct{}{}:
	default:
	}
}

func (q *queue) close() {
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
	select {
	case q.signal <- struct{}{}:
	default:
	}
}

func (q *queue) take() ([]item, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	its := q.items
	q.items = nil
	return its, q.closed
}

func (l *live) emitEngine(ev engine.Event) {
	var data []byte
	if len(ev.Data) > 0 {
		data, _ = json.Marshal(ev.Data)
	}
	t := ev.Time
	if t.IsZero() {
		t = time.Now().UTC()
	}
	l.q.push(item{ev: store.Event{RunID: ev.RunID, Time: t, Kind: ev.Kind, Step: ev.Step, Message: ev.Message, Data: data}})
}

func (l *live) emitPlatform(kind, step, msg string, data map[string]any) {
	var b []byte
	if data != nil {
		b, _ = json.Marshal(data)
	}
	l.q.push(item{ev: store.Event{RunID: l.id, Time: time.Now().UTC(), Kind: kind, Step: step, Message: msg, Data: b}})
}

// writer persists events in order, applies them to the run document, fans
// them out, and saves the document after each batch.
func (r *Runner) writer(l *live) {
	defer close(l.q.drained)
	for {
		its, closed := l.q.take()
		for _, it := range its {
			if it.doc {
				continue
			}
			ev, err := r.cfg.Store.AppendEvent(context.Background(), it.ev)
			if err != nil {
				r.cfg.Log.Error("store run event", "run", l.id, "err", err)
				ev = it.ev // still deliver live; it just cannot be replayed
			}
			r.apply(l, apiEvent(ev))
		}
		if len(its) > 0 || closed {
			r.persist(l)
		}
		if closed { // take returns the last items together with closed
			return
		}
		<-l.q.signal
	}
}

func (r *Runner) apply(l *live, ev apiv1.RunEvent) {
	l.mu.Lock()
	defer l.mu.Unlock()
	// Events only move the phase forward: the writer can apply an engine
	// event after drive or finish already set a later phase.
	if ev.Kind == engine.EventStepStarted && ev.Step != nil {
		next := l.doc.Phase
		switch *ev.Step {
		case engine.StepFence, engine.StepSwitchover, engine.StepStartTarget, engine.StepCutover:
			next = apiv1.RunPhaseCutover
		case engine.StepVerify, engine.StepFinalize:
			next = apiv1.RunPhaseVerify
		}
		if phaseRank[next] > phaseRank[l.doc.Phase] {
			l.doc.Phase = next
		}
	}
	if ev.Step != nil {
		// Live step status between the drive goroutine's engine snapshots.
		if rec, ok := l.doc.Engine.Steps[*ev.Step]; ok {
			t := ev.Time
			switch ev.Kind {
			case engine.EventStepStarted:
				rec.Status, rec.StartedAt, rec.EndedAt, rec.Error = apiv1.StepStatusRunning, &t, nil, nil
			case engine.EventStepSucceeded:
				rec.Status, rec.EndedAt = apiv1.StepStatusSucceeded, &t
			case engine.EventStepFailed:
				rec.Status, rec.EndedAt, rec.Error = apiv1.StepStatusFailed, &t, ev.Message
			}
			l.doc.Engine.Steps[*ev.Step] = rec
		}
	}
	if ev.Kind == engine.EventMetric && ev.Data != nil {
		if v, ok := (*ev.Data)["downtimeMillis"].(float64); ok {
			d := int64(v)
			l.doc.Result.DowntimeMillis = &d
		}
	}
	for ch := range l.subs {
		select {
		case ch <- ev:
		default: // too slow: drop; the client reconnects with Last-Event-ID
			delete(l.subs, ch)
			close(ch)
		}
	}
}

// persist saves the run document. It never touches the engine (see Engine).
func (r *Runner) persist(l *live) {
	l.mu.Lock()
	l.doc.UpdatedAt = r.now()
	row, err := runRow(l.doc) // marshal under the lock; the document is shared
	l.mu.Unlock()
	if err == nil {
		err = r.cfg.Store.UpdateRun(context.Background(), row)
	}
	if err != nil {
		r.cfg.Log.Error("store run", "run", l.id, "err", err)
	}
}

// --- conversions ---

func runRow(doc apiv1.Run) (store.Run, error) {
	b, err := json.Marshal(doc)
	if err != nil {
		return store.Run{}, err
	}
	return store.Run{
		RunID:     doc.RunId,
		PlanRef:   doc.PlanRef,
		SourceEnv: doc.Source.Env,
		TargetEnv: doc.Target.Env,
		Status:    string(doc.Status),
		Doc:       b,
		CreatedAt: doc.StartedAt,
		UpdatedAt: doc.UpdatedAt,
	}, nil
}

func apiEvent(e store.Event) apiv1.RunEvent {
	ev := apiv1.RunEvent{Seq: e.Seq, Time: e.Time, RunId: e.RunID, Kind: e.Kind}
	if e.Step != "" {
		ev.Step = &e.Step
	}
	if e.Message != "" {
		ev.Message = &e.Message
	}
	if len(e.Data) > 0 {
		var m map[string]any
		if json.Unmarshal(e.Data, &m) == nil {
			ev.Data = &m
		}
	}
	return ev
}

// engineState converts through JSON: apiv1.EngineState mirrors the engine's
// JSON field names, and evidence holds engine structs.
func engineState(s engine.State) apiv1.EngineState {
	var out apiv1.EngineState
	b, err := json.Marshal(s)
	if err == nil {
		err = json.Unmarshal(b, &out)
	}
	if err != nil {
		out = apiv1.EngineState{RunId: s.RunID, PlanId: s.PlanID, Outcome: apiv1.EngineOutcome(s.Outcome)}
	}
	if out.Steps == nil {
		out.Steps = map[string]apiv1.StepRecord{}
	}
	return out
}

// verifyReport is the part of the engine's verify evidence the result needs.
type verifyReport struct {
	OK          bool  `json:"ok"`
	MissingRows int64 `json:"missingRows"`
	Written     *struct {
		Missing int64 `json:"missing"`
	} `json:"written"`
	Files *struct {
		Missing int64 `json:"missingOnTarget"`
	} `json:"files"`
}

func fillResult(res *apiv1.RunResult, st apiv1.EngineState, started, ended time.Time) {
	d := ended.Sub(started).Milliseconds()
	res.DurationMillis = &d
	if st.DowntimeMillis != nil && *st.DowntimeMillis > 0 {
		res.DowntimeMillis = st.DowntimeMillis
	}
	rec, ok := st.Steps[engine.StepVerify]
	if !ok || rec.Evidence == nil {
		return
	}
	raw, ok := (*rec.Evidence)["report"]
	if !ok {
		return
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return
	}
	var rep verifyReport
	if json.Unmarshal(b, &rep) != nil {
		return
	}
	res.Verified = &rep.OK
	res.MissingRows = &rep.MissingRows
	if rep.Written != nil {
		res.MissingWrites = &rep.Written.Missing
	}
	if rep.Files != nil {
		res.MissingFiles = &rep.Files.Missing
	}
}

func stepSucceeded(e Engine, step string) bool {
	rec := e.State().Steps[step]
	return rec != nil && rec.Status == engine.StatusSucceeded
}

func clusterKeys(p *plan.Plan) []string {
	k := func(e plan.Endpoint) string { return e.Context + "/" + e.Namespace + "/" + e.Cluster }
	return []string{k(p.Source), k(p.Target)}
}

func overlaps(a, b []string) bool {
	for _, x := range a {
		if slices.Contains(b, x) {
			return true
		}
	}
	return false
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
