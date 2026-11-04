package store

import (
	"context"
	"fmt"
	"time"
)

// Session kinds.
const (
	// SessionConsole is an administrator's console login (POST /v1/auth/login).
	SessionConsole = "console"
	// SessionCLI is a CLI token from an approved device login.
	SessionCLI = "cli"
)

// Device login statuses, mirrored from apiv1.DeviceLoginStatus.
const (
	DeviceLoginPending  = "pending"
	DeviceLoginApproved = "approved"
	DeviceLoginDenied   = "denied"
)

// ErrDeviceLoginLimit means as many device logins as allowed are pending. It
// wraps ErrConflict.
var ErrDeviceLoginLimit = fmt.Errorf("%w: too many pending device logins", ErrConflict)

// Session is a console session or a CLI token (ADR 0018). The token itself
// is never stored, only its SHA-256.
type Session struct {
	TokenHash []byte
	Name      string // who logged in or approved the device login; the audit log's actor
	Role      string // apiv1.Role
	Kind      string // SessionConsole or SessionCLI
	Client    string // the CLI's DeviceLogin.Client; empty for the console
	CreatedAt time.Time
	ExpiresAt time.Time
}

// DeviceLogin is a CLI waiting for an administrator to approve its login in
// the console (ADR 0018). The device code is never stored, only its SHA-256.
type DeviceLogin struct {
	DeviceCodeHash []byte
	UserCode       string // the code the person enters, in the stored form (no dash)
	Client         string // what the CLI says it is, shown before approval
	Status         string
	CreatedAt      time.Time
	ExpiresAt      time.Time
	// The review: who approved or denied it, and when. Empty and nil until
	// reviewed.
	ReviewedBy string
	ReviewedAt *time.Time
	// TokenRole is the role the approver picked for the CLI token (apiv1.Role);
	// empty unless approved.
	TokenRole string
}

// DeviceLoginReview is an administrator's decision on a pending device login.
type DeviceLoginReview struct {
	Status    string // DeviceLoginApproved or DeviceLoginDenied
	By        string // the reviewer, as in the audit log
	TokenRole string // the CLI token's role when approved; empty when denied
	At        time.Time
	// KeepUntil moves ExpiresAt to it when it is later, so a CLI that polls
	// a little late can still exchange an approval. The zero time keeps
	// ExpiresAt.
	KeepUntil time.Time
}

// Clean returns s with every string passed through CleanText. Stores apply
// it on every write.
func (s Session) Clean() Session {
	for _, f := range []*string{&s.Name, &s.Role, &s.Kind, &s.Client} {
		*f = CleanText(*f)
	}
	return s
}

// Clean returns d with every string passed through CleanText. Stores apply
// it on every write.
func (d DeviceLogin) Clean() DeviceLogin {
	for _, f := range []*string{&d.UserCode, &d.Client, &d.Status, &d.ReviewedBy, &d.TokenRole} {
		*f = CleanText(*f)
	}
	return d
}

// AuthStore persists console sessions, CLI tokens and device logins. It is
// part of Store. Calls that read compare expiry with the time they are given,
// so a row that expired is gone for them even before it is removed. Strings
// are stored as Clean leaves them, and a lookup by a user code that is not
// clean finds nothing.
type AuthStore interface {
	// CreateSession removes the sessions that expired at s.CreatedAt and
	// inserts s. ErrConflict if the token hash exists.
	CreateSession(ctx context.Context, s Session) error
	// GetSession returns the session with tokenHash whose ExpiresAt is after
	// now. ErrNotFound when there is none.
	GetSession(ctx context.Context, tokenHash []byte, now time.Time) (Session, error)
	// DeleteSession removes the session with tokenHash. ErrNotFound when
	// there is none.
	DeleteSession(ctx context.Context, tokenHash []byte) error

	// CreateDeviceLogin removes the device logins that expired at
	// d.CreatedAt and inserts d, which is pending. ErrDeviceLoginLimit when
	// maxPending device logins are pending; ErrConflict when d.UserCode or
	// d.DeviceCodeHash is taken. Creates are serialized, so concurrent ones
	// never pass maxPending.
	CreateDeviceLogin(ctx context.Context, d DeviceLogin, maxPending int) error
	// GetDeviceLogin returns the device login with userCode whose ExpiresAt
	// is after now. ErrNotFound when there is none.
	GetDeviceLogin(ctx context.Context, userCode string, now time.Time) (DeviceLogin, error)
	// ReviewDeviceLogin applies r to the pending device login with userCode
	// whose ExpiresAt is after r.At, in one conditional write: it sets
	// Status, ReviewedBy, ReviewedAt and TokenRole, and moves ExpiresAt to
	// r.KeepUntil if that is later. It returns the updated device login.
	// ErrNotFound when there is none; ErrConflict (wrapped, with the current
	// status) when it is not pending.
	ReviewDeviceLogin(ctx context.Context, userCode string, r DeviceLoginReview) (DeviceLogin, error)
	// ExchangeDeviceLogin settles the device login with deviceCodeHash whose
	// ExpiresAt is after now and returns it as it was:
	//   - pending: it stays as it is.
	//   - denied: it is removed.
	//   - approved: it is removed, and in the same transaction s is inserted
	//     with Name from the review (ReviewedBy), Role from TokenRole and
	//     Client from the device login; the stored session is returned too.
	// ErrNotFound when there is none (unknown, expired or already settled);
	// ErrConflict, with nothing changed, if s.TokenHash exists. Of concurrent
	// calls for one device login, only one gets it approved or denied.
	ExchangeDeviceLogin(ctx context.Context, deviceCodeHash []byte, s Session, now time.Time) (DeviceLogin, Session, error)
}
