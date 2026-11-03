// Package store persists the platform's records in PostgreSQL: plans, runs,
// run events, the audit log and idempotency keys.
//
// The migration engine keeps its own state file per run (that is what it
// resumes from). The store keeps the platform's view of each run so lists,
// the result matrix and event history survive a restart.
package store

import (
	"context"
	"errors"
	"time"
)

var (
	// ErrNotFound means no row matched.
	ErrNotFound = errors.New("not found")
	// ErrConflict means a conditional write did not apply (wrong status, duplicate key).
	ErrConflict = errors.New("conflict")
)

// Plan statuses, mirrored from apiv1.PlanStatus.
const (
	PlanPending  = "pending_approval"
	PlanApproved = "approved"
	PlanRejected = "rejected"
	PlanStarting = "starting"
	PlanStarted  = "started"
)

// Plan is one submitted plan.
type Plan struct {
	Ref    string
	PlanID string // the engine plan's "id"
	Status string
	// Summary is the apiv1.PlanSummary JSON.
	Summary []byte
	// Raw is the plan JSON exactly as submitted, with ${VAR} references unexpanded.
	// It never contains secrets; they are expanded from the server environment at start.
	Raw []byte
	// Revision is 1 at submit and grows by one with every attachment upload
	// (PutPlanAttachment), so an approval can name the content it reviewed.
	Revision int64
	// RollbackOf is the run a rollback plan reverses, or empty. At most one
	// plan that is not rejected rolls back a given run (CreatePlan).
	RollbackOf string

	SubmittedBy   string
	SubmittedAt   time.Time
	ReviewedBy    string     // approver or rejecter, empty until reviewed
	ReviewedAt    *time.Time // nil until reviewed
	ReviewComment string
	RunID         string // empty until started
}

// PlanUpdate is applied together with a status transition. Empty fields are left unchanged.
type PlanUpdate struct {
	ReviewedBy    string
	ReviewedAt    *time.Time
	ReviewComment string
	RunID         string
	// IfRevision, when not zero, is one more condition of the transition:
	// it applies only while the plan's revision equals IfRevision.
	IfRevision int64
}

// Run is the platform's record of one migration run.
type Run struct {
	RunID     string
	PlanRef   string
	SourceEnv string
	TargetEnv string
	Status    string // apiv1.RunStatus
	// Doc is the full apiv1.Run JSON. The columns above are copies used for queries.
	Doc       []byte
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Event is one stored run event.
type Event struct {
	// Seq is assigned by AppendEvent. It increases across all runs, so it is
	// also increasing within one run and usable as an SSE id.
	Seq     int64
	RunID   string
	Time    time.Time
	Kind    string
	Step    string
	Message string
	Data    []byte // JSON object, or nil
}

// Audit is one audit log entry.
type Audit struct {
	ID        int64 // assigned by AppendAudit
	Time      time.Time
	Actor     string
	Role      string
	Action    string // e.g. "POST /v1/plans/{ref}/approve"
	Target    string // e.g. "/v1/plans/pln_123/approve"
	Status    int    // HTTP status of the response
	RequestID string
	Detail    []byte // JSON object, or nil
}

// Store is the platform's persistence. Implementations must be safe for concurrent use.
type Store interface {
	// CreatePlan inserts a plan. ErrConflict if the ref exists, or if
	// RollbackOf is set and another plan that is not rejected has the same
	// RollbackOf (unless this plan is rejected).
	CreatePlan(ctx context.Context, p Plan) error
	// GetPlan returns ErrNotFound when the ref does not exist.
	GetPlan(ctx context.Context, ref string) (Plan, error)
	// ListPlans returns all plans, newest SubmittedAt first.
	ListPlans(ctx context.Context) ([]Plan, error)
	// TransitionPlan atomically sets status to `to` when the current status is
	// one of `from` (and the revision is u.IfRevision, when set), applying u in
	// the same write, and returns the updated plan. ErrNotFound when the ref
	// does not exist; ErrConflict (wrapped, with the current status or
	// revision in the message) when a condition does not hold.
	TransitionPlan(ctx context.Context, ref string, from []string, to string, u PlanUpdate) (Plan, error)

	// CreateRun inserts a run. ErrConflict if the run id exists.
	CreateRun(ctx context.Context, r Run) error
	// UpdateRun replaces PlanRef, SourceEnv, TargetEnv, Status, Doc and UpdatedAt. ErrNotFound if missing.
	UpdateRun(ctx context.Context, r Run) error
	// GetRun returns ErrNotFound when the run does not exist.
	GetRun(ctx context.Context, runID string) (Run, error)
	// ListRuns returns all runs, newest CreatedAt first.
	ListRuns(ctx context.Context) ([]Run, error)
	// ListRunsByStatus returns runs whose status is one of statuses, newest first.
	ListRunsByStatus(ctx context.Context, statuses ...string) ([]Run, error)

	// AppendEvent stores an event and returns it with Seq set.
	AppendEvent(ctx context.Context, e Event) (Event, error)
	// ListEvents returns a run's events with Seq > afterSeq in Seq order, at most limit (<=0 means no limit).
	ListEvents(ctx context.Context, runID string, afterSeq int64, limit int) ([]Event, error)

	// AppendAudit stores an entry and returns its id.
	AppendAudit(ctx context.Context, a Audit) (int64, error)
	// ListAudit returns entries with ID < before (before <= 0 means from the newest), newest first, at most limit (<=0 means no limit).
	ListAudit(ctx context.Context, before int64, limit int) ([]Audit, error)

	// PutIdempotencyKey records key -> (planRef, runID). If the key exists it
	// returns the stored values and ErrConflict, so the caller can tell a retry
	// of the same plan from a key reused for a different one.
	PutIdempotencyKey(ctx context.Context, key, planRef, runID string) (storedPlanRef, storedRunID string, err error)
	// GetIdempotencyKey returns ErrNotFound when the key is unknown.
	GetIdempotencyKey(ctx context.Context, key string) (planRef, runID string, err error)

	// Close releases connections.
	Close()

	PlanTools   // written-records streams and plan attachments (written.go)
	AgentStore  // agent sessions and events (agent.go)
	DeployStore // apps from GitHub, deployments and their events, webhook deliveries (deploy.go)
}
