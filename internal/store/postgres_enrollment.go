package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

const enrollmentColumns = `id,actor,environment,install_k3s,code_hash,request_hash,status,created_at,updated_at,expires_at`

func scanEnrollment(row pgx.Row) (OnpremEnrollment, error) {
	var e OnpremEnrollment
	err := row.Scan(&e.ID, &e.Actor, &e.Environment, &e.InstallK3s, &e.CodeHash, &e.RequestHash, &e.Status, &e.CreatedAt, &e.UpdatedAt, &e.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrNotFound
	}
	return e, err
}

func (s *Postgres) CreateOnpremEnrollment(ctx context.Context, e OnpremEnrollment) (OnpremEnrollment, bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return OnpremEnrollment{}, false, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(5212219560043)`); err != nil {
		return OnpremEnrollment{}, false, err
	}
	old, err := scanEnrollment(tx.QueryRow(ctx, `SELECT `+enrollmentColumns+` FROM onprem_enrollments WHERE actor=$1 AND request_hash=$2`, e.Actor, e.RequestHash))
	if err == nil {
		if old.Environment != e.Environment || old.InstallK3s != e.InstallK3s {
			return OnpremEnrollment{}, false, ErrConflict
		}
		return old, false, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return OnpremEnrollment{}, false, err
	}
	var n int
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM onprem_enrollments WHERE status IN ('pending','claimed','verifying') AND expires_at>$1`, e.CreatedAt).Scan(&n); err != nil {
		return OnpremEnrollment{}, false, err
	}
	if n >= 64 {
		return OnpremEnrollment{}, false, ErrConflict
	}
	_, err = tx.Exec(ctx, `INSERT INTO onprem_enrollments (`+enrollmentColumns+`) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, e.ID, e.Actor, e.Environment, e.InstallK3s, e.CodeHash, e.RequestHash, e.Status, e.CreatedAt, e.UpdatedAt, e.ExpiresAt)
	if isUniqueViolation(err) {
		return OnpremEnrollment{}, false, ErrConflict
	}
	if err != nil {
		return OnpremEnrollment{}, false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return OnpremEnrollment{}, false, err
	}
	return e, true, nil
}

func (s *Postgres) GetOnpremEnrollment(ctx context.Context, id string, now time.Time) (OnpremEnrollment, error) {
	if _, err := s.pool.Exec(ctx, `UPDATE onprem_enrollments SET status='expired',updated_at=$2 WHERE id=$1 AND status='pending' AND expires_at<=$2`, id, now); err != nil {
		return OnpremEnrollment{}, err
	}
	return scanEnrollment(s.pool.QueryRow(ctx, `SELECT `+enrollmentColumns+` FROM onprem_enrollments WHERE id=$1`, id))
}

func (s *Postgres) ClaimOnpremEnrollment(ctx context.Context, id string, hash []byte, now time.Time) (OnpremEnrollment, error) {
	e, err := scanEnrollment(s.pool.QueryRow(ctx, `UPDATE onprem_enrollments SET status='claimed',updated_at=$3 WHERE id=$1 AND code_hash=$2 AND status='pending' AND expires_at>$3 RETURNING `+enrollmentColumns, id, hash, now))
	if errors.Is(err, ErrNotFound) {
		err = ErrConflict
	}
	return e, err
}

func (s *Postgres) RevokeOnpremEnrollment(ctx context.Context, id string, now time.Time) (OnpremEnrollment, error) {
	e, err := scanEnrollment(s.pool.QueryRow(ctx, `UPDATE onprem_enrollments SET status='revoked',updated_at=$2 WHERE id=$1 AND status IN ('pending','claimed','verifying','revoked','expired','failed') RETURNING `+enrollmentColumns, id, now))
	if errors.Is(err, ErrNotFound) {
		if _, getErr := s.GetOnpremEnrollment(ctx, id, now); getErr != nil {
			return e, getErr
		}
		err = ErrConflict
	}
	return e, err
}
