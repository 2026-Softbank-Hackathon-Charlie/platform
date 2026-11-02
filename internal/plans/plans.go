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
	"net"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

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
}

// New returns a Service enforcing policy.
func New(policy Policy, st store.Store) *Service {
	return &Service{policy: policy, store: st, now: func() time.Time { return time.Now().UTC() }}
}

// Parse applies the policy and parses raw plan JSON. Secrets are expanded from
// the server environment, so call it again at start time rather than caching.
func (s *Service) Parse(raw []byte) (*plan.Plan, error) {
	if err := s.policy.checkRaw(raw); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	p, err := plan.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if err := s.policy.checkParsed(p); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return p, nil
}

// Submit validates and stores a plan awaiting approval.
func (s *Service) Submit(ctx context.Context, raw []byte, actor string) (apiv1.PlanRecord, error) {
	p, err := s.Parse(raw)
	if err != nil {
		return apiv1.PlanRecord{}, err
	}
	sum, err := json.Marshal(Summarize(p))
	if err != nil {
		return apiv1.PlanRecord{}, err
	}
	row := store.Plan{
		Ref:         newRef("pln_"),
		PlanID:      p.ID,
		Status:      store.PlanPending,
		Summary:     sum,
		Raw:         append([]byte(nil), raw...),
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

// Approve records human approval. Approving an approved plan is a no-op.
func (s *Service) Approve(ctx context.Context, ref, actor, comment string) (apiv1.PlanRecord, error) {
	if err := s.requireAttachments(ctx, ref); err != nil {
		return apiv1.PlanRecord{}, err
	}
	now := s.now()
	row, err := s.transition(ctx, ref, []string{store.PlanPending}, store.PlanApproved,
		store.PlanUpdate{ReviewedBy: actor, ReviewedAt: &now, ReviewComment: comment})
	if errors.Is(err, ErrWrongStatus) {
		if cur, gerr := s.get(ctx, ref); gerr == nil && cur.Status == store.PlanApproved {
			return s.recordOf(ctx, cur)
		}
	}
	if err != nil {
		return apiv1.PlanRecord{}, err
	}
	return s.recordOf(ctx, row)
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

// Reserve moves an approved plan to starting and returns the parsed plan.
// Exactly one caller wins; it must call Started or Release afterwards.
func (s *Service) Reserve(ctx context.Context, ref string) (*plan.Plan, error) {
	row, err := s.transition(ctx, ref, []string{store.PlanApproved}, store.PlanStarting, store.PlanUpdate{})
	if err != nil {
		return nil, err
	}
	p, err := s.Parse(row.Raw) // re-parse: secrets come from the environment at start time
	if err == nil {
		err = s.materialize(ctx, ref, p)
	}
	if err != nil {
		s.Release(ctx, ref)
		return nil, err
	}
	return p, nil
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

// Summarize is the non-secret view of a plan.
func Summarize(p *plan.Plan) apiv1.PlanSummary {
	return apiv1.PlanSummary{
		PlanId:      p.ID,
		Source:      Endpoint(p.Source),
		Target:      Endpoint(p.Target),
		Files:       p.Files != nil,
		GatewayMode: p.Gateway.Mode,
		FenceGate:   *p.Verify.FenceGate,
		MaxDowntime: p.Thresholds.MaxDowntime.String(),
	}
}

// Endpoint drops the DSN and manifest from a plan endpoint. It keeps the
// DSN's host:port so an approver can see where the password would be sent.
func Endpoint(e plan.Endpoint) apiv1.PlanEndpoint {
	ep := apiv1.PlanEndpoint{Env: e.Env, Namespace: e.Namespace, Cluster: e.Cluster}
	if e.Context != "" {
		ep.Context = &e.Context
	}
	if h := dsnHost(e.DSN); h != "" {
		ep.DbHost = &h
	}
	return ep
}

// dsnHost returns host:port of a DSN, or "" when it cannot be parsed. It never
// returns credentials.
func dsnHost(dsn string) string {
	cfg, err := pgconn.ParseConfig(dsn)
	if err != nil || cfg.Host == "" {
		return ""
	}
	return net.JoinHostPort(cfg.Host, strconv.Itoa(int(cfg.Port)))
}

func record(row store.Plan) (apiv1.PlanRecord, error) {
	rec := apiv1.PlanRecord{
		Ref:         row.Ref,
		Status:      apiv1.PlanStatus(row.Status),
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
	return rec, nil
}

func newRef(prefix string) string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return prefix + hex.EncodeToString(b)
}
