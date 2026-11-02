package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

const writtenColumns = `seq, stream, tbl, pk, at, received_at`

func scanWritten(row pgx.Row) (WrittenRecord, error) {
	var w WrittenRecord
	if err := row.Scan(&w.Seq, &w.Stream, &w.Table, &w.PK, &w.At, &w.ReceivedAt); err != nil {
		return WrittenRecord{}, err
	}
	w.At, w.ReceivedAt = w.At.UTC(), w.ReceivedAt.UTC()
	return w, nil
}

// AppendWrittenRecords inserts the batch with one statement. WITH ORDINALITY
// keeps Seq in the order the client sent the records.
func (s *Postgres) AppendWrittenRecords(ctx context.Context, stream string, recs []WrittenRecord) error {
	if len(recs) == 0 {
		return nil
	}
	tbls, pks := make([]string, len(recs)), make([]string, len(recs))
	ats, received := make([]time.Time, len(recs)), make([]time.Time, len(recs))
	for i, r := range recs {
		tbls[i], pks[i], ats[i], received[i] = r.Table, r.PK, r.At, r.ReceivedAt
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO written_records (stream, tbl, pk, at, received_at)
		SELECT $1, t.tbl, t.pk, t.at, t.received_at
		FROM unnest($2::text[], $3::text[], $4::timestamptz[], $5::timestamptz[])
			WITH ORDINALITY AS t(tbl, pk, at, received_at, ord)
		ORDER BY t.ord`,
		stream, tbls, pks, ats, received)
	return err
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
	err := s.pool.QueryRow(ctx, `SELECT count(*), max(at) FROM written_records WHERE stream = $1`, stream).Scan(&st.Count, &st.LatestAt)
	if st.LatestAt != nil {
		t := st.LatestAt.UTC()
		st.LatestAt = &t
	}
	return st, err
}

// PutPlanAttachment reads the plan's status FOR SHARE in the same
// transaction as the write. An approval updates the plan row, so it waits for
// this transaction, and this transaction sees an approval that committed
// first; an attachment can never change after its plan was approved.
func (s *Postgres) PutPlanAttachment(ctx context.Context, a PlanAttachment) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) // no-op after Commit

	var status string
	err = tx.QueryRow(ctx, `SELECT status FROM plans WHERE ref = $1 FOR SHARE`, a.Ref).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: plan %s", ErrNotFound, a.Ref)
	}
	if err != nil {
		return err
	}
	if status != PlanPending {
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
