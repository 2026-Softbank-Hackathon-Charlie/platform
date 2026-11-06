package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

const securityExceptionColumns = `id, target_type, target_id, reason, created_by, expires_at, created_at, revoked_at, coalesce(revoked_by, '')`

// CreateSecurityException inserts e unless its target has an active exception
// at e.CreatedAt, in one statement.
func (s *Postgres) CreateSecurityException(ctx context.Context, e SecurityException) error {
	tag, err := s.pool.Exec(ctx, `INSERT INTO security_exceptions (id, target_type, target_id, reason, created_by, expires_at, created_at)
		SELECT $1, $2, $3, $4, $5, $6, $7
		WHERE NOT EXISTS (SELECT 1 FROM security_exceptions
			WHERE target_type = $2 AND target_id = $3 AND revoked_at IS NULL AND expires_at > $7)`,
		e.ID, e.TargetType, e.TargetID, e.Reason, e.CreatedBy, e.ExpiresAt, e.CreatedAt)
	if isUniqueViolation(err) {
		return fmt.Errorf("%w: security exception %s exists", ErrConflict, e.ID)
	}
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: %s %s has an active exception", ErrConflict, e.TargetType, e.TargetID)
	}
	return nil
}

func (s *Postgres) RevokeSecurityException(ctx context.Context, id, by string, at time.Time) (SecurityException, error) {
	e, err := scanSecurityException(s.pool.QueryRow(ctx, `UPDATE security_exceptions SET revoked_at = $2, revoked_by = $3
		WHERE id = $1 AND revoked_at IS NULL RETURNING `+securityExceptionColumns, id, at, by))
	if !errors.Is(err, pgx.ErrNoRows) {
		return e, err
	}
	var exists bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM security_exceptions WHERE id = $1)`, id).Scan(&exists); err != nil {
		return SecurityException{}, err
	}
	if !exists {
		return SecurityException{}, fmt.Errorf("%w: security exception %s", ErrNotFound, id)
	}
	return SecurityException{}, fmt.Errorf("%w: security exception %s is revoked", ErrConflict, id)
}

func (s *Postgres) ListSecurityExceptions(ctx context.Context) ([]SecurityException, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+securityExceptionColumns+` FROM security_exceptions ORDER BY created_at DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	return collect(rows, scanSecurityException)
}

func scanSecurityException(row pgx.Row) (SecurityException, error) {
	var e SecurityException
	if err := row.Scan(&e.ID, &e.TargetType, &e.TargetID, &e.Reason, &e.CreatedBy, &e.ExpiresAt, &e.CreatedAt, &e.RevokedAt, &e.RevokedBy); err != nil {
		return SecurityException{}, err
	}
	e.ExpiresAt, e.CreatedAt = e.ExpiresAt.UTC(), e.CreatedAt.UTC()
	if e.RevokedAt != nil {
		u := e.RevokedAt.UTC()
		e.RevokedAt = &u
	}
	return e, nil
}
