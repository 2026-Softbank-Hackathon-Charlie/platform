package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

const enrollmentBaseColumns = `id,actor,environment,install_k3s,code_hash,request_hash,status,created_at,updated_at,expires_at`
const enrollmentColumns = enrollmentBaseColumns + `,session_hash,session_expires_at,cluster_uid,stage,checks,registration_ciphertext,version,lease_id,lease_expires_at`

func scanEnrollment(row pgx.Row) (OnpremEnrollment, error) {
	var e OnpremEnrollment
	var sessionExpiry, leaseExpiry *time.Time
	var checks []byte
	err := row.Scan(&e.ID, &e.Actor, &e.Environment, &e.InstallK3s, &e.CodeHash, &e.RequestHash, &e.Status, &e.CreatedAt, &e.UpdatedAt, &e.ExpiresAt, &e.SessionHash, &sessionExpiry, &e.ClusterUID, &e.Stage, &checks, &e.RegistrationCiphertext, &e.Version, &e.LeaseID, &leaseExpiry)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrNotFound
	}
	if err == nil {
		if sessionExpiry != nil {
			e.SessionExpiresAt = *sessionExpiry
		}
		if leaseExpiry != nil {
			e.LeaseExpiresAt = *leaseExpiry
		}
		err = json.Unmarshal(checks, &e.Checks)
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
	_, err = tx.Exec(ctx, `INSERT INTO onprem_enrollments (`+enrollmentBaseColumns+`) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, e.ID, e.Actor, e.Environment, e.InstallK3s, e.CodeHash, e.RequestHash, e.Status, e.CreatedAt, e.UpdatedAt, e.ExpiresAt)
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
	e, err := scanEnrollment(s.pool.QueryRow(ctx, `UPDATE onprem_enrollments SET status='revoked',updated_at=$2,session_hash=NULL,session_expires_at=NULL,registration_ciphertext=NULL,lease_id='',lease_expires_at=NULL,version=version+1 WHERE id=$1 RETURNING `+enrollmentColumns, id, now))
	if errors.Is(err, ErrNotFound) {
		if _, getErr := s.GetOnpremEnrollment(ctx, id, now); getErr != nil {
			return e, getErr
		}
		err = ErrConflict
	}
	return e, err
}

func (s *Postgres) ClaimOnpremRegistration(ctx context.Context, id string, codeHash, sessionHash []byte, sessionExpiresAt, now time.Time) (OnpremEnrollment, error) {
	if len(sessionHash) != 32 || !sessionExpiresAt.After(now) || sessionExpiresAt.Sub(now) > 30*24*time.Hour {
		return OnpremEnrollment{}, ErrConflict
	}
	e, err := scanEnrollment(s.pool.QueryRow(ctx, `UPDATE onprem_enrollments SET status='claimed',updated_at=$5,session_hash=$3,session_expires_at=$4,version=version+1 WHERE id=$1 AND code_hash=$2 AND status='pending' AND expires_at>$5 RETURNING `+enrollmentColumns, id, codeHash, sessionHash, sessionExpiresAt, now))
	if errors.Is(err, ErrNotFound) {
		err = ErrConflict
	}
	return e, err
}

func (s *Postgres) ReserveOnpremBootstrap(ctx context.Context, id string, sessionHash []byte, now time.Time) (OnpremEnrollment, error) {
	if len(sessionHash) != 32 {
		return OnpremEnrollment{}, ErrConflict
	}
	e, err := scanEnrollment(s.pool.QueryRow(ctx, `UPDATE onprem_enrollments SET stage='bootstrap_issued',updated_at=$3,version=version+1 WHERE id=$1 AND session_hash=$2 AND session_expires_at>$3 AND status='claimed' AND stage='' AND cluster_uid='' RETURNING `+enrollmentColumns, id, sessionHash, now))
	if errors.Is(err, ErrNotFound) {
		err = ErrConflict
	}
	return e, err
}

func (s *Postgres) BeginOnpremRegistration(ctx context.Context, id string, sessionHash []byte, clusterUID, leaseID string, now, leaseUntil time.Time) (OnpremEnrollment, error) {
	if len(sessionHash) != 32 || clusterUID == "" || len(clusterUID) > 128 || leaseID == "" || len(leaseID) > 128 || !leaseUntil.After(now) || leaseUntil.Sub(now) > 5*time.Minute {
		return OnpremEnrollment{}, ErrConflict
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return OnpremEnrollment{}, err
	}
	defer tx.Rollback(ctx)
	e, err := scanEnrollment(tx.QueryRow(ctx, `SELECT `+enrollmentColumns+` FROM onprem_enrollments WHERE id=$1 AND session_hash=$2 AND session_expires_at>$3 AND status IN ('claimed','verifying','ready','failed') FOR UPDATE`, id, sessionHash, now))
	if errors.Is(err, ErrNotFound) {
		return OnpremEnrollment{}, ErrConflict
	}
	if err != nil {
		return OnpremEnrollment{}, err
	}
	if (e.ClusterUID != "" && e.ClusterUID != clusterUID) || (e.LeaseID != "" && e.LeaseExpiresAt.After(now)) {
		return OnpremEnrollment{}, ErrConflict
	}
	_, err = tx.Exec(ctx, `INSERT INTO onprem_environment_bindings(environment,enrollment_id,cluster_uid,created_at) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, e.Environment, id, clusterUID, now)
	if err != nil {
		return OnpremEnrollment{}, err
	}
	var owner, uid string
	if err = tx.QueryRow(ctx, `SELECT enrollment_id,cluster_uid FROM onprem_environment_bindings WHERE environment=$1`, e.Environment).Scan(&owner, &uid); err != nil {
		return OnpremEnrollment{}, err
	}
	if owner != id || uid != clusterUID {
		return OnpremEnrollment{}, ErrConflict
	}
	e, err = scanEnrollment(tx.QueryRow(ctx, `UPDATE onprem_enrollments SET cluster_uid=$2,lease_id=$3,lease_expires_at=$4,version=version+1,updated_at=$5 WHERE id=$1 RETURNING `+enrollmentColumns, id, clusterUID, leaseID, leaseUntil, now))
	if err != nil {
		return OnpremEnrollment{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return OnpremEnrollment{}, err
	}
	return e, nil
}

func (s *Postgres) SaveOnpremRegistration(ctx context.Context, id, leaseID string, expectedVersion int64, u OnpremRegistrationUpdate, now time.Time) (OnpremEnrollment, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return OnpremEnrollment{}, err
	}
	defer tx.Rollback(ctx)
	e, err := scanEnrollment(tx.QueryRow(ctx, `SELECT `+enrollmentColumns+` FROM onprem_enrollments WHERE id=$1 AND lease_id=$2 AND version=$3 AND lease_expires_at>$4 AND session_expires_at>$4 AND status IN ('claimed','verifying','ready','failed') FOR UPDATE`, id, leaseID, expectedVersion, now))
	if errors.Is(err, ErrNotFound) {
		return OnpremEnrollment{}, ErrConflict
	}
	if err != nil {
		return OnpremEnrollment{}, err
	}
	if leaseID == "" || !u.Valid(now, len(e.RegistrationCiphertext) > 0) {
		return OnpremEnrollment{}, ErrConflict
	}
	checks := u.Checks
	if checks == nil {
		checks = []OnpremEnrollmentCheck{}
	}
	raw, err := json.Marshal(checks)
	if err != nil {
		return OnpremEnrollment{}, err
	}
	if u.RegistrationCiphertext != nil {
		e.RegistrationCiphertext = u.RegistrationCiphertext
	}
	if u.SessionExpiresAt != nil {
		e.SessionExpiresAt = *u.SessionExpiresAt
	}
	var leaseExpiry any = e.LeaseExpiresAt
	if u.ReleaseLease {
		leaseID = ""
		leaseExpiry = nil
	}
	e, err = scanEnrollment(tx.QueryRow(ctx, `UPDATE onprem_enrollments SET status=$2,stage=$3,checks=$4,registration_ciphertext=$5,lease_id=$6,lease_expires_at=$7,updated_at=$8,version=version+1,session_expires_at=$9 WHERE id=$1 RETURNING `+enrollmentColumns, id, u.Status, u.Stage, raw, e.RegistrationCiphertext, leaseID, leaseExpiry, now, e.SessionExpiresAt))
	if err != nil {
		return OnpremEnrollment{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return OnpremEnrollment{}, err
	}
	return e, nil
}

func (s *Postgres) ListRegisteredOnpremEnvironments(ctx context.Context, now time.Time) ([]OnpremEnrollment, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+enrollmentColumns+` FROM onprem_enrollments WHERE status='ready' AND session_expires_at>$1 AND registration_ciphertext IS NOT NULL ORDER BY environment`, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []OnpremEnrollment{}
	for rows.Next() {
		e, err := scanEnrollment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
