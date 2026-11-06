package runs

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

// Platform event kinds of the observe phase.
const (
	EventObserveStep     = "observe_step"
	EventObserveRollback = "observe_rollback"
)

// AutoRollbackActor is the actor recorded when the observe phase starts a rollback.
const AutoRollbackActor = "slo-guard"

// AutoRollbackFunc starts the rollback of a succeeded run and returns the new
// run's id. The API server provides it (the same path as the rollback endpoint).
type AutoRollbackFunc func(ctx context.Context, runID, reason string) (string, error)

// SetAutoRollback installs the function the observe phase calls on an SLO
// violation when the run asked for autoRollback. Call it before serving.
func (r *Runner) SetAutoRollback(f AutoRollbackFunc) {
	r.mu.Lock()
	r.autoRollback = f
	r.mu.Unlock()
}

func (r *Runner) observeOptions(p *plan.Plan, in *apiv1.ObserveOptions) (*apiv1.ObserveOptions, error) {
	if in == nil {
		return nil, nil
	}
	if r.cfg.Traffic == nil || p.Gateway.Mode != "http" {
		return nil, fmt.Errorf("%w: observe needs the platform gateway (gateway.mode http)", ErrInvalid)
	}
	out := *in
	total := 2 * time.Minute
	if out.Duration != nil {
		d, err := time.ParseDuration(*out.Duration)
		if err != nil || d < r.minObserve() || d > 30*time.Minute {
			return nil, fmt.Errorf("%w: observe.duration must be a duration between %s and 30m", ErrInvalid, r.minObserve())
		}
		total = d
	}
	step := 20 * time.Second
	if out.StepDuration != nil {
		d, err := time.ParseDuration(*out.StepDuration)
		if err != nil || d < r.cfg.MinCanaryStep || d > 5*time.Minute {
			return nil, fmt.Errorf("%w: observe.stepDuration must be a duration between %s and 5m", ErrInvalid, r.cfg.MinCanaryStep)
		}
		step = d
	}
	if step > total {
		return nil, fmt.Errorf("%w: observe.stepDuration must not exceed observe.duration", ErrInvalid)
	}
	ts, ss := total.String(), step.String()
	out.Duration, out.StepDuration = &ts, &ss
	g, err := gateWithDefaults(out.Gate)
	if err != nil {
		return nil, fmt.Errorf("%w: observe.gate %v", ErrInvalid, err)
	}
	out.Gate = &g
	if out.AutoRollback == nil {
		f := false
		out.AutoRollback = &f
	}
	return &out, nil
}

// minObserve is the shortest observation: 10s, or less when tests shorten canary steps.
func (r *Runner) minObserve() time.Duration {
	if r.cfg.MinCanaryStep < 5*time.Second {
		return r.cfg.MinCanaryStep
	}
	return 10 * time.Second
}

// gateWithDefaults fills the default thresholds and checks their ranges.
func gateWithDefaults(in *apiv1.GateThresholds) (apiv1.GateThresholds, error) {
	g := apiv1.GateThresholds{}
	if in != nil {
		g = *in
	}
	if g.MaxErrorRate == nil {
		v := float32(0.01)
		g.MaxErrorRate = &v
	}
	if g.MaxP95Millis == nil {
		v := 800
		g.MaxP95Millis = &v
	}
	if g.MinRequests == nil {
		v := 20
		g.MinRequests = &v
	}
	if *g.MaxErrorRate < 0 || *g.MaxErrorRate > 1 || *g.MaxP95Millis <= 0 || *g.MinRequests < 1 {
		return g, fmt.Errorf("thresholds out of range")
	}
	return g, nil
}

func (l *live) needsObserve() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.doc.Observe != nil
}

// observe watches the target after a succeeded cutover. It returns the status
// reason for the run ("" when the observation passed).
func (r *Runner) observe(ctx context.Context, l *live) string {
	l.mu.Lock()
	o := l.doc.Observe
	opts := o.Options
	o.Steps, o.Passed, o.Violation, o.StoppedReason = o.Steps[:0], nil, nil, nil
	l.mu.Unlock()
	r.setStatus(l, apiv1.RunStatusRunning, apiv1.RunPhaseObserve, "")

	total, _ := time.ParseDuration(*opts.Duration)
	stepDur, _ := time.ParseDuration(*opts.StepDuration)
	dst := l.plan.Gateway.TargetBackend
	deadline := r.now().Add(total)
	conclusive := false
	for {
		window := min(stepDur, deadline.Sub(r.now()))
		if window <= 0 {
			break
		}
		step := apiv1.ObserveStep{StartedAt: r.now()}
		select {
		case <-ctx.Done():
			reason := "the platform is shutting down"
			if l.aborted() {
				reason = "stopped by an operator"
			}
			return r.observeStopped(l, reason)
		case <-r.stopCh:
			return r.observeStopped(l, "the platform is shutting down")
		case <-time.After(window):
		}

		var ts apiv1.TrafficState
		err := r.withTimeout(func(c context.Context) error {
			var err error
			ts, err = l.traffic.State(c, window)
			return err
		})
		var gate apiv1.GateResult
		switch {
		case err != nil:
			// No data is not a breach: a monitoring outage must not move the app back.
			gate = apiv1.GateResult{Reason: "gateway unreachable: " + err.Error()}
			step.Verdict = apiv1.ObserveStepVerdictInconclusive
		default:
			s := ts.Stats[dst]
			gate = evaluateGate(s, *opts.Gate)
			switch {
			case gate.Passed:
				step.Verdict = apiv1.ObserveStepVerdictOk
				conclusive = true
			case s.Requests < int64(*opts.Gate.MinRequests):
				step.Verdict = apiv1.ObserveStepVerdictInconclusive
			default:
				step.Verdict = apiv1.ObserveStepVerdictViolated
			}
		}
		end := r.now()
		step.EndedAt, step.Gate = &end, &gate
		l.mu.Lock()
		l.doc.Observe.Steps = append(l.doc.Observe.Steps, step)
		l.mu.Unlock()
		l.emitPlatform(EventObserveStep, "", fmt.Sprintf("observe %s: %s", step.Verdict, gate.Reason),
			map[string]any{"verdict": step.Verdict, "gate": gate})

		if step.Verdict == apiv1.ObserveStepVerdictViolated {
			f, v := false, gate.Reason
			l.mu.Lock()
			l.doc.Observe.Passed, l.doc.Observe.Violation = &f, &v
			l.mu.Unlock()
			if *opts.AutoRollback {
				return "SLO violated after cutover: " + v + "; starting the automatic rollback"
			}
			return "SLO violated after cutover: " + v + "; roll back with POST /v1/migrations/" + l.id + "/rollback"
		}
	}
	if !conclusive {
		return r.observeStopped(l, "no observe step had enough requests to judge the target")
	}
	t := true
	l.mu.Lock()
	l.doc.Observe.Passed = &t
	l.mu.Unlock()
	return ""
}

func (r *Runner) observeStopped(l *live, reason string) string {
	l.mu.Lock()
	l.doc.Observe.StoppedReason = &reason
	l.mu.Unlock()
	return "the cutover succeeded but the observation is incomplete: " + reason
}

// maybeAutoRollback starts the rollback after the run ended with an SLO
// violation and autoRollback. It runs after finish, when the run is stored as
// succeeded and no longer active, so the rollback can reserve its clusters.
func (r *Runner) maybeAutoRollback(l *live) {
	l.mu.Lock()
	o := l.doc.Observe
	want := o != nil && o.Violation != nil && o.Options.AutoRollback != nil && *o.Options.AutoRollback &&
		l.doc.Status == apiv1.RunStatusSucceeded
	reason := ""
	if want {
		reason = "SLO violated after cutover: " + *o.Violation
	}
	l.mu.Unlock()
	r.mu.Lock()
	f := r.autoRollback
	r.mu.Unlock()
	if !want {
		return
	}

	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.ctx), 30*time.Second)
	defer cancel()
	var rollbackID string
	var err error
	if f == nil {
		err = fmt.Errorf("automatic rollback is not available on this server")
	} else {
		rollbackID, err = f(ctx, l.id, reason)
	}

	doc, gerr := r.stored(ctx, l.id)
	if gerr != nil {
		r.cfg.Log.Error("record automatic rollback", "run", l.id, "err", gerr)
		return
	}
	data := map[string]any{}
	msg := ""
	if err != nil {
		e := err.Error()
		doc.Observe.RollbackError = &e
		data["error"] = e
		msg = "automatic rollback could not start: " + e
	} else {
		doc.Observe.RollbackRunId = &rollbackID
		data["rollbackRunId"] = rollbackID
		msg = "automatic rollback started as run " + rollbackID
	}
	doc.UpdatedAt = r.now()
	if row, err := runRow(doc); err == nil {
		err = r.cfg.Store.UpdateRun(ctx, row)
		if err != nil {
			r.cfg.Log.Error("record automatic rollback", "run", l.id, "err", err)
		}
	}
	b, _ := json.Marshal(data)
	if _, err := r.cfg.Store.AppendEvent(ctx, store.Event{RunID: l.id, Time: r.now(), Kind: EventObserveRollback, Message: msg, Data: b}); err != nil {
		r.cfg.Log.Error("record automatic rollback event", "run", l.id, "err", err)
	}
}
