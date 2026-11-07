package job

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/onboard"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/plans"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/runs"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/secrets"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/appspec"
)

// Cutover reserves the onboarding's next plan and starts its run (ready).
// A plan a gate refuses fails the onboarding with failedState
// cutting_over; resume goes back to ready.
func (r *Runner) Cutover(ctx context.Context, id, actor string) (apiv1.Onboarding, error) {
	x, err := r.load(ctx, id)
	if err != nil {
		return apiv1.Onboarding{}, err
	}
	if x.doc.Deleting {
		return apiv1.Onboarding{}, ErrDeleting
	}
	if x.row.State != store.OnboardingReady {
		return apiv1.Onboarding{}, fmt.Errorf("%w: onboarding %s is %s, not ready", ErrState, id, x.row.State)
	}
	if r.cfg.Cutover == nil || r.cfg.Apps == nil {
		return apiv1.Onboarding{}, fmt.Errorf("%w: plans and runs", ErrNotConfigured)
	}
	// The state first: nothing deploys the app from here (CuttingOver).
	attempt := x.doc.Internal.Attempt + 1
	x, err = r.update(ctx, id, []string{store.OnboardingReady}, func(x *rec) (string, error) {
		x.doc.Internal.Attempt, x.doc.Internal.PlanRef, x.doc.Internal.PostRun = attempt, "", nil
		x.doc.RunId = nil
		return store.OnboardingCuttingOver, nil
	})
	if err != nil {
		return apiv1.Onboarding{}, err
	}
	run, ref, err := r.startRun(ctx, x, actor)
	if err != nil {
		_ = r.fail(ctx, id, store.OnboardingCuttingOver, fmt.Errorf("the cutover did not start: %w", err))
		o, _ := r.Get(ctx, id)
		return o, err
	}
	x, err = r.update(ctx, id, []string{store.OnboardingCuttingOver}, func(x *rec) (string, error) {
		x.doc.RunId, x.doc.Internal.PlanRef = &run.RunId, ref
		return "", nil
	})
	if err != nil {
		return apiv1.Onboarding{}, err
	}
	r.event(ctx, x, apiv1.OnboardingEventKindStep, "cutover run "+run.RunId+" started by "+actor, map[string]any{"runId": run.RunId, "planRef": ref})
	r.kick(id)
	return View(x.row)
}

// startRun writes, reserves and starts the plan of x's attempt.
func (r *Runner) startRun(ctx context.Context, x *rec, actor string) (apiv1.Run, string, error) {
	raw, scope, err := r.plan(ctx, x)
	if err != nil {
		return apiv1.Run{}, "", err
	}
	if err := gates(x); err != nil {
		return apiv1.Run{}, "", err
	}
	pw, err := r.open(x, secrets.OnboardingSuperuserPassword, x.row.SuperuserPasswordNonce, x.row.SuperuserPasswordCiphertext)
	if err != nil {
		return apiv1.Run{}, "", err
	}
	r.cutoverMu.Lock()
	defer r.cutoverMu.Unlock()
	// plan.Parse expands the DSN from the process environment.
	_ = os.Setenv(onboard.LegacyPasswordEnv, pw)
	res, ref, err := r.cfg.Cutover.ReserveOnboarding(ctx, raw, actor, scope)
	_ = os.Unsetenv(onboard.LegacyPasswordEnv)
	if err != nil {
		return apiv1.Run{}, ref, err
	}
	run, err := r.cfg.Cutover.Start(ctx, runs.StartOptions{PlanRef: ref, Plan: res.Plan, Source: res.Summary.Source, Target: res.Summary.Target,
		Actor: actor, AutoSwitch: true})
	if err != nil {
		r.cfg.Cutover.Release(ctx, ref)
		return apiv1.Run{}, ref, err
	}
	if err := r.cfg.Cutover.Started(ctx, ref, run.RunId); err != nil {
		return run, ref, err
	}
	return run, ref, nil
}

// Scope is the plans.OnboardingScope of an onboarding's plan id, for
// resuming its halted run (plans.Service.SetOnboardingScopes). It puts the
// fence user's password into the environment again for the parse.
func (r *Runner) Scope(ctx context.Context, planID string) (plans.OnboardingScope, bool, error) {
	n := len(onboard.IDPrefix) + 12
	if len(planID) <= n || !onboard.ID.MatchString(planID[:n]) || planID[n] != '-' {
		return plans.OnboardingScope{}, false, nil
	}
	x, err := r.load(ctx, planID[:n])
	if errors.Is(err, ErrNotFound) {
		return plans.OnboardingScope{}, false, nil
	}
	if err != nil {
		return plans.OnboardingScope{}, false, err
	}
	_, scope, err := r.plan(ctx, x)
	if err != nil {
		return plans.OnboardingScope{}, false, err
	}
	pw, err := r.open(x, secrets.OnboardingSuperuserPassword, x.row.SuperuserPasswordNonce, x.row.SuperuserPasswordCiphertext)
	if err != nil {
		return plans.OnboardingScope{}, false, err
	}
	_ = os.Setenv(onboard.LegacyPasswordEnv, pw)
	return scope, true, nil
}

// plan is the engine plan of x's attempt as internal/onboard documents it,
// and its scope.
func (r *Runner) plan(ctx context.Context, x *rec) ([]byte, plans.OnboardingScope, error) {
	f := x.facts
	if f == nil || f.DB == nil || f.Target == nil {
		return nil, plans.OnboardingScope{}, errors.New("the facts have no database or target")
	}
	target, err := r.cfg.Apps.Target(ctx, x.row.App, x.doc.TargetEnv)
	if err != nil {
		return nil, plans.OnboardingScope{}, err
	}
	smoke, err := r.cfg.Apps.BackendURL(ctx, x.row.App, x.doc.TargetEnv)
	if err != nil {
		return nil, plans.OnboardingScope{}, err
	}
	svc, replicas, err := r.publicService(ctx, x.row.App)
	if err != nil {
		return nil, plans.OnboardingScope{}, err
	}
	host := x.doc.Internal.SourceHost
	app := map[string]any{"mode": "cold", "kind": "Deployment", "name": x.row.App + "-" + svc.Name, "replicas": replicas, "smokeUrl": smoke}
	if svc.HealthCheck != "" {
		app["smoke"] = []map[string]any{{"method": "GET", "path": svc.HealthCheck}}
	}
	tgt := map[string]any{"env": target.Endpoint.Env, "namespace": target.Endpoint.Namespace, "cluster": target.Endpoint.Cluster,
		"dsn": target.Endpoint.Dsn, "app": app}
	if target.Endpoint.Context != nil {
		tgt["context"] = *target.Endpoint.Context
	}
	if len(f.Target.Reindex) > 0 {
		tgt["reindex"] = f.Target.Reindex
	}
	p := map[string]any{
		"id": onboard.PlanID(x.row.ID, x.doc.Internal.Attempt),
		"source": map[string]any{"kind": "external", "name": onboard.SourceName, "env": onboard.SourceName, "namespace": "", "cluster": "",
			"dsn": "postgres://" + f.DB.FenceUser + ":${" + onboard.LegacyPasswordEnv + "}@" + host + "/" + f.DB.Database + "?sslmode=prefer"},
		"target": tgt,
		"verify": map[string]any{"fenceGate": true},
	}
	backend := target.GatewayBackend
	if backend == "" {
		backend = x.doc.TargetEnv
	}
	if target.GatewayURL != "" {
		p["gateway"] = map[string]any{"mode": "http", "url": plans.GatewayAppURL(target.GatewayURL, x.row.App), "token": "${GATEWAY_TOKEN}",
			"sourceBackend": onboard.SourceName, "targetBackend": backend}
	}
	scope := plans.OnboardingScope{App: x.row.App, SourceHost: host, SmokeURL: smoke}
	if f.Storage != nil && target.Bucket != "" {
		scope.SourceBucket = onboard.SourceName + ":" + f.Storage.Bucket
		p["files"] = map[string]any{"source": scope.SourceBucket, "target": target.Bucket}
	}
	raw, err := json.Marshal(p)
	return raw, scope, err
}

// superuserTarget is the new primary's DSN (the platform's route to its
// -rw service, as the plan's target) as the source's fence user: roles were
// replicated physically, so its password is the source's.
func (r *Runner) superuserTarget(ctx context.Context, x *rec) (string, error) {
	target, err := r.cfg.Apps.Target(ctx, x.row.App, x.doc.TargetEnv)
	if err != nil {
		return "", err
	}
	pw, err := r.open(x, secrets.OnboardingSuperuserPassword, x.row.SuperuserPasswordNonce, x.row.SuperuserPasswordCiphertext)
	if err != nil {
		return "", err
	}
	u, err := url.Parse(os.ExpandEnv(target.Endpoint.Dsn))
	if err != nil || x.facts == nil || x.facts.DB == nil || x.facts.DB.FenceUser == "" {
		return "", errors.New("the target's DSN or the fence user is not known")
	}
	u.User = url.UserPassword(x.facts.DB.FenceUser, pw)
	return u.String(), nil
}

func (r *Runner) publicService(ctx context.Context, app string) (appspec.Service, int, error) {
	a, err := r.cfg.Store.GetApp(ctx, app)
	if err != nil {
		return appspec.Service{}, 0, err
	}
	var spec appspec.Spec
	if err := json.Unmarshal(a.Spec, &spec); err != nil {
		return appspec.Service{}, 0, fmt.Errorf("the spec of %s: %w", app, err)
	}
	for _, s := range spec.Services {
		if s.Public {
			return s, max(s.Replicas, 1), nil
		}
	}
	return appspec.Service{}, 0, fmt.Errorf("app %s has no public service", app)
}

var imageMajor = regexp.MustCompile(`:(\d+)(\.\d+)?(-|@|$)`)

// gates are the platform's version and capacity gates of an external
// source (ADR 0042 "5."): the facts against the chosen image and PVC.
func gates(x *rec) error {
	f := x.facts
	if m := imageMajor.FindStringSubmatch(f.Target.Image); m != nil && f.DB.ServerVersionNum > 0 {
		if major, _ := strconv.Atoi(m[1]); major != f.DB.ServerVersionNum/10000 {
			return fmt.Errorf("version gate: the image %s is PostgreSQL %d, the source %d", f.Target.Image, major, f.DB.ServerVersionNum/10000)
		}
	}
	if size := quantity(f.Target.StorageSize); size > 0 {
		var db int64
		for _, d := range f.DB.Databases {
			db += d.SizeBytes
		}
		if db > size {
			return fmt.Errorf("capacity gate: the source has %d bytes, the PVC %s", db, f.Target.StorageSize)
		}
	}
	return nil
}

// quantity reads "2Gi", "512Mi"; 0 when it cannot.
func quantity(s string) int64 {
	for suffix, mul := range map[string]int64{"Ki": 1 << 10, "Mi": 1 << 20, "Gi": 1 << 30, "Ti": 1 << 40} {
		if n, ok := strings.CutSuffix(s, suffix); ok {
			v, err := strconv.ParseInt(n, 10, 64)
			if err != nil {
				return 0
			}
			return v * mul
		}
	}
	return 0
}

// cuttingOver follows the run: success goes on after it, a run that ended
// before the promotion goes back to ready, a halt waits for the run's resume.
func (r *Runner) cuttingOver(ctx context.Context, x *rec) (bool, error) {
	if x.doc.RunId == nil {
		return false, nil // Cutover is starting it
	}
	if !x.postRunDone("run") {
		run, err := r.cfg.Cutover.Get(ctx, *x.doc.RunId)
		if err != nil {
			return false, err
		}
		switch run.Status {
		case apiv1.RunStatusSucceeded:
			_, err := r.update(ctx, x.row.ID, []string{store.OnboardingCuttingOver}, func(x *rec) (string, error) {
				x.doc.Internal.PostRun = append(x.doc.Internal.PostRun, "run")
				return "", nil
			})
			return err == nil, err
		case apiv1.RunStatusRolledBack, apiv1.RunStatusAborted, apiv1.RunStatusFailed:
			_, err := r.update(ctx, x.row.ID, []string{store.OnboardingCuttingOver}, func(x *rec) (string, error) {
				return store.OnboardingReady, nil
			})
			if err == nil {
				r.event(ctx, x, apiv1.OnboardingEventKindStep, "the cutover run ended "+string(run.Status)+" before the promotion; the source takes writes again", nil)
			}
			return false, err
		default: // running, awaiting_switch, halted, interrupted
			return false, nil
		}
	}
	return r.postRun(ctx, x)
}

// postRun is what the platform does after a successful run (ADR 0042 "5.").
func (r *Runner) postRun(ctx context.Context, x *rec) (bool, error) {
	done := func(step string) error {
		_, err := r.update(ctx, x.row.ID, []string{store.OnboardingCuttingOver}, func(x *rec) (string, error) {
			x.doc.Internal.PostRun = append(x.doc.Internal.PostRun, step)
			return "", nil
		})
		return err
	}
	res, err := r.pgResource(ctx, x)
	if err != nil {
		return false, err
	}
	cluster := x.row.App + "-" + res
	if !x.postRunDone("replica_source") {
		if peer := x.doc.Internal.TargetPeer; peer != "" {
			if r.cfg.Kube == nil {
				return false, fmt.Errorf("%w: kube", ErrNotConfigured)
			}
			if err := r.cfg.Kube.SetReplicaSource(ctx, x.doc.TargetEnv, x.row.App, cluster, cluster+"-"+peer); err != nil {
				return false, fmt.Errorf("point replica.source at the peer: %w", err)
			}
		}
		return true, done("replica_source")
	}
	if !x.postRunDone("source_kind") {
		if err := r.cfg.Apps.FinishSource(ctx, x.row.App); err != nil && !errors.Is(err, store.ErrConflict) {
			return false, err
		}
		return true, done("source_kind")
	}
	if !x.postRunDone("redeploy") {
		dep, err := r.cfg.Apps.Redeploy(ctx, x.row.App, deref(x.doc.ApprovedBy))
		if err != nil {
			return false, err
		}
		_, err = r.update(ctx, x.row.ID, []string{store.OnboardingCuttingOver}, func(x *rec) (string, error) {
			x.doc.Internal.Deployment = dep
			x.doc.Internal.PostRun = append(x.doc.Internal.PostRun, "redeploy")
			return "", nil
		})
		return err == nil, err
	}
	if !x.postRunDone("redeployed") {
		status, msg, err := r.cfg.Apps.Deployment(ctx, x.doc.Internal.Deployment)
		if err != nil {
			return false, err
		}
		switch status {
		case store.DeployHealthy:
			return true, done("redeployed")
		case store.DeployFailed, store.DeploySuperseded:
			return false, fmt.Errorf("the deployment with the app's replicas %s: %s", status, msg)
		}
		return false, nil
	}
	if !x.postRunDone("drop_replicator") {
		if r.cfg.SourceDB != nil {
			dsn, err := r.superuserTarget(ctx, x)
			if err != nil {
				return false, err
			}
			if err := r.cfg.SourceDB.DropRole(ctx, dsn, onboard.ReplicationUser); err != nil {
				r.event(ctx, x, apiv1.OnboardingEventKindLog, "drop "+onboard.ReplicationUser+": "+err.Error(), nil)
				return false, nil
			}
		}
		if err := done("drop_replicator"); err != nil {
			return false, err
		}
	}
	_, err = r.update(ctx, x.row.ID, []string{store.OnboardingCuttingOver}, func(x *rec) (string, error) {
		return store.OnboardingDecommissioning, nil
	})
	return err == nil, err
}
