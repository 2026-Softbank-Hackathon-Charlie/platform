package store

import (
	"bytes"
	"context"
	"fmt"
	"time"
)

// Onboarding states, mirrored from apiv1.OnboardingState (ADR 0042, "흐름과
// 상태"). OnboardingFailed keeps the state it failed in in the doc
// (failedState).
const (
	OnboardingDiscovering      = "discovering"
	OnboardingPatching         = "patching"
	OnboardingChecking         = "checking"
	OnboardingAwaitingApproval = "awaiting_approval"
	OnboardingConnecting       = "connecting"
	OnboardingRegistering      = "registering"
	OnboardingSyncing          = "syncing"
	OnboardingReady            = "ready"
	OnboardingCuttingOver      = "cutting_over"
	OnboardingDecommissioning  = "decommissioning"
	OnboardingDone             = "done"
	OnboardingFailed           = "failed"
)

// Onboarding is one onboarding of an app that ran outside the platform
// (ADR 0042). Doc is the full apiv1.Onboarding JSON; App, Repo and State
// are copies of it for queries. Facts is the discovery's
// pkg/onboard/facts.Facts JSON, nil until discovery wrote it.
//
// The values from the VM are only stored encrypted, like AppSecret: under
// the secrets key of KeyVersion and KeyCheck (0 and "" while nothing is
// sealed), with additional data ("onboardings", ID, field, "") where field
// is "secrets", "superuser.password" or "replication.password". Each
// nonce and ciphertext pair is nil until it is known. The store never sees
// the values.
type Onboarding struct {
	ID    string
	App   string
	Repo  string
	State string
	Doc   []byte
	Facts []byte

	KeyVersion int
	KeyCheck   string
	// Secrets is a JSON object of the containers' environment values,
	// {"<container>": {"<NAME>": "<value>"}}, as discovery decrypted them.
	SecretsNonce, SecretsCiphertext []byte
	// SuperuserPassword is the fence user's password.
	SuperuserPasswordNonce, SuperuserPasswordCiphertext []byte
	// ReplicationPassword is hopcharlie_replicator's password.
	ReplicationPasswordNonce, ReplicationPasswordCiphertext []byte

	CreatedAt time.Time
	UpdatedAt time.Time
}

// Clean returns o with its strings passed through CleanText, Doc and Facts
// through CleanJSON and its byte slices copied. Stores apply it on every
// write.
func (o Onboarding) Clean() (Onboarding, error) {
	for _, f := range []*string{&o.ID, &o.App, &o.Repo, &o.State, &o.KeyCheck} {
		*f = CleanText(*f)
	}
	var err error
	if o.Doc, err = CleanJSON(o.Doc); err != nil {
		return Onboarding{}, fmt.Errorf("onboarding doc: %w", err)
	}
	if o.Facts, err = CleanJSON(o.Facts); err != nil {
		return Onboarding{}, fmt.Errorf("onboarding facts: %w", err)
	}
	for _, b := range []*[]byte{&o.SecretsNonce, &o.SecretsCiphertext, &o.SuperuserPasswordNonce,
		&o.SuperuserPasswordCiphertext, &o.ReplicationPasswordNonce, &o.ReplicationPasswordCiphertext} {
		*b = bytes.Clone(*b)
	}
	return o, nil
}

// OnboardingEvent is one step of an onboarding (GET
// /v1/onboardings/{id}/events).
type OnboardingEvent struct {
	// Seq is assigned by AppendOnboardingEvent. It increases across all
	// onboardings, so it is usable as an SSE id.
	Seq          int64
	OnboardingID string
	Time         time.Time
	State        string // the onboarding's state when it happened
	Kind         string // apiv1.OnboardingEventKind
	Message      string
	Data         []byte // JSON object, or nil
}

// OnboardingStore persists onboardings and their events.
type OnboardingStore interface {
	// CreateOnboarding inserts an onboarding. ErrConflict if the id exists
	// or another onboarding of the same app is not done.
	CreateOnboarding(ctx context.Context, o Onboarding) error
	// GetOnboarding returns ErrNotFound when the id does not exist.
	GetOnboarding(ctx context.Context, id string) (Onboarding, error)
	// ListOnboardings returns all onboardings, newest CreatedAt first.
	ListOnboardings(ctx context.Context) ([]Onboarding, error)
	// UpdateOnboarding replaces State, Doc, Facts, the sealed values and
	// UpdatedAt of o.ID while its state is one of from, and returns the
	// stored row. ErrNotFound when the id does not exist; ErrConflict
	// (wrapped, with the current state) when its state is not in from.
	UpdateOnboarding(ctx context.Context, o Onboarding, from []string) (Onboarding, error)
	// DeleteOnboarding removes an onboarding and its events. ErrNotFound
	// when the id does not exist.
	DeleteOnboarding(ctx context.Context, id string) error
	// AppendOnboardingEvent stores an event and returns it with Seq set.
	// ErrNotFound when the onboarding does not exist.
	AppendOnboardingEvent(ctx context.Context, e OnboardingEvent) (OnboardingEvent, error)
	// ListOnboardingEvents returns an onboarding's events with Seq >
	// afterSeq in Seq order, at most limit (<= 0 means no limit).
	ListOnboardingEvents(ctx context.Context, id string, afterSeq int64, limit int) ([]OnboardingEvent, error)
}

// clean returns e with its strings passed through CleanText and Data
// through CleanJSON. OnboardingID is a reference and stays as it is.
func (e OnboardingEvent) clean() (OnboardingEvent, error) {
	for _, f := range []*string{&e.State, &e.Kind, &e.Message} {
		*f = CleanText(*f)
	}
	var err error
	if e.Data, err = CleanJSON(e.Data); err != nil {
		return OnboardingEvent{}, fmt.Errorf("onboarding event data: %w", err)
	}
	return e, nil
}
