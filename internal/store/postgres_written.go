package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

const writtenColumns = `seq, stream, tbl, pk, at, received_at, event_id, scope, coverage, generation`

func scanWritten(row pgx.Row) (WrittenRecord, error) {
	var w WrittenRecord
	if err := row.Scan(&w.Seq, &w.Stream, &w.Table, &w.PK, &w.At, &w.ReceivedAt, &w.EventID, &w.Scope, &w.Coverage, &w.Generation); err != nil {
		return WrittenRecord{}, err
	}
	w.At, w.ReceivedAt = w.At.UTC(), w.ReceivedAt.UTC()
	return w, nil
}

// AppendWrittenRecords commits a batch atomically. Stream serialization keeps
// exported sequence cutoffs stable even when reporters retry concurrently.
func (s *Postgres) AppendWrittenRecords(ctx context.Context, stream string, recs []WrittenRecord) error {
	if len(recs) == 0 {
		return nil
	}
	recs = append([]WrittenRecord(nil), recs...)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "written-records:"+stream); err != nil {
		return err
	}
	for i := range recs {
		if recs[i].Scope == "" {
			recs[i].Scope = "unspecified"
		}
		if recs[i].Coverage == "" {
			recs[i].Coverage = "reported_records"
		}
	}
	var previousGeneration, previousScope, previousCoverage string
	err = tx.QueryRow(ctx, `SELECT generation,scope,coverage FROM written_records WHERE stream=$1 LIMIT 1`, stream).Scan(&previousGeneration, &previousScope, &previousCoverage)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if err == nil && (previousGeneration != recs[0].Generation || previousScope != recs[0].Scope || previousCoverage != recs[0].Coverage) {
		return fmt.Errorf("%w: written record stream belongs to another database generation", ErrConflict)
	}
	for _, r := range recs {
		if r.Generation != recs[0].Generation || r.Scope != recs[0].Scope || r.Coverage != recs[0].Coverage {
			return fmt.Errorf("%w: mixed database generations", ErrConflict)
		}
		var seq int64
		err = tx.QueryRow(ctx, `INSERT INTO written_records(stream,tbl,pk,at,received_at,event_id,scope,coverage,generation)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)
		ON CONFLICT(stream,event_id) WHERE event_id<>'' DO NOTHING
		RETURNING seq`, stream, r.Table, r.PK, r.At, r.ReceivedAt, r.EventID, r.Scope, r.Coverage, r.Generation).Scan(&seq)
		if errors.Is(err, pgx.ErrNoRows) {
			// An acknowledged retry must not rewrite the stored receipt or create
			// WAL churn on every append-only source sweep.
			err = tx.QueryRow(ctx, `SELECT seq FROM written_records WHERE stream=$1 AND event_id=$2 AND tbl=$3 AND pk=$4 AND at=$5 AND scope=$6 AND coverage=$7 AND generation=$8`, stream, r.EventID, r.Table, r.PK, r.At, r.Scope, r.Coverage, r.Generation).Scan(&seq)
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: written record event identifier was reused", ErrConflict)
		}
		if err != nil {
			return err
		}
	}
	if received := CollectorReceivedAt(recs); !received.IsZero() {
		tag, err := tx.Exec(ctx, `INSERT INTO written_record_collectors(stream,generation,scope,coverage,last_received_at) VALUES($1,$2,$3,$4,$5)
		ON CONFLICT(stream) DO UPDATE SET last_received_at=GREATEST(written_record_collectors.last_received_at,EXCLUDED.last_received_at)
		WHERE written_record_collectors.generation=EXCLUDED.generation AND written_record_collectors.scope=EXCLUDED.scope AND written_record_collectors.coverage=EXCLUDED.coverage`, stream, recs[0].Generation, recs[0].Scope, recs[0].Coverage, received)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("%w: collector identity changed", ErrConflict)
		}
	}
	return tx.Commit(ctx)
}

func (s *Postgres) ListWrittenRecords(ctx context.Context, stream string, afterSeq int64, limit int) ([]WrittenRecord, error) {
	var lim *int // LIMIT NULL means no limit
	if limit > 0 {
		lim = &limit
	}
	rows, err := s.pool.Query(ctx, `SELECT `+writtenColumns+` FROM written_records
		WHERE stream = $1 AND seq > $2 ORDER BY seq LIMIT $3`, stream, afterSeq, lim)
	if err != nil {
		return nil, err
	}
	return collect(rows, scanWritten)
}

func (s *Postgres) WrittenRecordStats(ctx context.Context, stream string) (WrittenStats, error) {
	var st WrittenStats
	err := s.pool.QueryRow(ctx, `SELECT count(*), max(at),coalesce(max(seq),0),CASE WHEN count(DISTINCT scope)=1 THEN min(scope) ELSE 'unspecified' END,CASE WHEN count(DISTINCT coverage)=1 THEN min(coverage) ELSE 'reported_records' END,coalesce(min(generation),''),(SELECT last_received_at FROM written_record_collectors WHERE stream=$1) FROM written_records WHERE stream = $1`, stream).Scan(&st.Count, &st.LatestAt, &st.LastSeq, &st.Scope, &st.Coverage, &st.Generation, &st.LastReceivedAt)
	if st.LatestAt != nil {
		t := st.LatestAt.UTC()
		st.LatestAt = &t
	}
	return st, err
}

// PutPlanAttachment raises the plan's revision with a conditional UPDATE in
// the same transaction as the write. An approval updates the plan row too, so
// it waits for this transaction and then sees the new revision, and this
// transaction sees an approval that committed first; an attachment can never
// change after its plan was approved, nor between a review and its approval.
func (s *Postgres) PutPlanAttachment(ctx context.Context, a PlanAttachment) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) // no-op after Commit

	tag, err := tx.Exec(ctx, `UPDATE plans SET revision = revision + 1 WHERE ref = $1 AND status = $2`, a.Ref, PlanPending)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		var status string
		err = tx.QueryRow(ctx, `SELECT status FROM plans WHERE ref = $1`, a.Ref).Scan(&status)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: plan %s", ErrNotFound, a.Ref)
		}
		if err != nil {
			return err
		}
		return fmt.Errorf("%w: plan is %s", ErrConflict, status)
	}
	content := a.Content
	if content == nil {
		content = []byte{} // the column is NOT NULL
	}
	_, err = tx.Exec(ctx, `INSERT INTO plan_attachments (ref, name, content, sha256, size, created_by, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (ref, name) DO UPDATE SET
			content = EXCLUDED.content, sha256 = EXCLUDED.sha256, size = EXCLUDED.size,
			created_by = EXCLUDED.created_by, created_at = EXCLUDED.created_at`,
		a.Ref, a.Name, content, a.SHA256, a.Size, a.CreatedBy, a.CreatedAt)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Postgres) GetPlanAttachment(ctx context.Context, ref, name string) (PlanAttachment, error) {
	var a PlanAttachment
	err := s.pool.QueryRow(ctx, `SELECT ref, name, content, sha256, size, created_by, created_at
		FROM plan_attachments WHERE ref = $1 AND name = $2`, ref, name).
		Scan(&a.Ref, &a.Name, &a.Content, &a.SHA256, &a.Size, &a.CreatedBy, &a.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return PlanAttachment{}, fmt.Errorf("%w: attachment %s of plan %s", ErrNotFound, name, ref)
	}
	if err != nil {
		return PlanAttachment{}, err
	}
	if a.Content == nil {
		a.Content = []byte{}
	}
	a.CreatedAt = a.CreatedAt.UTC()
	return a, nil
}

func (s *Postgres) ListPlanAttachments(ctx context.Context, refs []string) ([]PlanAttachment, error) {
	rows, err := s.pool.Query(ctx, `SELECT ref, name, sha256, size, created_by, created_at
		FROM plan_attachments WHERE ref = ANY($1::text[]) ORDER BY ref COLLATE "C", name COLLATE "C"`, refs)
	if err != nil {
		return nil, err
	}
	return collect(rows, func(row pgx.Row) (PlanAttachment, error) {
		var a PlanAttachment
		if err := row.Scan(&a.Ref, &a.Name, &a.SHA256, &a.Size, &a.CreatedBy, &a.CreatedAt); err != nil {
			return PlanAttachment{}, err
		}
		a.CreatedAt = a.CreatedAt.UTC()
		return a, nil
	})
}
