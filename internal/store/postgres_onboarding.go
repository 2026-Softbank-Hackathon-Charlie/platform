package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

const onboardingColumns = `id, app, repo, state, revision, doc, facts, key_version, key_check,
	secrets_nonce, secrets_ciphertext, superuser_password_nonce, superuser_password_ciphertext,
	replication_password_nonce, replication_password_ciphertext, created_at, updated_at`

func scanOnboarding(row pgx.Row) (Onboarding, error) {
	var o Onboarding
	err := row.Scan(&o.ID, &o.App, &o.Repo, &o.State, &o.Revision, &o.Doc, &o.Facts, &o.KeyVersion, &o.KeyCheck,
		&o.SecretsNonce, &o.SecretsCiphertext, &o.SuperuserPasswordNonce, &o.SuperuserPasswordCiphertext,
		&o.ReplicationPasswordNonce, &o.ReplicationPasswordCiphertext, &o.CreatedAt, &o.UpdatedAt)
	if err != nil {
		return Onboarding{}, err
	}
	o.CreatedAt, o.UpdatedAt = o.CreatedAt.UTC(), o.UpdatedAt.UTC()
	return o, nil
}

func (s *Postgres) CreateOnboarding(ctx context.Context, o Onboarding) error {
	o, err := o.Clean()
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `INSERT INTO onboardings (`+onboardingColumns+`)
		VALUES ($1, $2, $3, $4, 1, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)`,
		o.ID, o.App, o.Repo, o.State, o.Doc, o.Facts, o.KeyVersion, o.KeyCheck,
		o.SecretsNonce, o.SecretsCiphertext, o.SuperuserPasswordNonce, o.SuperuserPasswordCiphertext,
		o.ReplicationPasswordNonce, o.ReplicationPasswordCiphertext, o.CreatedAt, o.UpdatedAt)
	if isUniqueViolation(err) {
		return fmt.Errorf("%w: onboarding %s exists, or app %s has an onboarding that is not done", ErrConflict, o.ID, o.App)
	}
	return err
}

func (s *Postgres) GetOnboarding(ctx context.Context, id string) (Onboarding, error) {
	if !Clean(id) {
		return Onboarding{}, fmt.Errorf("%w: onboarding %q", ErrNotFound, id)
	}
	o, err := scanOnboarding(s.pool.QueryRow(ctx, `SELECT `+onboardingColumns+` FROM onboardings WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Onboarding{}, fmt.Errorf("%w: onboarding %s", ErrNotFound, id)
	}
	return o, err
}

func (s *Postgres) ListOnboardings(ctx context.Context) ([]Onboarding, error) {
	// Summaries: no facts and no sealed values.
	rows, err := s.pool.Query(ctx, `SELECT id, app, repo, state, revision, doc, NULL::jsonb, key_version, key_check,
		NULL::bytea, NULL::bytea, NULL::bytea, NULL::bytea, NULL::bytea, NULL::bytea, created_at, updated_at
		FROM onboardings ORDER BY created_at DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	return collect(rows, scanOnboarding)
}

func (s *Postgres) UpdateOnboarding(ctx context.Context, o Onboarding, from []string) (Onboarding, error) {
	o, err := o.Clean()
	if err != nil {
		return Onboarding{}, err
	}
	if !Clean(o.ID) || o.ID == "" {
		return Onboarding{}, fmt.Errorf("%w: onboarding %q", ErrNotFound, o.ID)
	}
	got, err := scanOnboarding(s.pool.QueryRow(ctx, `UPDATE onboardings SET state = $2,
		doc = $3::jsonb || jsonb_build_object('id', id, 'app', app, 'repo', repo, 'state', $2::text), facts = $4,
		key_version = $5, key_check = $6, secrets_nonce = $7, secrets_ciphertext = $8,
		superuser_password_nonce = $9, superuser_password_ciphertext = $10,
		replication_password_nonce = $11, replication_password_ciphertext = $12, updated_at = $13,
		revision = revision + 1
		WHERE id = $1 AND state = ANY($14) AND revision = $15 RETURNING `+onboardingColumns,
		o.ID, o.State, o.Doc, o.Facts, o.KeyVersion, o.KeyCheck, o.SecretsNonce, o.SecretsCiphertext,
		o.SuperuserPasswordNonce, o.SuperuserPasswordCiphertext, o.ReplicationPasswordNonce, o.ReplicationPasswordCiphertext,
		o.UpdatedAt, from, o.Revision))
	if isUniqueViolation(err) {
		return Onboarding{}, fmt.Errorf("%w: app %s has another onboarding that is not done", ErrConflict, o.App)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return got, err
	}
	var current string
	var rev int64
	err = s.pool.QueryRow(ctx, `SELECT state, revision FROM onboardings WHERE id = $1`, o.ID).Scan(&current, &rev)
	if errors.Is(err, pgx.ErrNoRows) {
		return Onboarding{}, fmt.Errorf("%w: onboarding %s", ErrNotFound, o.ID)
	}
	if err != nil {
		return Onboarding{}, err
	}
	return Onboarding{}, fmt.Errorf("%w: onboarding %s is %s at revision %d (written from revision %d)", ErrConflict, o.ID, current, rev, o.Revision)
}

func (s *Postgres) DeleteOnboarding(ctx context.Context, id string) error {
	if !Clean(id) {
		return fmt.Errorf("%w: onboarding %q", ErrNotFound, id)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) // no-op after Commit
	tag, err := tx.Exec(ctx, `DELETE FROM onboardings WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: onboarding %s", ErrNotFound, id)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM onboarding_events WHERE onboarding_id = $1`, id); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

const onboardingEventColumns = `seq, onboarding_id, time, state, kind, message, data`

func scanOnboardingEvent(row pgx.Row) (OnboardingEvent, error) {
	var e OnboardingEvent
	if err := row.Scan(&e.Seq, &e.OnboardingID, &e.Time, &e.State, &e.Kind, &e.Message, &e.Data); err != nil {
		return OnboardingEvent{}, err
	}
	e.Time = e.Time.UTC()
	return e, nil
}

func (s *Postgres) AppendOnboardingEvent(ctx context.Context, e OnboardingEvent) (OnboardingEvent, error) {
	e, err := e.clean()
	if err != nil {
		return OnboardingEvent{}, err
	}
	if !Clean(e.OnboardingID) {
		return OnboardingEvent{}, fmt.Errorf("%w: onboarding %q", ErrNotFound, e.OnboardingID)
	}
	// The row lock serializes the onboarding's appends, so its events commit
	// in seq order, and keeps a delete from passing an append.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return OnboardingEvent{}, err
	}
	defer tx.Rollback(ctx) // no-op after Commit
	var id string
	err = tx.QueryRow(ctx, `SELECT id FROM onboardings WHERE id = $1 FOR UPDATE`, e.OnboardingID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return OnboardingEvent{}, fmt.Errorf("%w: onboarding %s", ErrNotFound, e.OnboardingID)
	}
	if err != nil {
		return OnboardingEvent{}, err
	}
	got, err := scanOnboardingEvent(tx.QueryRow(ctx, `INSERT INTO onboarding_events (onboarding_id, time, state, kind, message, data)
		VALUES ($1, $2, $3, $4, $5, $6) RETURNING `+onboardingEventColumns,
		e.OnboardingID, e.Time, e.State, e.Kind, e.Message, e.Data))
	if err != nil {
		return OnboardingEvent{}, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM onboarding_events WHERE onboarding_id = $1 AND seq <= (
		SELECT seq FROM onboarding_events WHERE onboarding_id = $1 ORDER BY seq DESC OFFSET $2 LIMIT 1)`,
		e.OnboardingID, OnboardingEventsKept); err != nil {
		return OnboardingEvent{}, err
	}
	return got, tx.Commit(ctx)
}

func (s *Postgres) ListOnboardingEvents(ctx context.Context, id string, afterSeq int64, limit int) ([]OnboardingEvent, error) {
	if !Clean(id) {
		return []OnboardingEvent{}, nil
	}
	var lim *int // LIMIT NULL means no limit
	if limit > 0 {
		lim = &limit
	}
	rows, err := s.pool.Query(ctx, `SELECT `+onboardingEventColumns+` FROM onboarding_events
		WHERE onboarding_id = $1 AND seq > $2 ORDER BY seq LIMIT $3`, id, afterSeq, lim)
	if err != nil {
		return nil, err
	}
	return collect(rows, scanOnboardingEvent)
}
