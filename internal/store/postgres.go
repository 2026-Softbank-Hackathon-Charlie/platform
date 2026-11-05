package store

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"path"
	"slices"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrations embed.FS

// migrationLock is the pg_advisory_xact_lock key that serializes migrations
// when several API instances start at once. Advisory locks are per database,
// so instances using different schemas also wait for each other briefly.
const migrationLock int64 = 0x486f70436861726c // "HopCharl"

// Postgres is the Store backed by PostgreSQL.
type Postgres struct {
	pool *pgxpool.Pool
}

var _ Store = (*Postgres)(nil)

// Open connects to databaseURL and applies pending migrations. Query
// parameters pgx does not know are sent as runtime parameters, so
// search_path=... in the URL picks the schema the tables live in.
func Open(ctx context.Context, databaseURL string) (*Postgres, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err) // pgx redacts the password
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connect: %w", err)
	}
	if err := migrate(ctx, pool); err != nil {
		pool.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Postgres{pool: pool}, nil
}

// migrate applies the embedded migrations that schema_migrations does not
// list yet, in file name order, in one transaction.
func migrate(ctx context.Context, pool *pgxpool.Pool) error {
	names, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(names)

	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) // no-op after Commit

	// Lock before CREATE TABLE IF NOT EXISTS: two concurrent creates can still collide.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, migrationLock); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    text PRIMARY KEY,
		applied_at timestamptz NOT NULL DEFAULT now()
	)`); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return err
	}
	applied, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	done := make(map[string]bool, len(applied))
	for _, v := range applied {
		done[v] = true
	}

	for _, name := range names {
		version := strings.TrimSuffix(path.Base(name), ".sql")
		if done[version] {
			continue
		}
		sql, err := migrations.ReadFile(name)
		if err != nil {
			return err
		}
		// No arguments, so pgx uses the simple protocol and a file may hold several statements.
		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			return fmt.Errorf("%s: %w", version, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, version); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// Close releases connections.
func (s *Postgres) Close() { s.pool.Close() }

const planColumns = `ref, plan_id, status, summary, raw, submitted_by, submitted_at,
	reviewed_by, reviewed_at, review_comment, run_id, revision, rollback_of, submitted_role`

func scanPlan(row pgx.Row) (Plan, error) {
	var p Plan
	err := row.Scan(&p.Ref, &p.PlanID, &p.Status, &p.Summary, &p.Raw, &p.SubmittedBy, &p.SubmittedAt,
		&p.ReviewedBy, &p.ReviewedAt, &p.ReviewComment, &p.RunID, &p.Revision, &p.RollbackOf, &p.SubmittedRole)
	if err != nil {
		return Plan{}, err
	}
	p.SubmittedAt = p.SubmittedAt.UTC()
	if p.ReviewedAt != nil {
		t := p.ReviewedAt.UTC()
		p.ReviewedAt = &t
	}
	return p, nil
}

func (s *Postgres) CreatePlan(ctx context.Context, p Plan) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO plans (`+planColumns+`)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`,
		p.Ref, p.PlanID, p.Status, p.Summary, p.Raw, p.SubmittedBy, p.SubmittedAt,
		p.ReviewedBy, p.ReviewedAt, p.ReviewComment, p.RunID, p.Revision, p.RollbackOf, p.SubmittedRole)
	var pgErr *pgconn.PgError
	switch {
	case errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "plans_rollback_of_idx":
		return fmt.Errorf("%w: a plan rolling back run %s exists", ErrConflict, p.RollbackOf)
	case isUniqueViolation(err):
		return fmt.Errorf("%w: plan %s exists", ErrConflict, p.Ref)
	}
	return err
}

func (s *Postgres) GetPlan(ctx context.Context, ref string) (Plan, error) {
	p, err := scanPlan(s.pool.QueryRow(ctx, `SELECT `+planColumns+` FROM plans WHERE ref = $1`, ref))
	if errors.Is(err, pgx.ErrNoRows) {
		return Plan{}, fmt.Errorf("%w: plan %s", ErrNotFound, ref)
	}
	return p, err
}

func (s *Postgres) ListPlans(ctx context.Context) ([]Plan, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+planColumns+` FROM plans ORDER BY submitted_at DESC, ref DESC`)
	if err != nil {
		return nil, err
	}
	return collect(rows, scanPlan)
}

func (s *Postgres) TransitionPlan(ctx context.Context, ref string, from []string, to string, u PlanUpdate) (Plan, error) {
	// One conditional UPDATE: concurrent callers serialize on the row lock and
	// the losers re-check status against the winner's write, so only one applies.
	p, err := scanPlan(s.pool.QueryRow(ctx, `UPDATE plans SET
			status         = $2,
			reviewed_by    = COALESCE(NULLIF($3::text, ''), reviewed_by),
			reviewed_at    = COALESCE($4::timestamptz, reviewed_at),
			review_comment = COALESCE(NULLIF($5::text, ''), review_comment),
			run_id         = COALESCE(NULLIF($6::text, ''), run_id)
		WHERE ref = $1 AND status = ANY($7::text[]) AND ($8::bigint = 0 OR revision = $8::bigint)
		RETURNING `+planColumns,
		ref, to, u.ReviewedBy, u.ReviewedAt, u.ReviewComment, u.RunID, from, u.IfRevision))
	if !errors.Is(err, pgx.ErrNoRows) {
		return p, err
	}
	var current string
	var revision int64
	err = s.pool.QueryRow(ctx, `SELECT status, revision FROM plans WHERE ref = $1`, ref).Scan(&current, &revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return Plan{}, fmt.Errorf("%w: plan %s", ErrNotFound, ref)
	}
	if err != nil {
		return Plan{}, err
	}
	return Plan{}, transitionConflict(from, current, u.IfRevision, revision)
}

// transitionConflict names the condition of a transition that did not hold.
func transitionConflict(from []string, status string, ifRevision, revision int64) error {
	if slices.Contains(from, status) && ifRevision != 0 && ifRevision != revision {
		return fmt.Errorf("%w: plan revision is %d, not %d", ErrConflict, revision, ifRevision)
	}
	return fmt.Errorf("%w: plan is %s", ErrConflict, status)
}

const runColumns = `run_id, plan_ref, source_env, target_env, status, doc, created_at, updated_at`

func scanRun(row pgx.Row) (Run, error) {
	var r Run
	err := row.Scan(&r.RunID, &r.PlanRef, &r.SourceEnv, &r.TargetEnv, &r.Status, &r.Doc, &r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		return Run{}, err
	}
	r.CreatedAt, r.UpdatedAt = r.CreatedAt.UTC(), r.UpdatedAt.UTC()
	return r, nil
}

func (s *Postgres) CreateRun(ctx context.Context, r Run) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO runs (`+runColumns+`) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		r.RunID, r.PlanRef, r.SourceEnv, r.TargetEnv, r.Status, r.Doc, r.CreatedAt, r.UpdatedAt)
	if isUniqueViolation(err) {
		return fmt.Errorf("%w: run %s exists", ErrConflict, r.RunID)
	}
	return err
}

func (s *Postgres) UpdateRun(ctx context.Context, r Run) error {
	tag, err := s.pool.Exec(ctx, `UPDATE runs SET
			plan_ref = $2, source_env = $3, target_env = $4, status = $5, doc = $6, updated_at = $7
		WHERE run_id = $1`,
		r.RunID, r.PlanRef, r.SourceEnv, r.TargetEnv, r.Status, r.Doc, r.UpdatedAt)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: run %s", ErrNotFound, r.RunID)
	}
	return nil
}

func (s *Postgres) GetRun(ctx context.Context, runID string) (Run, error) {
	r, err := scanRun(s.pool.QueryRow(ctx, `SELECT `+runColumns+` FROM runs WHERE run_id = $1`, runID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Run{}, fmt.Errorf("%w: run %s", ErrNotFound, runID)
	}
	return r, err
}

func (s *Postgres) ListRuns(ctx context.Context) ([]Run, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+runColumns+` FROM runs ORDER BY created_at DESC, run_id DESC`)
	if err != nil {
		return nil, err
	}
	return collect(rows, scanRun)
}

func (s *Postgres) ListRunsByStatus(ctx context.Context, statuses ...string) ([]Run, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+runColumns+` FROM runs
		WHERE status = ANY($1::text[]) ORDER BY created_at DESC, run_id DESC`, statuses)
	if err != nil {
		return nil, err
	}
	return collect(rows, scanRun)
}

const eventColumns = `seq, run_id, time, kind, step, message, data`

func scanEvent(row pgx.Row) (Event, error) {
	var e Event
	if err := row.Scan(&e.Seq, &e.RunID, &e.Time, &e.Kind, &e.Step, &e.Message, &e.Data); err != nil {
		return Event{}, err
	}
	e.Time = e.Time.UTC()
	return e, nil
}

// AppendEvent takes Seq from a sequence. Concurrent appends can commit out of
// Seq order, so a reader resuming with afterSeq could skip an event that
// commits late. Appending one run's events one at a time rules that out.
func (s *Postgres) AppendEvent(ctx context.Context, e Event) (Event, error) {
	return scanEvent(s.pool.QueryRow(ctx, `INSERT INTO run_events (run_id, time, kind, step, message, data)
		VALUES ($1, $2, $3, $4, $5, $6) RETURNING `+eventColumns,
		e.RunID, e.Time, e.Kind, e.Step, e.Message, e.Data))
}

func (s *Postgres) ListEvents(ctx context.Context, runID string, afterSeq int64, limit int) ([]Event, error) {
	var lim *int // LIMIT NULL means no limit
	if limit > 0 {
		lim = &limit
	}
	rows, err := s.pool.Query(ctx, `SELECT `+eventColumns+` FROM run_events
		WHERE run_id = $1 AND seq > $2 ORDER BY seq LIMIT $3`, runID, afterSeq, lim)
	if err != nil {
		return nil, err
	}
	return collect(rows, scanEvent)
}

const auditColumns = `id, time, actor, role, action, target, status, request_id, detail`

func scanAudit(row pgx.Row) (Audit, error) {
	var a Audit
	err := row.Scan(&a.ID, &a.Time, &a.Actor, &a.Role, &a.Action, &a.Target, &a.Status, &a.RequestID, &a.Detail)
	if err != nil {
		return Audit{}, err
	}
	a.Time = a.Time.UTC()
	return a, nil
}

func (s *Postgres) AppendAudit(ctx context.Context, a Audit) (int64, error) {
	var id int64
	err := s.pool.QueryRow(ctx, `INSERT INTO audit_log (time, actor, role, action, target, status, request_id, detail)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING id`,
		a.Time, a.Actor, a.Role, a.Action, a.Target, a.Status, a.RequestID, a.Detail).Scan(&id)
	return id, err
}

// ListAudit treats limit <= 0 as no limit, like ListEvents.
func (s *Postgres) ListAudit(ctx context.Context, before int64, limit int) ([]Audit, error) {
	if before <= 0 {
		before = math.MaxInt64
	}
	var lim *int
	if limit > 0 {
		lim = &limit
	}
	rows, err := s.pool.Query(ctx, `SELECT `+auditColumns+` FROM audit_log
		WHERE id < $1 ORDER BY id DESC LIMIT $2`, before, lim)
	if err != nil {
		return nil, err
	}
	return collect(rows, scanAudit)
}

func (s *Postgres) PutIdempotencyKey(ctx context.Context, key, planRef, runID string) (string, string, error) {
	var storedPlanRef, storedRunID string
	err := s.pool.QueryRow(ctx, `INSERT INTO idempotency_keys (key, plan_ref, run_id) VALUES ($1, $2, $3)
		ON CONFLICT (key) DO NOTHING RETURNING plan_ref, run_id`,
		key, planRef, runID).Scan(&storedPlanRef, &storedRunID)
	if !errors.Is(err, pgx.ErrNoRows) {
		return storedPlanRef, storedRunID, err
	}
	// DO NOTHING waited for any concurrent insert of the key to commit, so this read sees it.
	storedPlanRef, storedRunID, err = s.GetIdempotencyKey(ctx, key)
	if err != nil {
		return "", "", err
	}
	return storedPlanRef, storedRunID, fmt.Errorf("%w: idempotency key exists", ErrConflict)
}

func (s *Postgres) GetIdempotencyKey(ctx context.Context, key string) (string, string, error) {
	var planRef, runID string
	err := s.pool.QueryRow(ctx, `SELECT plan_ref, run_id FROM idempotency_keys WHERE key = $1`, key).Scan(&planRef, &runID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", fmt.Errorf("%w: idempotency key", ErrNotFound)
	}
	return planRef, runID, err
}

// collect scans every row with scan.
func collect[T any](rows pgx.Rows, scan func(pgx.Row) (T, error)) ([]T, error) {
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (T, error) { return scan(r) })
}

// isUniqueViolation reports a duplicate key (SQLSTATE 23505).
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
