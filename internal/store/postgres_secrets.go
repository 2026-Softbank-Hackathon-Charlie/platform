package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

const appSecretColumns = `app, instance, name, key_version, key_check, nonce, ciphertext, updated_at, updated_by`

func scanAppSecret(row pgx.Row) (AppSecret, error) {
	var s AppSecret
	if err := row.Scan(&s.App, &s.Instance, &s.Name, &s.KeyVersion, &s.KeyCheck, &s.Nonce, &s.Ciphertext, &s.UpdatedAt, &s.UpdatedBy); err != nil {
		return AppSecret{}, err
	}
	s.UpdatedAt = s.UpdatedAt.UTC()
	return s, nil
}

func (s *Postgres) ListAppSecrets(ctx context.Context, app string) ([]AppSecret, error) {
	if !Clean(app) {
		return []AppSecret{}, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT `+appSecretColumns+` FROM app_secrets
		WHERE app = $1 ORDER BY instance COLLATE "C", name COLLATE "C"`, app)
	if err != nil {
		return nil, err
	}
	return collect(rows, scanAppSecret)
}

func (s *Postgres) GetAppSecret(ctx context.Context, app, instance, name string) (AppSecret, error) {
	if !Clean(app) || !Clean(instance) || !Clean(name) {
		return AppSecret{}, fmt.Errorf("%w: secret %q", ErrNotFound, name)
	}
	out, err := scanAppSecret(s.pool.QueryRow(ctx, `SELECT `+appSecretColumns+` FROM app_secrets
		WHERE app = $1 AND instance = $2 AND name = $3`, app, instance, name))
	if errors.Is(err, pgx.ErrNoRows) {
		return AppSecret{}, fmt.Errorf("%w: secret %s", ErrNotFound, name)
	}
	return out, err
}

// ChangeAppSecrets locks the app row (FOR UPDATE), so changes to one app's
// secrets run one at a time and count each other's rows, and a DeleteApp
// waits for it and then removes the new rows too. An instance override
// also holds the branch row (FOR SHARE), so a DeleteAppBranch waits the
// same way. The locks go app, then branch, as in CreateDeployment.
func (s *Postgres) ChangeAppSecrets(ctx context.Context, app, instance string, set []AppSecret, del []string, max int) error {
	if !Clean(app) {
		return fmt.Errorf("%w: app %q", ErrNotFound, app)
	}
	if !Clean(instance) {
		return fmt.Errorf("%w: app %s has no instance %q", ErrInstanceNotFound, app, instance)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) // no-op after Commit

	var one int
	err = tx.QueryRow(ctx, `SELECT 1 FROM apps WHERE name = $1 FOR UPDATE`, app).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: app %s", ErrNotFound, app)
	}
	if err != nil {
		return err
	}
	if instance != "" && instance != app {
		err = tx.QueryRow(ctx, `SELECT 1 FROM app_branches WHERE app = $1 AND instance = $2 FOR SHARE`, app, instance).Scan(&one)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: app %s has no instance %s", ErrInstanceNotFound, app, instance)
		}
		if err != nil {
			return err
		}
	}
	var before int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM app_secrets WHERE app = $1 AND instance = $2`, app, instance).Scan(&before); err != nil {
		return err
	}
	var gone []string
	for _, n := range del {
		if Clean(n) { // no stored name is unclean
			gone = append(gone, n)
		}
	}
	if len(gone) > 0 {
		if _, err := tx.Exec(ctx, `DELETE FROM app_secrets WHERE app = $1 AND instance = $2 AND name = ANY($3::text[])`,
			app, instance, gone); err != nil {
			return err
		}
	}
	for _, r := range set {
		r = r.Clean()
		if _, err := tx.Exec(ctx, `INSERT INTO app_secrets (`+appSecretColumns+`) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
			ON CONFLICT (app, instance, name) DO UPDATE SET key_version = EXCLUDED.key_version, key_check = EXCLUDED.key_check,
				nonce = EXCLUDED.nonce, ciphertext = EXCLUDED.ciphertext, updated_at = EXCLUDED.updated_at, updated_by = EXCLUDED.updated_by`,
			app, instance, r.Name, r.KeyVersion, r.KeyCheck, r.Nonce, r.Ciphertext, r.UpdatedAt, r.UpdatedBy); err != nil {
			return err
		}
	}
	var count int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM app_secrets WHERE app = $1 AND instance = $2`, app, instance).Scan(&count); err != nil {
		return err
	}
	if count > max && count > before {
		return fmt.Errorf("%w: the change leaves %d secrets, at most %d are allowed", ErrSecretLimit, count, max)
	}
	return tx.Commit(ctx)
}

const instanceStorageColumns = `instance, env, app, driver, bucket, key_id, sealed_access_key, sealed_secret_key, cert_fingerprint, created_at`

func scanInstanceStorage(row pgx.Row) (InstanceStorage, error) {
	var s InstanceStorage
	if err := row.Scan(&s.Instance, &s.Env, &s.App, &s.Driver, &s.Bucket, &s.KeyID, &s.SealedAccessKey, &s.SealedSecretKey, &s.CertFingerprint, &s.CreatedAt); err != nil {
		return InstanceStorage{}, err
	}
	s.CreatedAt = s.CreatedAt.UTC()
	return s, nil
}

func (s *Postgres) PutInstanceStorage(ctx context.Context, is InstanceStorage) error {
	is = is.Clean()
	_, err := s.pool.Exec(ctx, `INSERT INTO instance_storage (`+instanceStorageColumns+`) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (instance, env) DO UPDATE SET app = EXCLUDED.app, driver = EXCLUDED.driver, bucket = EXCLUDED.bucket,
			key_id = EXCLUDED.key_id, sealed_access_key = EXCLUDED.sealed_access_key, sealed_secret_key = EXCLUDED.sealed_secret_key,
			cert_fingerprint = EXCLUDED.cert_fingerprint`,
		is.Instance, is.Env, is.App, is.Driver, is.Bucket, is.KeyID, is.SealedAccessKey, is.SealedSecretKey, is.CertFingerprint, is.CreatedAt)
	return err
}

func (s *Postgres) GetInstanceStorage(ctx context.Context, instance, env string) (InstanceStorage, error) {
	if !Clean(instance) || !Clean(env) {
		return InstanceStorage{}, fmt.Errorf("%w: storage of instance %q in %q", ErrNotFound, instance, env)
	}
	out, err := scanInstanceStorage(s.pool.QueryRow(ctx, `SELECT `+instanceStorageColumns+` FROM instance_storage
		WHERE instance = $1 AND env = $2`, instance, env))
	if errors.Is(err, pgx.ErrNoRows) {
		return InstanceStorage{}, fmt.Errorf("%w: storage of instance %s in %s", ErrNotFound, instance, env)
	}
	return out, err
}

func (s *Postgres) ListInstanceStorage(ctx context.Context, app, instance string) ([]InstanceStorage, error) {
	if !Clean(app) || !Clean(instance) {
		return []InstanceStorage{}, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT `+instanceStorageColumns+` FROM instance_storage
		WHERE ($1 = '' OR app = $1) AND ($2 = '' OR instance = $2) ORDER BY instance COLLATE "C", env COLLATE "C"`, app, instance)
	if err != nil {
		return nil, err
	}
	return collect(rows, scanInstanceStorage)
}

func (s *Postgres) DeleteInstanceStorage(ctx context.Context, instance, env string) error {
	if !Clean(instance) || !Clean(env) {
		return fmt.Errorf("%w: storage of instance %q in %q", ErrNotFound, instance, env)
	}
	tag, err := s.pool.Exec(ctx, `DELETE FROM instance_storage WHERE instance = $1 AND env = $2`, instance, env)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: storage of instance %s in %s", ErrNotFound, instance, env)
	}
	return nil
}
