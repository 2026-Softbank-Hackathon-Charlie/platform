package store

import (
	"context"
	"time"
)

// OnpremEnrollment contains hashes only, never the bootstrap code or command.
type OnpremEnrollment struct {
	ID, Actor, Environment, Status  string
	InstallK3s                      bool
	CodeHash, RequestHash           []byte
	CreatedAt, UpdatedAt, ExpiresAt time.Time
}

type EnrollmentStore interface {
	// CreateOnpremEnrollment serializes issuance, bounds live enrollments to
	// 64, and returns the existing row for an actor's idempotency hash. Reusing
	// the key with another environment/install choice returns ErrConflict.
	CreateOnpremEnrollment(context.Context, OnpremEnrollment) (row OnpremEnrollment, created bool, err error)
	// GetOnpremEnrollment also expires an unclaimed grant when its deadline
	// has passed. Claim and revoke race through conditional atomic writes.
	GetOnpremEnrollment(context.Context, string, time.Time) (OnpremEnrollment, error)
	ClaimOnpremEnrollment(context.Context, string, []byte, time.Time) (OnpremEnrollment, error)
	RevokeOnpremEnrollment(context.Context, string, time.Time) (OnpremEnrollment, error)
}
