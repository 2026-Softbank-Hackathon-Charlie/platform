package job

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/airun"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/gateway"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/onboard"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/secrets"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/onboard/facts"
)

// step does the next thing of id's state. moved says the state or the
// onboarding's place in it changed, so the next step may follow at once;
// false means it waits (for a Job, a deployment, a person or the clock).
func (r *Runner) step(ctx context.Context, id string) (bool, error) {
	x, err := r.load(ctx, id)
	if err != nil {
		return false, err
	}
	if x.doc.Deleting {
		if !x.doc.Internal.DeleteRequested {
			return false, nil // the undo failed: only DELETE tries again
		}
		return false, r.undo(ctx, x)
	}
	if x.doc.Internal.Waiting {
		return false, nil
	}
	var run func(context.Context, *rec) (bool, error)
	switch x.row.State {
	case store.OnboardingDiscovering:
		run = r.discovering
	case store.OnboardingPatching:
		run = r.patching
	case store.OnboardingChecking:
		run = r.checking
	case store.OnboardingConnecting:
		run = r.connecting
	case store.OnboardingRegistering:
		run = r.registering
	case store.OnboardingSyncing:
		run = r.syncing
	case store.OnboardingCuttingOver:
		run = r.cuttingOver
	case store.OnboardingDecommissioning:
		run = r.decommissioning
	default: // awaiting_approval, ready, failed, done: a person goes on
		return false, nil
	}
	moved, err := run(ctx, x)
	if err != nil && !errors.Is(err, store.ErrConflict) && !errors.Is(err, ErrNotFound) && ctx.Err() == nil {
		if ferr := r.fail(ctx, id, x.row.State, err); ferr != nil {
			return false, ferr
		}
		return false, nil
	}
	return moved, err
}

func (r *Runner) connectReq(x *rec) ConnectRequest {
	req := ConnectRequest{ID: x.row.ID, App: x.row.App}
	if x.facts != nil {
		req.Facts = *x.facts
	}
	return req
}

// --- discovering ---

func (r *Runner) discovering(ctx context.Context, x *rec) (bool, error) {
	if r.cfg.Discover == nil {
		return false, fmt.Errorf("%w: discovery", ErrNotConfigured)
	}
	d, err := r.cfg.Discover.Discover(ctx, DiscoverRequest{ID: x.row.ID, Repo: x.row.Repo, ServiceURL: x.doc.ServiceUrl, TargetEnv: x.doc.TargetEnv})
	if err != nil {
		return false, fmt.Errorf("discovery: %w", err)
	}
	f := d.Facts
	var to string
	x2, err := r.update(ctx, x.row.ID, []string{store.OnboardingDiscovering}, func(x *rec) (string, error) {
		x.facts = &f
		x.row.KeyVersion, x.row.KeyCheck = d.KeyVersion, d.KeyCheck
		if d.KeyCheck == "" {
			x.row.KeyVersion, x.row.KeyCheck = d.KeyVersion, d.KeyCheck
		if d.KeyCheck == "" {
			x.row.KeyVersion, x.row.KeyCheck = secrets.KeyVersion, r.keyCheck()
		}
		}
		x.row.SecretsNonce, x.row.SecretsCiphertext = d.Secrets.Nonce, d.Secrets.Ciphertext
		x.row.SuperuserPasswordNonce, x.row.SuperuserPasswordCiphertext = d.SuperuserPassword.Nonce, d.SuperuserPassword.Ciphertext
		x.doc.HumanActions = d.HumanActions
		x.doc.Report = report(f.Report)
		x.doc.Facts = summary(f)
		if x.doc.TargetEnv == "" && f.Target != nil {
			x.doc.TargetEnv = f.Target.Env
		}
		x.doc.Internal.TargetPeer = r.peerOf(x.doc.TargetEnv)
		switch {
		case slices.ContainsFunc(d.HumanActions, func(a apiv1.OnboardingHumanAction) bool {
			return !a.Done && a.Kind == apiv1.OnboardingHumanActionKindInstallGithubApp
		}):
			x.doc.Internal.Waiting = true
			return "", nil
		case len(f.Report.Blockers) > 0:
			codes := make([]string, len(f.Report.Blockers))
			for i, b := range f.Report.Blockers {
				codes[i] = b.Code
			}
			st := apiv1.OnboardingStateDiscovering
			x.doc.FailedState, x.doc.Failure = &st, ptr("blocked: "+strings.Join(codes, ", "))
			to = store.OnboardingFailed
			return to, nil
		case x.doc.TargetEnv == "":
			return "", errors.New("discovery chose no target environment")
		case f.GitHub != nil && f.GitHub.PlatformYAML:
			x.doc.Internal.SkipPatch, x.doc.Internal.Branch = true, f.GitHub.DefaultBranch
			to = store.OnboardingChecking
			return to, nil
		}
		to = store.OnboardingPatching
		return to, nil
	})
	if err != nil {
		return false, err
	}
	if x2.doc.Internal.Waiting {
		r.event(ctx, x2, apiv1.OnboardingEventKindHuman, "waiting for a person (humanActions); resume checks again", nil)
		return false, nil
	}
	if to == store.OnboardingFailed {
		r.event(ctx, x2, apiv1.OnboardingEventKindError, deref(x2.doc.Failure), nil)
		return false, nil
	}
	return true, nil
}

func (r *Runner) keyCheck() string {
	if r.cfg.Secrets == nil {
		return ""
	}
	return r.cfg.Secrets.KeyCheck()
}

// peerOf is the other environment of a two-cloud pair (eks and gke).
func (r *Runner) peerOf(env string) string {
	for _, e := range r.cfg.Envs {
		if e != env && (e == "eks" || e == "gke") && (env == "eks" || env == "gke") {
			return e
		}
	}
	return ""
}

// --- patching ---

// hopaiResult is the "kind":"onboard" line of hopai (ai-sandbox #49).
type hopaiResult struct {
	Kind       string `json:"kind"`
	Head       string `json:"head"`
	Base       string `json:"base"`
	Branch     string `json:"branch"`
	Deployable bool   `json:"deployable"`
	Summary    string `json:"summary"`
	Blockers   []struct {
		Need, Reason string
	} `json:"blockers"`
}

func (r *Runner) patching(ctx context.Context, x *rec) (bool, error) {
	if r.cfg.AI == nil || r.cfg.GitHub == nil {
		return false, fmt.Errorf("%w: hopai or GitHub", ErrNotConfigured)
	}
	if x.doc.Internal.HopaiJob == "" {
		factsJSON, _ := json.Marshal(x.facts)
		req := airun.OnboardRequest{ID: x.row.ID, App: x.row.App, Repo: x.row.Repo, Facts: factsJSON}
		if x.doc.Internal.CheckLog != "" {
			req.CheckLog = []byte(x.doc.Internal.CheckLog)
		}
		run, err := r.cfg.AI.StartOnboard(ctx, req, x.doc.CreatedBy)
		if errors.Is(err, airun.ErrBusy) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("start hopai: %w", err)
		}
		_, err = r.update(ctx, x.row.ID, []string{store.OnboardingPatching}, func(x *rec) (string, error) {
			x.doc.Internal.HopaiJob = run.Name
			return "", nil
		})
		if err == nil {
			r.event(ctx, x, apiv1.OnboardingEventKindStep, "hopai started: "+run.Name, map[string]any{"job": run.Name})
		}
		return err == nil, err
	}
	runs, err := r.cfg.AI.ListOnboard(ctx, x.row.ID)
	if err != nil {
		return false, err
	}
	i := slices.IndexFunc(runs, func(a airun.Run) bool { return a.Name == x.doc.Internal.HopaiJob })
	if i < 0 {
		return false, fmt.Errorf("hopai job %s is gone", x.doc.Internal.HopaiJob)
	}
	if runs[i].Status == "running" {
		return false, nil
	}
	out, err := r.cfg.AI.Outcome(ctx, x.doc.Internal.HopaiJob)
	if err != nil {
		return false, err
	}
	var res hopaiResult
	_ = json.Unmarshal(out.Result, &res)
	switch {
	case out.ExitCode == 3 && len(res.Blockers) == 0 && x.facts != nil && x.facts.GitHub != nil:
		// The default branch has platform.yaml already: no PR.
		_, err := r.update(ctx, x.row.ID, []string{store.OnboardingPatching}, func(x *rec) (string, error) {
			x.doc.Internal.HopaiJob, x.doc.Internal.SkipPatch, x.doc.Internal.Branch = "", true, x.facts.GitHub.DefaultBranch
			return store.OnboardingChecking, nil
		})
		return err == nil, err
	case out.ExitCode == 3:
		need := make([]string, len(res.Blockers))
		for i, b := range res.Blockers {
			need[i] = b.Need + ": " + b.Reason
		}
		_, err := r.update(ctx, x.row.ID, []string{store.OnboardingPatching}, func(x *rec) (string, error) {
			x.doc.Report.Blockers = append(x.doc.Report.Blockers, apiv1.OnboardingFinding{Code: apiv1.OnboardingFindingCode(facts.BlockAppCodeChange), Message: strings.Join(need, "; ")})
			return "", nil
		})
		if err != nil {
			return false, err
		}
		return false, fmt.Errorf("hopai cannot add the contract: %s", strings.Join(need, "; "))
	case out.ExitCode != 0 || res.Head == "":
		return false, fmt.Errorf("hopai failed (exit %d): %s", out.ExitCode, tail(out.Log, 500))
	}
	base := "main"
	if x.facts != nil && x.facts.GitHub != nil && x.facts.GitHub.DefaultBranch != "" {
		base = x.facts.GitHub.DefaultBranch
	}
	body := "hopcharlie 온보딩 " + x.row.ID + "의 계약 파일이다. 원본 배포에 영향을 주지 않는 새 파일만 더한다.\n\n" + res.Summary
	pr, err := r.cfg.GitHub.OpenPR(ctx, x.row.Repo, onboard.Branch, base, "hopcharlie 계약 파일 추가 ("+x.row.ID+")", body)
	if err != nil {
		return false, err
	}
	_, err = r.update(ctx, x.row.ID, []string{store.OnboardingPatching}, func(x *rec) (string, error) {
		x.doc.Internal.HopaiJob, x.doc.Internal.HopaiResult, x.doc.Internal.Branch = "", string(out.Result), onboard.Branch
		x.doc.Pr = &apiv1.OnboardingPR{Number: pr.Number, Url: pr.URL, HeadSha: pr.HeadSHA}
		return store.OnboardingChecking, nil
	})
	return err == nil, err
}

// --- checking ---

func (r *Runner) checking(ctx context.Context, x *rec) (bool, error) {
	if r.cfg.Apps == nil {
		return false, fmt.Errorf("%w: apps", ErrNotConfigured)
	}
	app := onboard.TestApp(x.row.ID)
	c := x.doc.Check
	if c == nil || c.DeploymentId == nil {
		attempts := 1
		if c != nil {
			attempts = c.Attempts + 1
		}
		dep, err := r.cfg.Apps.DeployTest(ctx, TestApp{Name: app, Repo: x.row.Repo, Branch: x.doc.Internal.Branch, Env: x.doc.TargetEnv, Secrets: secretNames(x.facts)})
		if err != nil {
			return false, fmt.Errorf("deploy the test app: %w", err)
		}
		_, err = r.update(ctx, x.row.ID, []string{store.OnboardingChecking}, func(x *rec) (string, error) {
			x.doc.Internal.TestApp = true
			x.doc.Check = &apiv1.OnboardingCheck{App: app, Attempts: attempts, DeploymentId: &dep, Result: apiv1.OnboardingCheckResultRunning}
			return "", nil
		})
		return err == nil, err
	}
	status, msg, err := r.cfg.Apps.Deployment(ctx, *c.DeploymentId)
	if err != nil {
		return false, err
	}
	switch status {
	case store.DeployHealthy:
	case store.DeployFailed, store.DeploySuperseded:
	default:
		return false, nil
	}
	if status != store.DeployHealthy {
		log, _ := r.cfg.Apps.DeploymentLog(ctx, *c.DeploymentId)
		if err := r.cfg.Apps.Remove(ctx, app); err != nil {
			return false, err
		}
		if c.Attempts >= onboard.CheckAttempts || x.doc.Internal.SkipPatch {
			_, err := r.update(ctx, x.row.ID, []string{store.OnboardingChecking}, func(x *rec) (string, error) {
				x.doc.Internal.TestApp = false
				x.doc.Check.Result, x.doc.Check.Message = apiv1.OnboardingCheckResultFailed, &msg
				x.doc.Report.Blockers = append(x.doc.Report.Blockers, apiv1.OnboardingFinding{Code: apiv1.OnboardingFindingCode(facts.BlockCheckFailed), Message: msg})
				return "", nil
			})
			if err != nil {
				return false, err
			}
			return false, fmt.Errorf("the test deployment failed %d times: %s", c.Attempts, msg)
		}
		_, err := r.update(ctx, x.row.ID, []string{store.OnboardingChecking}, func(x *rec) (string, error) {
			x.doc.Internal.TestApp = false
			x.doc.Internal.CheckLog = tail(string(log), onboard.CheckLogMax)
			x.doc.Check.Result, x.doc.Check.Message, x.doc.Check.DeploymentId = apiv1.OnboardingCheckResultFailed, &msg, nil
			return store.OnboardingPatching, nil
		})
		return err == nil, err
	}
	// Healthy: how long the app took to start is part of the hold.
	start := 0.0
	if r.cfg.Kube != nil && x.facts != nil && x.facts.Target != nil {
		if s, err := r.cfg.Kube.StartSeconds(ctx, x.doc.TargetEnv, app, app+"-"+x.facts.Target.PublicService); err == nil {
			start = s
		}
	}
	if err := r.cfg.Apps.Remove(ctx, app); err != nil {
		return false, err
	}
	hold := holdEstimate(x.facts, start)
	over := hold.TotalSeconds > float64(facts.HoldBudgetSeconds)
	_, err = r.update(ctx, x.row.ID, []string{store.OnboardingChecking}, func(x *rec) (string, error) {
		x.doc.Internal.TestApp = false
		x.doc.Check.Result = apiv1.OnboardingCheckResultPassed
		x.doc.Check.StartSeconds = ptr(float32(start))
		x.doc.Report.HoldSeconds, x.doc.Report.HoldBudgetSeconds = ptr(float32(hold.TotalSeconds)), ptr(float32(facts.HoldBudgetSeconds))
		if x.facts != nil {
			x.facts.Report.Hold = &hold
		}
		if over {
			x.doc.Report.Blockers = append(x.doc.Report.Blockers, apiv1.OnboardingFinding{Code: apiv1.OnboardingFindingCode(facts.BlockHoldBudget),
				Message: fmt.Sprintf("the cutover would hold writes about %.1fs, over the %ds budget", hold.TotalSeconds, facts.HoldBudgetSeconds)})
			return "", nil
		}
		planned := plannedChanges(x)
		x.doc.PlannedLegacyChanges = &planned
		return store.OnboardingAwaitingApproval, nil
	})
	if err != nil {
		return false, err
	}
	if over {
		return false, fmt.Errorf("hold budget exceeded: %.1fs", hold.TotalSeconds)
	}
	return true, nil
}

// holdEstimate is the facts' estimate with the start time checking measured.
func holdEstimate(f *facts.Facts, start float64) facts.HoldEstimate {
	var h facts.HoldEstimate
	if f != nil && f.Report.Hold != nil {
		h = *f.Report.Hold
	}
	h.AppStartSeconds = start
	h.BudgetSeconds = facts.HoldBudgetSeconds
	h.TotalSeconds = h.FinalCopySeconds + h.ReplaySeconds + h.FenceGateSeconds + h.PromoteSeconds + h.ReindexSeconds + h.AppStartSeconds
	return h
}

// secretNames are the variables the app keeps as secrets.
func secretNames(f *facts.Facts) []string {
	if f == nil || f.VM == nil {
		return nil
	}
	var out []string
	for _, c := range f.VM.Containers {
		for _, e := range c.Env {
			if e.Secret && e.Excluded == "" && !slices.Contains(out, e.Name) {
				out = append(out, e.Name)
			}
		}
	}
	slices.Sort(out)
	return out
}

// plannedChanges is what connecting will do, for the approval screen.
func plannedChanges(x *rec) []apiv1.OnboardingPlannedChange {
	f, app := x.facts, x.row.App
	var out []apiv1.OnboardingPlannedChange
	add := func(kind apiv1.OnboardingLegacyChangeKind, target, detail string) {
		c := apiv1.OnboardingPlannedChange{Kind: kind, Target: target}
		if detail != "" {
			c.Detail = &detail
		}
		out = append(out, c)
	}
	if f != nil && f.GitHub != nil {
		for _, w := range f.GitHub.Workflows {
			if w.DeploysToVM {
				add(apiv1.OnboardingLegacyChangeKindWorkflowDisabled, w.Path, "")
			}
		}
	}
	if f != nil && f.Instance != nil {
		add(apiv1.OnboardingLegacyChangeKindInstanceTagged, f.Instance.ID, onboard.InstanceTag+"="+x.row.ID)
	}
	if f != nil && f.DB != nil && !f.DB.PostgresRole {
		add(apiv1.OnboardingLegacyChangeKindPostgresRoleCreated, "postgres", "SUPERUSER LOGIN, no password")
	}
	add(apiv1.OnboardingLegacyChangeKindReplicationRoleCreated, onboard.ReplicationUser, "LOGIN REPLICATION")
	add(apiv1.OnboardingLegacyChangeKindTailscaleVolumeCreated, onboard.TailscaleVolume(app), "")
	add(apiv1.OnboardingLegacyChangeKindTailscaleContainerStarted, onboard.TailscaleContainer(app), "")
	add(apiv1.OnboardingLegacyChangeKindTailscaleDeviceJoined, onboard.TailnetHost(app), onboard.TailscaleTag)
	subnet := ""
	if f != nil && f.VM != nil && f.DB != nil {
		for _, n := range f.VM.Networks {
			if n.Subnet != "" {
				subnet = n.Subnet
				break
			}
		}
	}
	add(apiv1.OnboardingLegacyChangeKindHbaLineAdded, "pg_hba.conf", "host replication "+onboard.ReplicationUser+" "+subnet+" scram-sha-256")
	add(apiv1.OnboardingLegacyChangeKindWalKeepSizeAdded, "postgresql.conf", "wal_keep_size = '"+onboard.WALKeepSize+"'")
	add(apiv1.OnboardingLegacyChangeKindDbRouteCreated, onboard.RouteName(app), "")
	return out
}

// --- connecting ---

func (r *Runner) connecting(ctx context.Context, x *rec) (bool, error) {
	if r.cfg.GitHub == nil || r.cfg.Connect == nil || r.cfg.Routes == nil {
		return false, fmt.Errorf("%w: GitHub, connect or routes", ErrNotConfigured)
	}
	// 1. The deploy workflows, off; a run in progress ends or is cancelled.
	if x.facts != nil && x.facts.GitHub != nil {
		for _, w := range x.facts.GitHub.Workflows {
			if !w.DeploysToVM || x.hasChange(apiv1.OnboardingLegacyChangeKindWorkflowDisabled, w.Path) {
				continue
			}
			if err := r.cfg.GitHub.DisableWorkflow(ctx, x.row.Repo, w.ID); err != nil {
				return false, fmt.Errorf("disable %s: %w", w.Path, err)
			}
			if _, err := r.update(ctx, x.row.ID, nil, func(x *rec) (string, error) {
				x.addChange(apiv1.OnboardingLegacyChange{At: r.cfg.Now(), Kind: apiv1.OnboardingLegacyChangeKindWorkflowDisabled, Target: w.Path})
				x.doc.LegacyDeployStopped = true
				return "", nil
			}); err != nil {
				return false, err
			}
		}
		active := false
		for _, w := range x.facts.GitHub.Workflows {
			if !w.DeploysToVM {
				continue
			}
			list, err := r.cfg.GitHub.ActiveRuns(ctx, x.row.Repo, w.ID)
			if err != nil {
				return false, err
			}
			for _, run := range list {
				active = true
				if x.doc.Internal.WorkflowsSince != nil && r.cfg.Now().Sub(*x.doc.Internal.WorkflowsSince) > onboard.WorkflowWait {
					_ = r.cfg.GitHub.CancelRun(ctx, x.row.Repo, run.ID)
				}
			}
		}
		if active {
			if x.doc.Internal.WorkflowsSince == nil {
				_, err := r.update(ctx, x.row.ID, nil, func(x *rec) (string, error) {
					x.doc.Internal.WorkflowsSince = ptr(r.cfg.Now())
					return "", nil
				})
				return false, err
			}
			return false, nil
		}
	}
	// 2-4. The VM and its database.
	if !x.doc.Internal.Connected {
		changes, pw, err := r.cfg.Connect.Connect(ctx, r.connectReq(x))
		_, uerr := r.update(ctx, x.row.ID, nil, func(x *rec) (string, error) {
			for _, c := range changes {
				x.addChange(c)
			}
			if err == nil {
				x.row.ReplicationPasswordNonce, x.row.ReplicationPasswordCiphertext = pw.Nonce, pw.Ciphertext
				x.doc.Internal.Connected = true
			}
			return "", nil
		})
		if err != nil {
			return false, fmt.Errorf("connect the source: %w", err)
		}
		if uerr != nil {
			return false, uerr
		}
		x, _ = r.load(ctx, x.row.ID)
	}
	// 5. The management cluster's route.
	if x.doc.Internal.SourceHost == "" {
		host, err := r.cfg.Routes.EnsureLegacy(ctx, x.row.App)
		if err != nil {
			return false, fmt.Errorf("the route to the source: %w", err)
		}
		if _, err := r.update(ctx, x.row.ID, nil, func(x *rec) (string, error) {
			x.addChange(apiv1.OnboardingLegacyChange{At: r.cfg.Now(), Kind: apiv1.OnboardingLegacyChangeKindDbRouteCreated, Target: onboard.RouteName(x.row.App)})
			x.doc.Internal.SourceHost = host
			return "", nil
		}); err != nil {
			return false, err
		}
		x, _ = r.load(ctx, x.row.ID)
	}
	// 6. Superuser and replication connections through it.
	if r.cfg.SourceDB != nil && x.facts != nil && x.facts.DB != nil {
		su, err := r.open(x, secrets.OnboardingSuperuserPassword, x.row.SuperuserPasswordNonce, x.row.SuperuserPasswordCiphertext)
		if err != nil {
			return false, err
		}
		repl, err := r.open(x, secrets.OnboardingReplicationPassword, x.row.ReplicationPasswordNonce, x.row.ReplicationPasswordCiphertext)
		if err != nil {
			return false, err
		}
		if err := r.cfg.SourceDB.Check(ctx, x.doc.Internal.SourceHost, x.facts.DB.Database, x.facts.DB.FenceUser, su, repl); err != nil {
			// The tailnet proxy may need a moment; try again next round.
			r.event(ctx, x, apiv1.OnboardingEventKindLog, "the source does not answer through the route yet: "+err.Error(), nil)
			return false, nil
		}
	}
	_, err := r.update(ctx, x.row.ID, []string{store.OnboardingConnecting}, func(x *rec) (string, error) {
		x.doc.Internal.SourceChecked = true
		return store.OnboardingRegistering, nil
	})
	return err == nil, err
}

func (r *Runner) open(x *rec, field string, nonce, ct []byte) (string, error) {
	if ct == nil {
		return "", nil
	}
	if r.cfg.Secrets == nil {
		return "", fmt.Errorf("%w: secrets key", ErrNotConfigured)
	}
	return r.cfg.Secrets.OpenOnboarding(x.row, field, nonce, ct)
}

// --- registering ---

func (r *Runner) registering(ctx context.Context, x *rec) (bool, error) {
	if r.cfg.Apps == nil || r.cfg.Gateway == nil || r.cfg.GitHub == nil {
		return false, fmt.Errorf("%w: apps, gateway or GitHub", ErrNotConfigured)
	}
	f := x.facts
	if f == nil || f.DB == nil || f.Target == nil {
		return false, errors.New("the facts have no database or target")
	}
	// The PR, squash-merged at the commit approved.
	if x.doc.Pr != nil && (x.doc.Pr.Merged == nil || !*x.doc.Pr.Merged) {
		pr, err := r.cfg.GitHub.GetPR(ctx, x.row.Repo, x.doc.Pr.Number)
		if err != nil {
			return false, err
		}
		if !pr.Merged {
			if err := r.cfg.GitHub.MergePR(ctx, x.row.Repo, x.doc.Pr.Number, deref(x.doc.ApprovedSha)); err != nil {
				return false, fmt.Errorf("merge the PR: %w", err)
			}
		}
		if _, err := r.update(ctx, x.row.ID, nil, func(x *rec) (string, error) {
			x.doc.Pr.Merged = ptr(true)
			return "", nil
		}); err != nil {
			return false, err
		}
	}
	// The app, its database row and secrets, then its first deployment.
	if !x.doc.Internal.Registered {
		reg, err := r.registration(x)
		if err != nil {
			return false, err
		}
		dep, err := r.cfg.Apps.Register(ctx, reg)
		if err != nil {
			return false, fmt.Errorf("register the app: %w", err)
		}
		if _, err := r.update(ctx, x.row.ID, nil, func(x *rec) (string, error) {
			x.doc.Internal.Registered, x.doc.Internal.Deployment = true, dep
			return "", nil
		}); err != nil {
			return false, err
		}
		x, _ = r.load(ctx, x.row.ID)
	}
	// The gateway: legacy (dialled at the VM) and the target, writes and
	// reads to legacy. Again is the same.
	if !x.doc.Internal.GatewayReady {
		target, err := r.cfg.Apps.BackendURL(ctx, x.row.App, x.doc.TargetEnv)
		if err != nil {
			return false, err
		}
		spec := legacySpec(x, target)
		if err := r.cfg.Gateway.PutApp(ctx, x.row.App, spec); err != nil {
			return false, fmt.Errorf("register with the gateway: %w", err)
		}
		if _, err := r.update(ctx, x.row.ID, nil, func(x *rec) (string, error) {
			x.doc.Internal.GatewayReady = true
			return "", nil
		}); err != nil {
			return false, err
		}
	}
	status, msg, err := r.cfg.Apps.Deployment(ctx, x.doc.Internal.Deployment)
	if err != nil {
		return false, err
	}
	switch status {
	case store.DeployHealthy:
	case store.DeployFailed, store.DeploySuperseded:
		return false, fmt.Errorf("the first deployment %s: %s", status, msg)
	default:
		return false, nil
	}
	_, err = r.update(ctx, x.row.ID, []string{store.OnboardingRegistering}, func(x *rec) (string, error) {
		return store.OnboardingSyncing, nil
	})
	return err == nil, err
}

// legacySpec is the gateway app while the onboarding syncs.
func legacySpec(x *rec, target string) gateway.AppSpec {
	spec := gateway.AppSpec{
		Backends:     map[string]string{onboard.SourceName: x.doc.ServiceUrl, x.doc.TargetEnv: target},
		WriteBackend: onboard.SourceName,
		ReadWeights:  map[string]int{onboard.SourceName: 100},
	}
	if x.facts != nil && x.facts.Instance != nil && x.facts.Instance.PublicIP != "" {
		spec.Dial = map[string]string{onboard.SourceName: x.facts.Instance.PublicIP + ":443"}
	}
	if x.facts != nil && x.facts.Target != nil {
		spec.HealthPath = x.facts.Target.HealthCheck
	}
	return spec
}

// registration is the app as registering stores it.
func (r *Runner) registration(x *rec) (Registration, error) {
	f := x.facts
	values, err := r.sourceValues(x)
	if err != nil {
		return Registration{}, err
	}
	su, err := r.open(x, secrets.OnboardingSuperuserPassword, x.row.SuperuserPasswordNonce, x.row.SuperuserPasswordCiphertext)
	if err != nil {
		return Registration{}, err
	}
	repl, err := r.open(x, secrets.OnboardingReplicationPassword, x.row.ReplicationPasswordNonce, x.row.ReplicationPasswordCiphertext)
	if err != nil {
		return Registration{}, err
	}
	appPW := su
	if f.DB.Owner != f.DB.FenceUser {
		appPW = values[f.DB.Container]["POSTGRES_PASSWORD"]
		if f.DB.Owner != f.DB.EnvUser || appPW == "" {
			return Registration{}, fmt.Errorf("the password of the app account %s is not known (only %s's)", f.DB.Owner, f.DB.FenceUser)
		}
	}
	host := onboard.TailnetHost(x.row.App)
	return Registration{App: x.row.App, Repo: x.row.Repo, Branch: f.GitHub.DefaultBranch, Env: x.doc.TargetEnv, Peer: x.doc.Internal.TargetPeer,
		Actor: deref(x.doc.ApprovedBy), Image: f.Target.Image, Database: f.DB.Database, Owner: f.DB.Owner, StorageSize: f.Target.StorageSize,
		ExternalHost: host, Reindex: f.Target.Reindex, AppPassword: appPW, ReplicationPassword: repl,
		Secrets: AppSecrets(f, values)}, nil
}

// sourceValues are the environment values discovery sealed.
func (r *Runner) sourceValues(x *rec) (map[string]map[string]string, error) {
	raw, err := r.open(x, secrets.OnboardingSecrets, x.row.SecretsNonce, x.row.SecretsCiphertext)
	if err != nil || raw == "" {
		return map[string]map[string]string{}, err
	}
	var v map[string]map[string]string
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return nil, errors.New("the sealed environment values are not a JSON object")
	}
	return v, nil
}

// platformNames are the variables the platform sets itself (ADR 0024 and
// the resources), never carried over.
var platformPrefixes = []string{"PG", "S3_", "DATABASE_URL", "HOPCHARLIE_", "PLATFORM_", "PORT"}

// AppSecrets are the source's values the app gets as secrets (ADR 0042
// "4."): the variables discovery marks secret and carried over, of every
// container but the database's, without the platform's names, the storage
// keys, and values that name a compose service.
func AppSecrets(f *facts.Facts, values map[string]map[string]string) map[string]string {
	out := map[string]string{}
	if f == nil || f.VM == nil {
		return out
	}
	var services []string
	for _, c := range f.VM.Containers {
		if c.Service != "" {
			services = append(services, c.Service)
		}
	}
	skip := map[string]bool{}
	if s := f.Storage; s != nil {
		for _, n := range []string{s.AccessKeyEnv, s.SecretKeyEnv, s.EndpointEnv, s.BucketEnv, s.RegionEnv} {
			skip[n] = n != ""
		}
	}
	for _, c := range f.VM.Containers {
		if f.DB != nil && c.Name == f.DB.Container {
			continue
		}
		for _, e := range c.Env {
			if !e.Secret || e.Excluded != "" || skip[e.Name] || slices.ContainsFunc(platformPrefixes, func(p string) bool { return strings.HasPrefix(e.Name, p) }) {
				continue
			}
			v, ok := values[c.Name][e.Name]
			if !ok || namesService(v, services) {
				continue
			}
			if _, set := out[e.Name]; !set {
				out[e.Name] = v
			}
		}
	}
	return out
}

// namesService says v names a compose service as a host: db:5432,
// //db/, @db.
func namesService(v string, services []string) bool {
	for _, s := range services {
		for _, form := range []string{"//" + s + ":", "//" + s + "/", "@" + s + ":", "@" + s + "/", s + ":"} {
			if strings.Contains(v, form) || v == s {
				return true
			}
		}
	}
	return false
}

// --- syncing ---

func (r *Runner) syncing(ctx context.Context, x *rec) (bool, error) {
	if r.cfg.SourceDB == nil || r.cfg.DNS == nil {
		return false, fmt.Errorf("%w: source database or DNS", ErrNotConfigured)
	}
	f := x.facts
	su, err := r.open(x, secrets.OnboardingSuperuserPassword, x.row.SuperuserPasswordNonce, x.row.SuperuserPasswordCiphertext)
	if err != nil {
		return false, err
	}
	streaming, lag, err := r.cfg.SourceDB.Replication(ctx, x.doc.Internal.SourceHost, f.DB.Database, f.DB.FenceUser, su)
	if err != nil {
		r.event(ctx, x, apiv1.OnboardingEventKindLog, "replication not read: "+err.Error(), nil)
		return false, nil
	}
	copied, pending := 0, 0
	if f != nil && f.Storage != nil && r.cfg.Files != nil {
		target, err := r.cfg.Apps.Target(ctx, x.row.App, x.doc.TargetEnv)
		if err != nil {
			return false, err
		}
		env, err := r.legacyRemote(x)
		if err != nil {
			return false, err
		}
		if target.Bucket != "" {
			copied, pending, err = r.cfg.Files.Copy(ctx, env, onboard.SourceName+":"+f.Storage.Bucket, target.Bucket)
			if err != nil {
				r.event(ctx, x, apiv1.OnboardingEventKindLog, "file copy: "+err.Error(), nil)
				pending = onboard.MaxFileDiff + 1
			}
		}
	}
	host := strings.TrimPrefix(x.doc.ServiceUrl, "https://")
	dns, err := r.cfg.DNS.PointsAtGateway(ctx, host)
	if err != nil {
		dns = false
	}
	r.event(ctx, x, apiv1.OnboardingEventKindStep, "syncing", map[string]any{
		"lagSeconds": lag.Seconds(), "filesCopied": copied, "filesPending": pending, "dnsToGateway": dns, "streaming": streaming})
	now := r.cfg.Now()
	ttl := 0
	if f != nil && f.HTTPS != nil {
		ttl = f.HTTPS.TTLSeconds
	}
	ready := false
	_, err = r.update(ctx, x.row.ID, []string{store.OnboardingSyncing}, func(x *rec) (string, error) {
		if dns {
			if x.doc.Internal.DNSAt == nil {
				x.doc.Internal.DNSAt = &now
			}
			for i := range x.doc.HumanActions {
				if x.doc.HumanActions[i].Kind == apiv1.OnboardingHumanActionKindDeleteDnsRecord {
					x.doc.HumanActions[i].Done = true
				}
			}
		} else {
			x.doc.Internal.DNSAt = nil
		}
		ready = streaming && lag <= onboard.MaxReplicationLag && pending <= onboard.MaxFileDiff &&
			x.doc.Internal.DNSAt != nil && now.Sub(*x.doc.Internal.DNSAt) >= time.Duration(ttl)*time.Second
		if ready {
			return store.OnboardingReady, nil
		}
		return "", nil
	})
	return ready && err == nil, err
}

// pgResource is the postgres resource's name in platform.yaml: hopai's
// mapping of the database's compose service, "db" when it does not say.
func (r *Runner) pgResource(x *rec) string {
	var res struct {
		Mapping []struct {
			ComposeService string `json:"composeService"`
			Resource       string `json:"resource"`
		} `json:"mapping"`
	}
	_ = json.Unmarshal([]byte(x.doc.Internal.HopaiResult), &res)
	for _, m := range res.Mapping {
		if x.facts != nil && x.facts.DB != nil && m.ComposeService == x.facts.DB.Service && m.Resource != "" {
			return m.Resource
		}
	}
	return "db"
}

// legacyRemote is the rclone remote legacy of the source bucket, only for
// this onboarding's processes.
func (r *Runner) legacyRemote(x *rec) ([]string, error) {
	s := x.facts.Storage
	values, err := r.sourceValues(x)
	if err != nil {
		return nil, err
	}
	v := values[s.Container]
	provider := "AWS"
	if s.Provider == facts.StorageGCS {
		provider = "GCS"
	} else if s.Provider == facts.StorageOther {
		provider = "Other"
	}
	env := []string{"RCLONE_CONFIG_LEGACY_TYPE=s3", "RCLONE_CONFIG_LEGACY_PROVIDER=" + provider,
		"RCLONE_CONFIG_LEGACY_ACCESS_KEY_ID=" + v[s.AccessKeyEnv], "RCLONE_CONFIG_LEGACY_SECRET_ACCESS_KEY=" + v[s.SecretKeyEnv]}
	if s.Endpoint != "" {
		env = append(env, "RCLONE_CONFIG_LEGACY_ENDPOINT="+s.Endpoint)
	}
	if s.Region != "" {
		env = append(env, "RCLONE_CONFIG_LEGACY_REGION="+s.Region)
	}
	return env, nil
}

// --- decommissioning ---

func (r *Runner) decommissioning(ctx context.Context, x *rec) (bool, error) {
	if r.cfg.Gateway == nil || r.cfg.Connect == nil || r.cfg.Routes == nil {
		return false, fmt.Errorf("%w: gateway, connect or routes", ErrNotConfigured)
	}
	if !x.doc.Internal.LegacyRemoved {
		list, err := r.cfg.Gateway.Apps(ctx)
		if err != nil {
			return false, err
		}
		for _, a := range list.Apps {
			if a.Name != x.row.App {
				continue
			}
			if a.WriteBackend == onboard.SourceName {
				return false, errors.New("the gateway still writes to legacy after the cutover")
			}
			spec := gateway.AppSpec{HealthPath: a.HealthPath, Backends: map[string]string{}, WriteBackend: a.WriteBackend, ReadWeights: map[string]int{}}
			for k, v := range a.Backends {
				if k != onboard.SourceName {
					spec.Backends[k] = v
				}
			}
			for k, v := range a.ReadWeights {
				if k != onboard.SourceName && v > 0 {
					spec.ReadWeights[k] = v
				}
			}
			for k, v := range a.Dial {
				if k != onboard.SourceName {
					if spec.Dial == nil {
						spec.Dial = map[string]string{}
					}
					spec.Dial[k] = v
				}
			}
			if err := r.cfg.Gateway.PutApp(ctx, x.row.App, spec); err != nil {
				return false, err
			}
		}
		if _, err := r.update(ctx, x.row.ID, nil, func(x *rec) (string, error) {
			x.doc.Internal.LegacyRemoved = true
			return "", nil
		}); err != nil {
			return false, err
		}
	}
	if !x.doc.Internal.DeviceRemoved {
		var ts []apiv1.OnboardingLegacyChange
		for _, c := range x.doc.LegacyChanges {
			switch c.Kind {
			case apiv1.OnboardingLegacyChangeKindTailscaleDeviceJoined, apiv1.OnboardingLegacyChangeKindTailscaleContainerStarted, apiv1.OnboardingLegacyChangeKindTailscaleVolumeCreated:
				if c.UndoneAt == nil {
					ts = append(ts, c)
				}
			}
		}
		undone, err := r.cfg.Connect.Disconnect(ctx, r.connectReq(x), ts)
		r.markUndone(ctx, x.row.ID, undone)
		if err != nil {
			return false, fmt.Errorf("remove the Tailscale device: %w", err)
		}
		if err := r.cfg.Routes.RemoveLegacy(ctx, x.row.App); err != nil {
			return false, err
		}
		if _, err := r.update(ctx, x.row.ID, nil, func(x *rec) (string, error) {
			x.doc.Internal.DeviceRemoved = true
			now := r.cfg.Now()
			for i, c := range x.doc.LegacyChanges {
				if c.Kind == apiv1.OnboardingLegacyChangeKindDbRouteCreated && c.UndoneAt == nil {
					x.doc.LegacyChanges[i].UndoneAt = &now
				}
			}
			return "", nil
		}); err != nil {
			return false, err
		}
		r.event(ctx, x, apiv1.OnboardingEventKindHuman, "the source is no longer routed; confirm stopping the instance (decommission)", nil)
	}
	return false, nil
}

// markUndone copies UndoneAt of undone onto the row's changes.
func (r *Runner) markUndone(ctx context.Context, id string, undone []apiv1.OnboardingLegacyChange) {
	if len(undone) == 0 {
		return
	}
	_, _ = r.update(ctx, id, nil, func(x *rec) (string, error) {
		for _, u := range undone {
			if u.UndoneAt == nil {
				continue
			}
			for i, c := range x.doc.LegacyChanges {
				if c.Kind == u.Kind && c.Target == u.Target && c.UndoneAt == nil {
					x.doc.LegacyChanges[i].UndoneAt = u.UndoneAt
				}
			}
		}
		return "", nil
	})
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
