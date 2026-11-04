// Package storetest holds an in-memory store.Store and a conformance suite
// that every store implementation must pass.
//
// It is for tests only. Production and demo code paths use store.Open; the
// memory store exists so packages built on store.Store can be tested without
// PostgreSQL, and the shared suite keeps its behavior identical to Postgres.
package storetest

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
)

type idemEntry struct{ planRef, runID string }

type memory struct {
	mu      sync.Mutex
	plans   map[string]store.Plan
	runs    map[string]store.Run
	events  []store.Event // in Seq order
	audit   []store.Audit // in ID order
	idem    map[string]idemEntry
	seq     int64
	auditID int64

	tools   memoryPlanTools // written records and plan attachments (memory_written.go)
	agent   agentMemory     // agent sessions and events (memory_agent.go)
	deploy  deployMemory    // apps, deployments, their events and webhook deliveries (memory_deploy.go)
	auth    authMemory      // sessions and device logins (memory_auth.go)
	secrets secretsMemory   // app secrets and instance storage (memory_secrets.go)
}

// NewMemory returns an empty in-memory store with the same semantics as the
// Postgres store, including microsecond time precision and UTC times.
func NewMemory() store.Store {
	return &memory{
		plans: map[string]store.Plan{},
		runs:  map[string]store.Run{},
		idem:  map[string]idemEntry{},
	}
}

// pgTime matches what a timestamptz round trip returns.
func pgTime(t time.Time) time.Time { return t.UTC().Truncate(time.Microsecond) }

func pgTimePtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := pgTime(*t)
	return &u
}

// checkJSON rejects what a jsonb column would reject. nil is SQL NULL.
func checkJSON(field string, b []byte) error {
	if b != nil && !json.Valid(b) {
		return fmt.Errorf("storetest: %s is not valid JSON", field)
	}
	return nil
}

// Copies keep callers from mutating stored rows through shared slices or pointers.
func copyPlan(p store.Plan) store.Plan {
	p.Summary, p.Raw = bytes.Clone(p.Summary), bytes.Clone(p.Raw)
	p.ReviewedAt = pgTimePtr(p.ReviewedAt)
	return p
}

func copyRun(r store.Run) store.Run {
	r.Doc = bytes.Clone(r.Doc)
	return r
}

func copyEvent(e store.Event) store.Event {
	e.Data = bytes.Clone(e.Data)
	return e
}

func copyAudit(a store.Audit) store.Audit {
	a.Detail = bytes.Clone(a.Detail)
	return a
}

func (m *memory) CreatePlan(_ context.Context, p store.Plan) error {
	if err := checkJSON("plan summary", p.Summary); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.plans[p.Ref]; ok {
		return fmt.Errorf("%w: plan %s exists", store.ErrConflict, p.Ref)
	}
	// Like the partial unique index plans_rollback_of_idx.
	if p.RollbackOf != "" && p.Status != store.PlanRejected {
		for _, q := range m.plans {
			if q.RollbackOf == p.RollbackOf && q.Status != store.PlanRejected {
				return fmt.Errorf("%w: a plan rolling back run %s exists", store.ErrConflict, p.RollbackOf)
			}
		}
	}
	p = copyPlan(p)
	p.SubmittedAt = pgTime(p.SubmittedAt)
	m.plans[p.Ref] = p
	return nil
}

func (m *memory) GetPlan(_ context.Context, ref string) (store.Plan, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.plans[ref]
	if !ok {
		return store.Plan{}, fmt.Errorf("%w: plan %s", store.ErrNotFound, ref)
	}
	return copyPlan(p), nil
}

func (m *memory) ListPlans(_ context.Context) ([]store.Plan, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]store.Plan, 0, len(m.plans))
	for _, p := range m.plans {
		out = append(out, copyPlan(p))
	}
	// Same order as Postgres: submitted_at DESC, ref DESC.
	slices.SortFunc(out, func(a, b store.Plan) int {
		return cmp.Or(b.SubmittedAt.Compare(a.SubmittedAt), cmp.Compare(b.Ref, a.Ref))
	})
	return out, nil
}

func (m *memory) TransitionPlan(_ context.Context, ref string, from []string, to string, u store.PlanUpdate) (store.Plan, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.plans[ref]
	if !ok {
		return store.Plan{}, fmt.Errorf("%w: plan %s", store.ErrNotFound, ref)
	}
	if !slices.Contains(from, p.Status) {
		return store.Plan{}, fmt.Errorf("%w: plan is %s", store.ErrConflict, p.Status)
	}
	if u.IfRevision != 0 && u.IfRevision != p.Revision {
		return store.Plan{}, fmt.Errorf("%w: plan revision is %d, not %d", store.ErrConflict, p.Revision, u.IfRevision)
	}
	p.Status = to
	if u.ReviewedBy != "" {
		p.ReviewedBy = u.ReviewedBy
	}
	if u.ReviewedAt != nil {
		p.ReviewedAt = pgTimePtr(u.ReviewedAt)
	}
	if u.ReviewComment != "" {
		p.ReviewComment = u.ReviewComment
	}
	if u.RunID != "" {
		p.RunID = u.RunID
	}
	m.plans[ref] = p
	return copyPlan(p), nil
}

func (m *memory) CreateRun(_ context.Context, r store.Run) error {
	if err := checkJSON("run doc", r.Doc); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.runs[r.RunID]; ok {
		return fmt.Errorf("%w: run %s exists", store.ErrConflict, r.RunID)
	}
	r = copyRun(r)
	r.CreatedAt, r.UpdatedAt = pgTime(r.CreatedAt), pgTime(r.UpdatedAt)
	m.runs[r.RunID] = r
	return nil
}

func (m *memory) UpdateRun(_ context.Context, r store.Run) error {
	if err := checkJSON("run doc", r.Doc); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.runs[r.RunID]
	if !ok {
		return fmt.Errorf("%w: run %s", store.ErrNotFound, r.RunID)
	}
	cur.PlanRef, cur.SourceEnv, cur.TargetEnv, cur.Status = r.PlanRef, r.SourceEnv, r.TargetEnv, r.Status
	cur.Doc, cur.UpdatedAt = bytes.Clone(r.Doc), pgTime(r.UpdatedAt)
	m.runs[r.RunID] = cur
	return nil
}

func (m *memory) GetRun(_ context.Context, runID string) (store.Run, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.runs[runID]
	if !ok {
		return store.Run{}, fmt.Errorf("%w: run %s", store.ErrNotFound, runID)
	}
	return copyRun(r), nil
}

func (m *memory) ListRuns(_ context.Context) ([]store.Run, error) {
	return m.listRuns(func(store.Run) bool { return true }), nil
}

func (m *memory) ListRunsByStatus(_ context.Context, statuses ...string) ([]store.Run, error) {
	return m.listRuns(func(r store.Run) bool { return slices.Contains(statuses, r.Status) }), nil
}

func (m *memory) listRuns(keep func(store.Run) bool) []store.Run {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]store.Run, 0, len(m.runs))
	for _, r := range m.runs {
		if keep(r) {
			out = append(out, copyRun(r))
		}
	}
	// Same order as Postgres: created_at DESC, run_id DESC.
	slices.SortFunc(out, func(a, b store.Run) int {
		return cmp.Or(b.CreatedAt.Compare(a.CreatedAt), cmp.Compare(b.RunID, a.RunID))
	})
	return out
}

func (m *memory) AppendEvent(_ context.Context, e store.Event) (store.Event, error) {
	if err := checkJSON("event data", e.Data); err != nil {
		return store.Event{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seq++
	e = copyEvent(e)
	e.Seq, e.Time = m.seq, pgTime(e.Time)
	m.events = append(m.events, e)
	return copyEvent(e), nil
}

func (m *memory) ListEvents(_ context.Context, runID string, afterSeq int64, limit int) ([]store.Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []store.Event{}
	for _, e := range m.events {
		if limit > 0 && len(out) == limit {
			break
		}
		if e.RunID == runID && e.Seq > afterSeq {
			out = append(out, copyEvent(e))
		}
	}
	return out, nil
}

func (m *memory) AppendAudit(_ context.Context, a store.Audit) (int64, error) {
	if err := checkJSON("audit detail", a.Detail); err != nil {
		return 0, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.auditID++
	a = copyAudit(a)
	a.ID, a.Time = m.auditID, pgTime(a.Time)
	m.audit = append(m.audit, a)
	return a.ID, nil
}

// ListAudit treats limit <= 0 as no limit, like the Postgres store.
func (m *memory) ListAudit(_ context.Context, before int64, limit int) ([]store.Audit, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []store.Audit{}
	for i := len(m.audit) - 1; i >= 0; i-- {
		if limit > 0 && len(out) == limit {
			break
		}
		if a := m.audit[i]; before <= 0 || a.ID < before {
			out = append(out, copyAudit(a))
		}
	}
	return out, nil
}

func (m *memory) PutIdempotencyKey(_ context.Context, key, planRef, runID string) (string, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.idem[key]; ok {
		return e.planRef, e.runID, fmt.Errorf("%w: idempotency key exists", store.ErrConflict)
	}
	m.idem[key] = idemEntry{planRef, runID}
	return planRef, runID, nil
}

func (m *memory) GetIdempotencyKey(_ context.Context, key string) (string, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.idem[key]
	if !ok {
		return "", "", fmt.Errorf("%w: idempotency key", store.ErrNotFound)
	}
	return e.planRef, e.runID, nil
}

func (m *memory) Close() {}
