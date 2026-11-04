package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// deviceLoginLock is the pg_advisory_xact_lock key that serializes
// CreateDeviceLogin, so the count of pending device logins it checks stays
// true until its insert commits.
const deviceLoginLock int64 = 0x486f704465766963 // "HopDevic"

const sessionColumns = `token_hash, name, role, kind, client, created_at, expires_at`

func scanSession(row pgx.Row) (Session, error) {
	var s Session
	if err := row.Scan(&s.TokenHash, &s.Name, &s.Role, &s.Kind, &s.Client, &s.CreatedAt, &s.ExpiresAt); err != nil {
		return Session{}, err
	}
	s.CreatedAt, s.ExpiresAt = s.CreatedAt.UTC(), s.ExpiresAt.UTC()
	return s, nil
}

func (s *Postgres) CreateSession(ctx context.Context, a Session) error {
	a = a.Clean()
	// A data-modifying WITH runs to completion even though nothing reads it.
	_, err := s.pool.Exec(ctx, `WITH expired AS (DELETE FROM auth_sessions WHERE expires_at <= $6)
		INSERT INTO auth_sessions (`+sessionColumns+`) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		a.TokenHash, a.Name, a.Role, a.Kind, a.Client, a.CreatedAt, a.ExpiresAt)
	if isUniqueViolation(err) {
		return fmt.Errorf("%w: session exists", ErrConflict)
	}
	return err
}

func (s *Postgres) GetSession(ctx context.Context, tokenHash []byte, now time.Time) (Session, error) {
	a, err := scanSession(s.pool.QueryRow(ctx, `SELECT `+sessionColumns+` FROM auth_sessions
		WHERE token_hash = $1 AND expires_at > $2`, tokenHash, now))
	if errors.Is(err, pgx.ErrNoRows) {
		return Session{}, fmt.Errorf("%w: session", ErrNotFound)
	}
	return a, err
}

func (s *Postgres) DeleteSession(ctx context.Context, tokenHash []byte) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM auth_sessions WHERE token_hash = $1`, tokenHash)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: session", ErrNotFound)
	}
	return nil
}

const deviceLoginColumns = `device_code_hash, user_code, client, status, created_at, expires_at,
	reviewed_by, reviewed_at, token_role`

func scanDeviceLogin(row pgx.Row) (DeviceLogin, error) {
	var d DeviceLogin
	err := row.Scan(&d.DeviceCodeHash, &d.UserCode, &d.Client, &d.Status, &d.CreatedAt, &d.ExpiresAt,
		&d.ReviewedBy, &d.ReviewedAt, &d.TokenRole)
	if err != nil {
		return DeviceLogin{}, err
	}
	d.CreatedAt, d.ExpiresAt = d.CreatedAt.UTC(), d.ExpiresAt.UTC()
	if d.ReviewedAt != nil {
		t := d.ReviewedAt.UTC()
		d.ReviewedAt = &t
	}
	return d, nil
}

func (s *Postgres) CreateDeviceLogin(ctx context.Context, d DeviceLogin, maxPending int) error {
	d = d.Clean()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) // no-op after Commit

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, deviceLoginLock); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM device_logins WHERE expires_at <= $1`, d.CreatedAt); err != nil {
		return err
	}
	var pending int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM device_logins WHERE status = $1`, DeviceLoginPending).Scan(&pending); err != nil {
		return err
	}
	if pending >= maxPending {
		return ErrDeviceLoginLimit
	}
	_, err = tx.Exec(ctx, `INSERT INTO device_logins (`+deviceLoginColumns+`)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		d.DeviceCodeHash, d.UserCode, d.Client, d.Status, d.CreatedAt, d.ExpiresAt, d.ReviewedBy, d.ReviewedAt, d.TokenRole)
	if isUniqueViolation(err) {
		return fmt.Errorf("%w: device login exists", ErrConflict)
	}
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Postgres) GetDeviceLogin(ctx context.Context, userCode string, now time.Time) (DeviceLogin, error) {
	if !Clean(userCode) { // PostgreSQL would reject the parameter
		return DeviceLogin{}, fmt.Errorf("%w: device login", ErrNotFound)
	}
	d, err := scanDeviceLogin(s.pool.QueryRow(ctx, `SELECT `+deviceLoginColumns+` FROM device_logins
		WHERE user_code = $1 AND expires_at > $2`, userCode, now))
	if errors.Is(err, pgx.ErrNoRows) {
		return DeviceLogin{}, fmt.Errorf("%w: device login", ErrNotFound)
	}
	return d, err
}

func (s *Postgres) ReviewDeviceLogin(ctx context.Context, userCode string, r DeviceLoginReview) (DeviceLogin, error) {
	if !Clean(userCode) {
		return DeviceLogin{}, fmt.Errorf("%w: device login", ErrNotFound)
	}
	at := r.At
	// One conditional UPDATE: of concurrent reviews only one finds it pending.
	d, err := scanDeviceLogin(s.pool.QueryRow(ctx, `UPDATE device_logins SET
			status = $2, reviewed_by = $3, token_role = $4, reviewed_at = $5,
			expires_at = GREATEST(expires_at, $6)
		WHERE user_code = $1 AND expires_at > $5 AND status = $7
		RETURNING `+deviceLoginColumns,
		userCode, CleanText(r.Status), CleanText(r.By), CleanText(r.TokenRole), at, r.KeepUntil, DeviceLoginPending))
	if !errors.Is(err, pgx.ErrNoRows) {
		return d, err
	}
	var current string
	err = s.pool.QueryRow(ctx, `SELECT status FROM device_logins WHERE user_code = $1 AND expires_at > $2`, userCode, at).Scan(&current)
	if errors.Is(err, pgx.ErrNoRows) {
		return DeviceLogin{}, fmt.Errorf("%w: device login", ErrNotFound)
	}
	if err != nil {
		return DeviceLogin{}, err
	}
	return DeviceLogin{}, fmt.Errorf("%w: device login is %s", ErrConflict, current)
}

// ExchangeDeviceLogin takes the device login in one statement: the DELETE
// removes it when it is approved or denied, and the SELECT returns it when
// it is pending. Both see the same snapshot, so a review that commits in
// between leaves it pending for the next poll. Of two concurrent calls on an
// approved one, the second waits for the first's row lock and then finds the
// row gone.
func (s *Postgres) ExchangeDeviceLogin(ctx context.Context, deviceCodeHash []byte, a Session, now time.Time) (DeviceLogin, Session, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return DeviceLogin{}, Session{}, err
	}
	defer tx.Rollback(ctx) // no-op after Commit

	d, err := scanDeviceLogin(tx.QueryRow(ctx, `WITH taken AS (
			DELETE FROM device_logins
			WHERE device_code_hash = $1 AND expires_at > $2 AND status <> $3
			RETURNING `+deviceLoginColumns+`
		)
		SELECT `+deviceLoginColumns+` FROM taken
		UNION ALL
		SELECT `+deviceLoginColumns+` FROM device_logins
		WHERE device_code_hash = $1 AND expires_at > $2 AND status = $3`,
		deviceCodeHash, now, DeviceLoginPending))
	if errors.Is(err, pgx.ErrNoRows) {
		return DeviceLogin{}, Session{}, fmt.Errorf("%w: device login", ErrNotFound)
	}
	if err != nil {
		return DeviceLogin{}, Session{}, err
	}
	var out Session
	if d.Status == DeviceLoginApproved {
		a.Name, a.Role, a.Client = d.ReviewedBy, d.TokenRole, d.Client
		a = a.Clean()
		out, err = scanSession(tx.QueryRow(ctx, `INSERT INTO auth_sessions (`+sessionColumns+`)
			VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING `+sessionColumns,
			a.TokenHash, a.Name, a.Role, a.Kind, a.Client, a.CreatedAt, a.ExpiresAt))
		if isUniqueViolation(err) {
			return DeviceLogin{}, Session{}, fmt.Errorf("%w: session exists", ErrConflict)
		}
		if err != nil {
			return DeviceLogin{}, Session{}, err
		}
	}
	// A pending one is unchanged; committing is the same as rolling back.
	if err := tx.Commit(ctx); err != nil {
		return DeviceLogin{}, Session{}, err
	}
	return d, out, nil
}
