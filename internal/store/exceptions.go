package store

import (
	"context"
	"time"
)

// SecurityException accepts the risk of one finding or vulnerability until
// ExpiresAt, unless revoked first (SEC-08).
type SecurityException struct {
	ID         string
	TargetType string // apiv1.SecurityExceptionTarget
	TargetID   string
	Reason     string
	CreatedBy  string
	ExpiresAt  time.Time
	CreatedAt  time.Time
	RevokedAt  *time.Time
	RevokedBy  string // "" until revoked
}

// ExceptionStore keeps security exceptions. It is part of Store.
type ExceptionStore interface {
	// CreateSecurityException stores e. ErrConflict (wrapped) when the target has
	// an exception that is not revoked and expires after e.CreatedAt, or when
	// e.ID is taken.
	CreateSecurityException(ctx context.Context, e SecurityException) error
	// RevokeSecurityException sets revoked_at/by. ErrNotFound for an unknown id,
	// ErrConflict when it is already revoked. Returns the updated row.
	RevokeSecurityException(ctx context.Context, id, by string, at time.Time) (SecurityException, error)
	// ListSecurityExceptions returns all exceptions, newest created first.
	ListSecurityExceptions(ctx context.Context) ([]SecurityException, error)
}
