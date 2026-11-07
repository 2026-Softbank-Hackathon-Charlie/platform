package store

import (
	"context"
	"encoding/json"
	"time"
)

func (u OnpremRegistrationUpdate) Valid(now time.Time, hasCipher bool) bool {
	if (u.Status != "verifying" && u.Status != "ready" && u.Status != "failed") || len(u.Stage) > 128 || len(u.RegistrationCiphertext) > 131072 || len(u.Checks) > 5 {
		return false
	}
	if u.SessionExpiresAt != nil && (!u.SessionExpiresAt.After(now) || u.SessionExpiresAt.Sub(now) > 30*24*time.Hour) {
		return false
	}
	seen := map[string]bool{}
	for _, c := range u.Checks {
		if seen[c.ID] || (c.ID != "k3s" && c.ID != "network" && c.ID != "permissions" && c.ID != "image_pull" && c.ID != "hello_world") || len(c.Message) > 2048 {
			return false
		}
		seen[c.ID] = true
		if c.Status != "pending" && c.Status != "running" && c.Status != "passed" && c.Status != "failed" && c.Status != "unknown" {
			return false
		}
		if u.Status == "ready" && (c.Status != "passed" || c.CheckedAt == nil || c.CheckedAt.IsZero()) {
			return false
		}
	}
	b, err := json.Marshal(u.Checks)
	if err != nil || len(b) > 16384 {
		return false
	}
	return u.Status != "ready" || (len(seen) == 5 && (hasCipher || len(u.RegistrationCiphertext) > 0))
}

// OnpremEnrollment contains hashes only, never the bootstrap code or command.
type OnpremEnrollment struct {
	ID, Actor, Environment, Status  string
	InstallK3s                      bool
	CodeHash, RequestHash           []byte
	CreatedAt, UpdatedAt, ExpiresAt time.Time
	SessionHash                     []byte
	SessionExpiresAt                time.Time
	ClusterUID, Stage               string
	Checks                          []OnpremEnrollmentCheck
	RegistrationCiphertext          []byte
	Version                         int64
	LeaseID                         string
	LeaseExpiresAt                  time.Time
}

// OnpremEnrollmentCheck is a server observation, never an installer assertion.
type OnpremEnrollmentCheck struct {
	ID        string     `json:"id"`
	Status    string     `json:"status"`
	Message   string     `json:"message,omitempty"`
	CheckedAt *time.Time `json:"checkedAt,omitempty"`
}

// OnpremRegistrationUpdate replaces observed state under a fenced registration lease.
// A nil RegistrationCiphertext preserves the previously encrypted configuration.
type OnpremRegistrationUpdate struct {
	Status, Stage          string
	Checks                 []OnpremEnrollmentCheck
	RegistrationCiphertext []byte
	ReleaseLease           bool
	SessionExpiresAt       *time.Time
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
	ClaimOnpremRegistration(ctx context.Context, id string, codeHash, sessionHash []byte, sessionExpiresAt, now time.Time) (OnpremEnrollment, error)
	ReserveOnpremBootstrap(ctx context.Context, id string, sessionHash []byte, now time.Time) (OnpremEnrollment, error)
	// Begin reserves the environment permanently for this enrollment and pins its UID.
	// Expired leases can be reacquired only by the same enrollment and cluster UID.
	BeginOnpremRegistration(ctx context.Context, id string, sessionHash []byte, clusterUID, leaseID string, now, leaseUntil time.Time) (OnpremEnrollment, error)
	SaveOnpremRegistration(ctx context.Context, id, leaseID string, expectedVersion int64, update OnpremRegistrationUpdate, now time.Time) (OnpremEnrollment, error)
	ListRegisteredOnpremEnvironments(ctx context.Context, now time.Time) ([]OnpremEnrollment, error)
}
