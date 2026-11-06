package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// PutImageScan upserts the digest's row unless the stored scan finished later.
func (s *Postgres) PutImageScan(ctx context.Context, sc ImageScan) error {
	tag, err := s.pool.Exec(ctx, `INSERT INTO image_scans (digest, id, status, finished_at, doc, vulnerabilities, report, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (digest) DO UPDATE SET id = EXCLUDED.id, status = EXCLUDED.status, finished_at = EXCLUDED.finished_at,
			doc = EXCLUDED.doc, vulnerabilities = EXCLUDED.vulnerabilities, report = EXCLUDED.report, created_at = EXCLUDED.created_at
		WHERE image_scans.finished_at <= EXCLUDED.finished_at`,
		sc.Digest, sc.ID, sc.Status, sc.FinishedAt, sc.Doc, sc.Vulnerabilities, sc.Report, sc.CreatedAt)
	if isUniqueViolation(err) {
		return fmt.Errorf("%w: scan %s belongs to another digest", ErrConflict, sc.ID)
	}
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: digest %s has a scan that finished later", ErrConflict, sc.Digest)
	}
	return nil
}

func (s *Postgres) ListImageScans(ctx context.Context, since time.Time) ([]ImageScan, error) {
	rows, err := s.pool.Query(ctx, `SELECT digest, id, status, finished_at, doc, vulnerabilities, created_at
		FROM image_scans WHERE finished_at >= $1 ORDER BY digest`, since)
	if err != nil {
		return nil, err
	}
	return collect(rows, func(row pgx.Row) (ImageScan, error) {
		var sc ImageScan
		err := row.Scan(&sc.Digest, &sc.ID, &sc.Status, &sc.FinishedAt, &sc.Doc, &sc.Vulnerabilities, &sc.CreatedAt)
		sc.FinishedAt, sc.CreatedAt = sc.FinishedAt.UTC(), sc.CreatedAt.UTC()
		return sc, err
	})
}
