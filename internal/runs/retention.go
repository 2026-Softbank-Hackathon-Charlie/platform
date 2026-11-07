package runs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/plans"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/retention"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/appspec"
)

func (r *Runner) retirementJob(ctx context.Context, id string) (*retention.Job, error) {
	events, err := r.cfg.Store.ListEvents(ctx, id, 0, 0)
	if err != nil {
		return nil, err
	}
	var last *retention.Job
	for _, e := range events {
		if e.Kind != retention.Event {
			continue
		}
		var j retention.Job
		if json.Unmarshal(e.Data, &j) != nil || j.RunID != id || j.SourceUID == "" || j.KeepFor <= 0 {
			return nil, errors.New("invalid source retirement record")
		}
		switch j.Status {
		case "captured", "scheduled", "blocked", "deleting", "deleted", "cancelled":
		default:
			return nil, errors.New("invalid source retirement status")
		}
		last = &j
	}
	return last, nil
}

func (r *Runner) saveRetirement(ctx context.Context, j retention.Job) error {
	b, err := json.Marshal(j)
	if err != nil {
		return err
	}
	previous, err := r.retirementJob(ctx, j.RunID)
	if err != nil {
		return err
	}
	if previous != nil {
		old, _ := json.Marshal(previous)
		if string(old) == string(b) {
			return nil
		}
	}
	// Persist an audit intention before the state event and before any delete.
	// User preservation requests additionally keep the API actor's audit entry.
	if _, err := r.cfg.Store.AppendAudit(ctx, store.Audit{Time: r.now(), Actor: "source-retention-controller", Role: "operator", Action: "source_retention_state", Target: "/v1/migrations/" + j.RunID + "/retention", Status: 200, Detail: b}); err != nil {
		return err
	}
	_, err = r.cfg.Store.AppendEvent(ctx, store.Event{RunID: j.RunID, Time: r.now(), Kind: retention.Event, Message: j.Status, Data: b})
	return err
}

// A third member can still stream through the old source after cutover.
// Until deletion reconciles every member's replication path, retain the source
// even when a manually submitted plan omitted AllowSourceRewire. Store errors
// are unknown membership, never permission to delete. File apps with no
// registered row and the existing two-member path remain unchanged.
func (r *Runner) retirementDatabaseGroupBlock(ctx context.Context, p *plan.Plan) string {
	if p == nil {
		return "database_group_identity_unavailable"
	}
	if p.AllowSourceRewire {
		return "three_environment_group_requires_reconciliation"
	}
	a, err := r.cfg.Store.GetApp(ctx, p.Source.Namespace)
	if errors.Is(err, store.ErrNotFound) {
		return ""
	}
	if err != nil {
		return "database_group_membership_unavailable"
	}
	if len(a.Environments) < 3 {
		return ""
	}
	db, err := r.cfg.Store.GetAppDatabase(ctx, a.Name)
	if err != nil {
		return "database_group_identity_unavailable"
	}
	if a.Name != p.Source.Namespace || db.App != a.Name {
		return "database_group_identity_unavailable"
	}
	var spec appspec.Spec
	if json.Unmarshal(a.Spec, &spec) != nil {
		return "database_group_spec_invalid"
	}
	matched, hasPostgres := false, false
	for _, res := range spec.Resources {
		if res.Type == appspec.TypePostgres {
			hasPostgres = true
			matched = matched || (res.Name != "" && p.Source.Cluster == a.Name+"-"+res.Name)
		}
	}
	if !hasPostgres {
		return "database_group_spec_invalid"
	}
	// A static app may share a namespace with another registered app; only
	// match the registered default instance's actual database resource.
	if !matched {
		return ""
	}
	if !slices.Contains(a.Environments, p.Source.Env) || !slices.Contains(a.Environments, db.PrimaryEnv) || !slices.Contains(a.Environments, db.PeerEnv) || p.Target.Env == p.Source.Env || !slices.Contains(a.Environments, p.Target.Env) || p.Target.Namespace != a.Name || p.Target.Cluster != p.Source.Cluster {
		return "database_group_endpoint_mismatch"
	}
	return "three_environment_group_requires_reconciliation"
}

func (r *Runner) blockRetirement(ctx context.Context, j *retention.Job, reason string) error {
	if j.Status != "deleting" {
		j.Status = "blocked"
	}
	// Keep an interrupted deletion quarantined; changing it back to blocked
	// would permit a new migration to reuse an object already being removed.
	j.Reason = reason
	return r.saveRetirement(ctx, *j)
}

// retirementGuard runs while r.mu is held, before any engine action. It shares
// the runner's cluster exclusion with the worker. A new run supersedes a pending
// retirement; it must never re-use a source whose deletion was requested.
func (r *Runner) retirementGuard(ctx context.Context, p *plan.Plan, runID string) error {
	if plans.NoDatabase(p) {
		return nil // no cluster that a retirement could be deleting (ADR 0049)
	}
	keys := clusterKeys(p)
	for _, active := range r.retiring {
		if overlaps(active, keys) {
			return ErrBusy
		}
	}
	rows, err := r.cfg.Store.ListRuns(ctx)
	if err != nil {
		return err
	}
	for _, row := range rows {
		if row.RunID == runID {
			continue
		}
		j, err := r.retirementJob(ctx, row.RunID)
		if err != nil {
			return err
		}
		if j == nil || j.Status == "cancelled" {
			continue
		}
		var previous apiv1.Run
		if json.Unmarshal(row.Doc, &previous) != nil {
			return errors.New("invalid prior run")
		}
		// Public endpoint identity does not depend on a kubeconfig's current context.
		uses := func(a, b apiv1.PlanEndpoint) bool {
			return a.Env == b.Env && a.Namespace == b.Namespace && a.Cluster == b.Cluster
		}
		if uses(previous.Source, apiv1.PlanEndpoint{Env: p.Source.Env, Namespace: p.Source.Namespace, Cluster: p.Source.Cluster}) && (j.Status == "deleting" || (j.Status == "deleted" && !restoredAfter(rows, previous))) {
			return fmt.Errorf("%w: source retirement has started; restore the source before another migration", ErrWrongStatus)
		}
		if j.Status == "deleting" { // Do not race incomplete pod cleanup, in either direction.
			if uses(previous.Source, apiv1.PlanEndpoint{Env: p.Target.Env, Namespace: p.Target.Namespace, Cluster: p.Target.Cluster}) {
				return ErrBusy
			}
		}
		if j.Status == "deleted" {
			continue
		}
		if uses(previous.Source, apiv1.PlanEndpoint{Env: p.Source.Env, Namespace: p.Source.Namespace, Cluster: p.Source.Cluster}) || uses(previous.Source, apiv1.PlanEndpoint{Env: p.Target.Env, Namespace: p.Target.Namespace, Cluster: p.Target.Cluster}) || uses(previous.Target, apiv1.PlanEndpoint{Env: p.Source.Env, Namespace: p.Source.Namespace, Cluster: p.Source.Cluster}) || uses(previous.Target, apiv1.PlanEndpoint{Env: p.Target.Env, Namespace: p.Target.Namespace, Cluster: p.Target.Cluster}) {
			j.Status, j.Reason = "cancelled", "superseded by another migration"
			if err := r.saveRetirement(ctx, *j); err != nil {
				return err
			}
		}
	}
	return nil
}

// RetireSources is called periodically by the API. Events are the durable
// journal, so restart does not reset the deadline or enroll historical runs.
func (r *Runner) RetireSources(ctx context.Context) error {
	if r.cfg.Retention == nil {
		return nil
	}
	if r.cfg.RetentionPlan == nil {
		return errors.New("source retention plan loader unavailable")
	}
	rows, err := r.cfg.Store.ListRuns(ctx)
	if err != nil {
		return err
	}
	for _, row := range rows {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := r.retireSource(ctx, row.RunID); err != nil {
			return err
		}
	}
	return nil
}

func (r *Runner) retireSource(ctx context.Context, id string) error {
	r.mu.Lock()
	locked := true
	defer func() {
		if locked {
			r.mu.Unlock()
		}
	}()
	j, err := r.retirementJob(ctx, id)
	if err != nil {
		return err
	}
	if j == nil || j.Status == "deleted" || j.Status == "cancelled" {
		return nil
	}
	doc, err := r.stored(ctx, id)
	if err != nil {
		return err
	}
	if _, active := r.live[id]; active {
		return nil
	}
	p, err := r.cfg.RetentionPlan(ctx, doc.PlanRef)
	if err != nil {
		return err
	}
	keys := clusterKeys(p)
	for _, live := range r.live {
		if overlaps(live.clusters, keys) {
			return nil
		}
	}
	for _, active := range r.retiring {
		if overlaps(active, keys) {
			return nil
		}
	}
	if r.cfg.ClusterLease == nil {
		return errors.New("source retention requires cluster leases")
	}
	lease, err := r.cfg.ClusterLease(ctx, keys)
	if errors.Is(err, store.ErrConflict) {
		return nil
	}
	if err != nil {
		return err
	}
	defer lease.Release()
	// All journal mutations, including initial scheduling, are inside the lease.
	j, err = r.retirementJob(ctx, id)
	if err != nil {
		return err
	}
	if j == nil || j.Status == "deleted" || j.Status == "cancelled" {
		return nil
	}
	doc, err = r.stored(ctx, id)
	if err != nil {
		return err
	}
	if reason := r.retirementDatabaseGroupBlock(ctx, p); reason != "" {
		return r.blockRetirement(ctx, j, reason)
	}
	if doc.Status != apiv1.RunStatusSucceeded || doc.EndedAt == nil || doc.Result.Verified == nil || !*doc.Result.Verified || (doc.Observe != nil && (doc.Observe.Passed == nil || !*doc.Observe.Passed)) {
		if j.Status != "deleting" {
			j.Status = "blocked"
		}
		j.Reason = "migration_verification_incomplete"
		return r.saveRetirement(ctx, *j)
	}
	if j.DueAt == nil {
		due := doc.EndedAt.Add(j.KeepFor)
		j.DueAt, j.Status, j.Reason = &due, "scheduled", ""
		if err := r.saveRetirement(ctx, *j); err != nil {
			return err
		}
	}
	if r.now().Before(*j.DueAt) {
		return nil
	}
	rows, err := r.cfg.Store.ListRuns(ctx)
	if err != nil {
		return err
	}
	for _, row := range rows {
		if row.RunID == id {
			continue
		}
		var other apiv1.Run
		if json.Unmarshal(row.Doc, &other) != nil {
			return errors.New("invalid run record")
		}
		if other.StartedAt.Before(doc.StartedAt) {
			continue
		}
		if sameEndpoint(doc.Source, other.Source) || sameEndpoint(doc.Source, other.Target) || sameEndpoint(doc.Target, other.Source) || sameEndpoint(doc.Target, other.Target) {
			if j.Status == "deleting" {
				return errors.New("new migration overlaps an interrupted retirement; operator reconciliation required")
			}
			j.Status, j.Reason = "cancelled", "superseded_by_migration"
			return r.saveRetirement(ctx, *j)
		}
	}
	plans, err := r.cfg.Store.ListPlans(ctx)
	if err != nil {
		return err
	}
	for _, p := range plans {
		if p.RollbackOf == id && p.Status != store.PlanRejected {
			if j.Status != "deleting" {
				j.Status = "blocked"
			}
			j.Reason = "rollback_plan_exists"
			return r.saveRetirement(ctx, *j)
		}
	}
	r.retiring[id] = keys
	r.mu.Unlock()
	locked = false
	defer func() { r.mu.Lock(); delete(r.retiring, id); r.mu.Unlock() }()
	if j.Status != "deleting" {
		captured, err := r.cfg.Retention.Capture(ctx, p)
		if err != nil || captured.SourceUID != j.SourceUID || captured.TargetUID != j.TargetUID || j.TargetUID == "" {
			j.Status, j.Reason = "blocked", "cluster_identity_changed"
			return r.saveRetirement(ctx, *j)
		}
		j.Pods = captured.Pods
	}
	if err := r.cfg.Retention.Check(ctx, p, *j, *doc.EndedAt); err != nil {
		if j.Status != "deleting" {
			j.Status = "blocked"
		}
		j.Reason = "safety_check_failed"
		return r.saveRetirement(ctx, *j)
	}
	if j.Status != "deleting" {
		j.Status, j.Reason = "deleting", ""
		if err := r.saveRetirement(ctx, *j); err != nil {
			return err
		}
	}
	if err := lease.Check(ctx); err != nil {
		return err
	}
	// Re-read membership after the live backend checks and immediately before
	// removal, including when resuming a persisted deleting job.
	if reason := r.retirementDatabaseGroupBlock(ctx, p); reason != "" {
		return r.blockRetirement(ctx, j, reason)
	}
	done, err := r.cfg.Retention.Remove(ctx, p, *j)
	if err != nil {
		j.Reason = "deletion_unconfirmed"
	} else if done {
		j.Status, j.Reason = "deleted", "PVCs and non-database resources retained"
	}
	return r.saveRetirement(ctx, *j)
}

func sameEndpoint(a, b apiv1.PlanEndpoint) bool {
	return a.Env == b.Env && a.Namespace == b.Namespace && a.Cluster == b.Cluster
}

// A later successful migration may deliberately recreate the retired endpoint
// as a target. Its next migration must not be blocked by an older incarnation.
func restoredAfter(rows []store.Run, retired apiv1.Run) bool {
	if retired.EndedAt == nil {
		return false
	}
	for _, row := range rows {
		var newer apiv1.Run
		if json.Unmarshal(row.Doc, &newer) != nil {
			continue
		}
		if newer.Status == apiv1.RunStatusSucceeded && newer.EndedAt != nil && newer.StartedAt.After(*retired.EndedAt) && sameEndpoint(newer.Target, retired.Source) {
			return true
		}
	}
	return false
}

// SourceRetention is read-only and works even when automatic cleanup is off.
func (r *Runner) SourceRetention(ctx context.Context, id string) (*retention.Job, error) {
	if _, err := r.stored(ctx, id); err != nil {
		return nil, err
	}
	return r.retirementJob(ctx, id)
}

// PreserveSource only delays or cancels deletion; it never shortens retention
// and cannot reverse a persisted deletion request.
func (r *Runner) PreserveSource(ctx context.Context, id string, until *time.Time, keep bool) (*retention.Job, error) {
	if (until == nil) == !keep {
		return nil, fmt.Errorf("%w: choose retainUntil or keep", ErrInvalid)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, active := r.retiring[id]; active {
		return nil, ErrBusy
	}
	doc, err := r.stored(ctx, id)
	if err != nil {
		return nil, err
	}
	if r.cfg.RetentionPlan == nil || r.cfg.ClusterLease == nil {
		return nil, fmt.Errorf("%w: retention controls unavailable", ErrWrongStatus)
	}
	p, err := r.cfg.RetentionPlan(ctx, doc.PlanRef)
	if err != nil {
		return nil, err
	}
	lease, err := r.cfg.ClusterLease(ctx, clusterKeys(p))
	if errors.Is(err, store.ErrConflict) {
		return nil, ErrBusy
	}
	if err != nil {
		return nil, err
	}
	defer lease.Release()
	j, err := r.retirementJob(ctx, id)
	if err != nil {
		return nil, err
	}
	if j == nil || j.Status == "deleting" || j.Status == "deleted" || j.Status == "cancelled" {
		return nil, fmt.Errorf("%w: source retention cannot be changed", ErrWrongStatus)
	}
	if keep {
		j.Status, j.Reason = "cancelled", "operator_keep"
	} else {
		base := r.now()
		if doc.EndedAt != nil {
			base = doc.EndedAt.Add(j.KeepFor)
		}
		if j.DueAt != nil {
			base = *j.DueAt
		}
		if !until.After(base) || !until.After(r.now()) || until.After(r.now().Add(30*24*time.Hour)) {
			return nil, fmt.Errorf("%w: retainUntil must extend the deadline, within 30 days", ErrInvalid)
		}
		j.DueAt = until
		j.Status, j.Reason = "scheduled", "operator_extended"
	}
	if err := r.saveRetirement(ctx, *j); err != nil {
		return nil, err
	}
	return j, nil
}

// RunRetention polls until shutdown. A bounded attempt cannot block server drain.
func (r *Runner) RunRetention(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		attempt, cancel := context.WithTimeout(ctx, 45*time.Second)
		if err := r.RetireSources(attempt); err != nil && ctx.Err() == nil {
			r.cfg.Log.Warn("source retention poll failed")
		}
		cancel()
		select {
		case <-ctx.Done():
			return
		case <-r.stopCh:
			return
		case <-ticker.C:
		}
	}
}
