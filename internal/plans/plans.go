// Package plans holds submitted migration plans and their human review.
//
// Lifecycle: pending_approval → approved → starting → started, or
// pending_approval → rejected. A plan starts at most one run. Every transition
// is a conditional write in the store, so two operators (or two API replicas)
// cannot start the same plan twice.
package plans

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

var (
	ErrNotFound    = errors.New("plan not found")
	ErrInvalid     = errors.New("invalid plan")
	ErrWrongStatus = errors.New("plan is not in the required status")
)

// Service validates plans and records their review.
type Service struct {
	policy Policy
	store  store.Store
	now    func() time.Time

	stateDir string // attachments are written below it before a run (attachments.go)

	residency ResidencyGate // nil: no residency policy (residency.go)
	capacity  CapacityGate  // nil: no capacity measurement (capacity.go)
}

// New returns a Service enforcing policy.
func New(policy Policy, st store.Store) *Service {
	return &Service{policy: policy, store: st, now: func() time.Time { return time.Now().UTC() }}
}

// Parse applies the policy and parses raw plan JSON. Secrets are expanded from
// the server environment, so call it again at start time rather than caching.
func (s *Service) Parse(raw []byte) (*plan.Plan, error) {
	p, _, err := s.parse(raw)
	return p, err
}

// parse also returns the plan's summary, built from the plan as submitted:
// nothing expanded from the environment ever reaches it or an error.
func (s *Service) parse(raw []byte) (*plan.Plan, apiv1.PlanSummary, error) {
	hosts, err := s.policy.checkRefs(raw)
	if err != nil {
		return nil, apiv1.PlanSummary{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	// The engine's errors name fields, never expanded values.
	p, err := plan.Parse(raw)
	if err != nil {
		return nil, apiv1.PlanSummary{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if err := s.policy.checkParsed(p); err != nil {
		return nil, apiv1.PlanSummary{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return p, summarize(p, hosts), nil
}

// Submit validates and stores a plan awaiting approval.
func (s *Service) Submit(ctx context.Context, raw []byte, actor string) (apiv1.PlanRecord, error) {
	p, summary, err := s.parse(raw)
	if err != nil {
		return apiv1.PlanRecord{}, err
	}
	sum, err := json.Marshal(summary)
	if err != nil {
		return apiv1.PlanRecord{}, err
	}
	row := store.Plan{
		Ref:         newRef("pln_"),
		PlanID:      p.ID,
		Status:      store.PlanPending,
		Summary:     sum,
		Raw:         append([]byte(nil), raw...),
		Revision:    1,
		SubmittedBy: actor,
		SubmittedAt: s.now(),
	}
	if err := s.store.CreatePlan(ctx, row); err != nil {
		return apiv1.PlanRecord{}, err
	}
	return s.recordOf(ctx, row)
}

// Get returns a plan by ref.
func (s *Service) Get(ctx context.Context, ref string) (apiv1.PlanRecord, error) {
	row, err := s.get(ctx, ref)
	if err != nil {
		return apiv1.PlanRecord{}, err
	}
	return s.recordOf(ctx, row)
}

// List returns all plans, newest first.
func (s *Service) List(ctx context.Context) ([]apiv1.PlanRecord, error) {
	rows, err := s.store.ListPlans(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]apiv1.PlanRecord, 0, len(rows))
	for _, row := range rows {
		rec, err := record(row)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	if err := s.withAttachments(ctx, out); err != nil {
		return nil, err
	}
	return out, nil
}

// Approve records human approval of the plan at revision, the
// PlanRecord.revision the approver reviewed. A plan with attachments needs
// it, so content uploaded after the review is never approved; for other
// plans it is checked when given. Approving an approved plan again is a no-op.
func (s *Service) Approve(ctx context.Context, ref, actor, comment string, revision *int64) (apiv1.PlanRecord, error) {
	row, err := s.get(ctx, ref)
	if err != nil {
		return apiv1.PlanRecord{}, err
	}
	attached, err := s.store.ListPlanAttachments(ctx, []string{ref})
	if err != nil {
		return apiv1.PlanRecord{}, err
	}
	if err := requireAttachments(row, attached); err != nil {
		return apiv1.PlanRecord{}, err
	}
	want := row.Revision
	switch {
	case revision != nil:
		want = *revision
	case len(attached) > 0:
		return apiv1.PlanRecord{}, fmt.Errorf("%w: plan %s has attachments, so approving it needs the revision you reviewed (now %d): send \"revision\", or use charlie plan approve",
			ErrWrongStatus, ref, row.Revision)
	}
	now := s.now()
	// IfRevision makes the check and the approval one write: an upload that
	// lands in between changes the revision and the approval fails.
	next, err := s.transition(ctx, ref, []string{store.PlanPending}, store.PlanApproved,
		store.PlanUpdate{ReviewedBy: actor, ReviewedAt: &now, ReviewComment: comment, IfRevision: want})
	if errors.Is(err, ErrWrongStatus) {
		if cur, gerr := s.get(ctx, ref); gerr == nil && (cur.Status == store.PlanPending || cur.Status == store.PlanApproved) {
			if cur.Revision != want {
				return apiv1.PlanRecord{}, fmt.Errorf("%w: plan %s changed since you reviewed it (revision %d, now %d); review it again",
					ErrWrongStatus, ref, want, cur.Revision)
			}
			if cur.Status == store.PlanApproved {
				return s.recordOf(ctx, cur)
			}
		}
	}
	if err != nil {
		return apiv1.PlanRecord{}, err
	}
	return s.recordOf(ctx, next)
}

// Reject records that a human refused the plan. A rejected plan never starts.
func (s *Service) Reject(ctx context.Context, ref, actor, comment string) (apiv1.PlanRecord, error) {
	now := s.now()
	row, err := s.transition(ctx, ref, []string{store.PlanPending}, store.PlanRejected,
		store.PlanUpdate{ReviewedBy: actor, ReviewedAt: &now, ReviewComment: comment})
	if err != nil {
		return apiv1.PlanRecord{}, err
	}
	return s.recordOf(ctx, row)
}

// Reserved is an approved plan that one caller may now start.
type Reserved struct {
	// Plan has secrets expanded: it goes to the engine and nowhere else.
	Plan *plan.Plan
	// Summary is built from the plan as submitted, for the run record.
	Summary apiv1.PlanSummary
	// RollbackOf is the run this plan rolls back, if any.
	RollbackOf string
	// Residency is what the residency gate decided.
	Residency ResidencyCheck
	// Capacity is what the capacity gate measured and decided.
	Capacity CapacityCheck
}

// Reserve moves an approved plan to starting and parses it. With a residency
// gate it then checks the policy against the source's live inventory, before
// the engine can create a replica; a denial (*ResidencyDenied) returns the
// plan to approved. Exactly one caller wins; it must call Started or Release
// afterwards.
func (s *Service) Reserve(ctx context.Context, ref string) (Reserved, error) {
	row, err := s.transition(ctx, ref, []string{store.PlanApproved}, store.PlanStarting, store.PlanUpdate{})
	if err != nil {
		return Reserved{}, err
	}
	var check ResidencyCheck
	var capCheck CapacityCheck
	p, sum, err := s.parse(row.Raw) // re-parse: secrets come from the environment at start time
	if err == nil {
		check, err = s.checkResidency(ctx, row.RollbackOf, p)
	}
	if err == nil {
		err = s.materialize(ctx, ref, p)
	}
	if err == nil { // after materialize: a new target's manifest is a local file now
		capCheck, err = s.checkCapacity(ctx, row.RollbackOf, p)
	}
	if err != nil {
		s.Release(ctx, ref)
		return Reserved{}, err
	}
	return Reserved{Plan: p, Summary: sum, RollbackOf: row.RollbackOf, Residency: check, Capacity: capCheck}, nil
}

// Load re-parses a started plan, for resuming its run.
func (s *Service) Load(ctx context.Context, ref string) (*plan.Plan, error) {
	row, err := s.get(ctx, ref)
	if err != nil {
		return nil, err
	}
	p, err := s.Parse(row.Raw)
	if err != nil {
		return nil, err
	}
	if err := s.materialize(ctx, ref, p); err != nil {
		return nil, err
	}
	return p, nil
}

// Started links a reserved plan to its run.
func (s *Service) Started(ctx context.Context, ref, runID string) error {
	_, err := s.transition(ctx, ref, []string{store.PlanStarting}, store.PlanStarted, store.PlanUpdate{RunID: runID})
	return err
}

// Release returns a reserved plan to approved (the run could not be created).
func (s *Service) Release(ctx context.Context, ref string) {
	_, _ = s.transition(ctx, ref, []string{store.PlanStarting}, store.PlanApproved, store.PlanUpdate{})
}

// RecoverStarting repairs plans left in starting by a crash between Reserve
// and Started: a plan whose run exists is linked to it (so it can never start
// a second run), any other goes back to approved. Call once at startup,
// before serving requests.
func (s *Service) RecoverStarting(ctx context.Context) (int, error) {
	rows, err := s.store.ListPlans(ctx)
	if err != nil {
		return 0, err
	}
	runs, err := s.store.ListRuns(ctx)
	if err != nil {
		return 0, err
	}
	runOf := map[string]string{}
	for _, r := range runs {
		if _, seen := runOf[r.PlanRef]; !seen { // newest first
			runOf[r.PlanRef] = r.RunID
		}
	}
	n := 0
	for _, row := range rows {
		if row.Status != store.PlanStarting {
			continue
		}
		if runID, ok := runOf[row.Ref]; ok {
			if err := s.Started(ctx, row.Ref, runID); err != nil {
				return n, err
			}
		} else {
			s.Release(ctx, row.Ref)
		}
		n++
	}
	return n, nil
}

func (s *Service) get(ctx context.Context, ref string) (store.Plan, error) {
	row, err := s.store.GetPlan(ctx, ref)
	if errors.Is(err, store.ErrNotFound) {
		return store.Plan{}, ErrNotFound
	}
	return row, err
}

func (s *Service) transition(ctx context.Context, ref string, from []string, to string, u store.PlanUpdate) (store.Plan, error) {
	row, err := s.store.TransitionPlan(ctx, ref, from, to, u)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return store.Plan{}, ErrNotFound
	case errors.Is(err, store.ErrConflict):
		return store.Plan{}, fmt.Errorf("%w: %v", ErrWrongStatus, err)
	}
	return row, err
}

// summarize is the non-secret view of a plan. hosts come from the DSNs as
// submitted (Policy.checkRefs), so an approver sees where each password would
// be sent and nothing expanded from the environment.
func summarize(p *plan.Plan, hosts dbHosts) apiv1.PlanSummary {
	return apiv1.PlanSummary{
		PlanId:      p.ID,
		Source:      endpoint(p.Source, hosts.source),
		Target:      endpoint(p.Target, hosts.target),
		Files:       p.Files != nil,
		GatewayMode: p.Gateway.Mode,
		FenceGate:   *p.Verify.FenceGate,
		MaxDowntime: p.Thresholds.MaxDowntime.String(),
	}
}

// endpoint drops the DSN and manifest from a plan endpoint.
func endpoint(e plan.Endpoint, dbHost string) apiv1.PlanEndpoint {
	ep := apiv1.PlanEndpoint{Env: e.Env, Namespace: e.Namespace, Cluster: e.Cluster}
	if e.Context != "" {
		ep.Context = &e.Context
	}
	if dbHost != "" {
		ep.DbHost = &dbHost
	}
	return ep
}

func record(row store.Plan) (apiv1.PlanRecord, error) {
	rec := apiv1.PlanRecord{
		Ref:         row.Ref,
		Status:      apiv1.PlanStatus(row.Status),
		Revision:    row.Revision,
		SubmittedBy: row.SubmittedBy,
		SubmittedAt: row.SubmittedAt,
		ReviewedAt:  row.ReviewedAt,
	}
	if err := json.Unmarshal(row.Summary, &rec.Summary); err != nil {
		return apiv1.PlanRecord{}, fmt.Errorf("decode plan %s summary: %w", row.Ref, err)
	}
	if row.ReviewedBy != "" {
		rec.ReviewedBy = &row.ReviewedBy
	}
	if row.ReviewComment != "" {
		rec.ReviewComment = &row.ReviewComment
	}
	if row.RunID != "" {
		rec.RunId = &row.RunID
	}
	if row.RollbackOf != "" {
		rec.RollbackOf = &row.RollbackOf
	}
	return rec, nil
}

func newRef(prefix string) string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return prefix + hex.EncodeToString(b)
}
