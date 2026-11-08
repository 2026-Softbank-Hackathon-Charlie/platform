package store

import (
	"bytes"
	"context"
	"fmt"
	"time"
)

// ErrSecretLimit means a change would leave more secrets in one scope (the
// app-wide set or one instance's overrides) than allowed. It wraps
// ErrConflict.
var ErrSecretLimit = fmt.Errorf("%w: too many secrets", ErrConflict)

// AppSecret is one secret of an app registered from GitHub (ADR 0019): an
// app-wide one when Instance is empty, else an override of the instance
// named Instance (the app's name for the default instance). The value is
// only stored encrypted (internal/secrets); the store never sees it.
type AppSecret struct {
	App      string
	Instance string // "" for app-wide
	Name     string
	// KeyVersion is the version of the secrets key that encrypted it, and
	// KeyCheck a short value derived from that key (secrets.Cipher.KeyCheck),
	// so a server with another key can tell.
	KeyVersion int
	KeyCheck   string
	Nonce      []byte
	Ciphertext []byte
	UpdatedAt  time.Time
	UpdatedBy  string
}

// InstanceStorage is the bucket and key the platform made for one instance
// in one environment with that environment's storage driver (ADR 0016).
// Only the key sealed for the environment's cluster is kept, with the key's
// id so the driver can delete it; the plain key is never stored.
type InstanceStorage struct {
	Instance string
	Env      string
	App      string
	Driver   string // aws, minio
	Bucket   string
	KeyID    string
	// OldKeyID is a key a new key replaced that the pods may still use, to
	// delete once a healthy deployment restarted them with KeyID; "" when
	// there is none.
	OldKeyID string
	// SealedAccessKey and SealedSecretKey are the key sealed (strict scope,
	// namespace Instance, Secret app-s3) with the cluster's sealed-secrets
	// certificate, base64 as values.sealedSecrets.s3 holds them.
	SealedAccessKey string
	SealedSecretKey string
	// CertFingerprint identifies the certificate they were sealed with
	// (sealing.Sealer.Fingerprint).
	CertFingerprint string
	CreatedAt       time.Time
}

// Clean returns s with every string passed through CleanText and its byte
// slices copied. Stores apply it on every write.
func (s AppSecret) Clean() AppSecret {
	for _, f := range []*string{&s.App, &s.Instance, &s.Name, &s.KeyCheck, &s.UpdatedBy} {
		*f = CleanText(*f)
	}
	s.Nonce, s.Ciphertext = bytes.Clone(s.Nonce), bytes.Clone(s.Ciphertext)
	return s
}

// Clean returns s with every string passed through CleanText. Stores apply
// it on every write.
func (s InstanceStorage) Clean() InstanceStorage {
	for _, f := range []*string{&s.Instance, &s.Env, &s.App, &s.Driver, &s.Bucket, &s.KeyID, &s.OldKeyID, &s.SealedAccessKey, &s.SealedSecretKey, &s.CertFingerprint} {
		*f = CleanText(*f)
	}
	return s
}

// SecretStore persists the app secrets (ADR 0019) and the buckets and keys
// made for instances (ADR 0016). It is part of Store. Strings are stored as
// Clean leaves them, and a lookup by a key that is not clean finds nothing.
//
// DeleteApp also deletes the app's secrets, and DeleteAppBranch the
// overrides of the branch's instance. Instance storage rows are not
// deleted with the app or the instance: the bucket and key outlive the
// gitops files until the driver has deleted them, and the row is what lets
// the platform do that (and retry it). DeleteInstanceStorage removes a row
// once its bucket and key are gone.
type SecretStore interface {
	// ListAppSecrets returns an app's secrets, the app-wide ones and every
	// instance's overrides, by Instance ("" first) and then Name, in byte
	// order. It is empty, not nil, when there are none or the app does not
	// exist.
	ListAppSecrets(ctx context.Context, app string) ([]AppSecret, error)
	// GetAppSecret returns one secret; instance "" is the app-wide one.
	// ErrNotFound when there is none.
	GetAppSecret(ctx context.Context, app, instance, name string) (AppSecret, error)
	// ChangeAppSecrets changes one scope of an app's secrets in one
	// transaction: instance "" is the app-wide set, otherwise the overrides
	// of that instance (the app's name for the default instance, or the
	// instance of one of its added branches). It stores every row of set
	// (App and Instance are taken from the arguments; a row with the name of
	// an existing secret replaces it) and deletes the names in del (a name
	// that is not there is ignored). ErrNotFound when the app does not
	// exist; ErrInstanceNotFound when instance is not one of its instances;
	// ErrSecretLimit, with nothing changed, when the change would grow the
	// scope to more than max secrets (a change that only deletes or replaces
	// is allowed above max, say after max was lowered). Changes to one app's
	// secrets are serialized,
	// and a concurrent DeleteApp or DeleteAppBranch either sees the change
	// and removes it or makes it fail.
	ChangeAppSecrets(ctx context.Context, app, instance string, set []AppSecret, del []string, max int) error

	// PutInstanceStorage stores the bucket and key of s.Instance in s.Env,
	// replacing the row there is but keeping its CreatedAt.
	PutInstanceStorage(ctx context.Context, s InstanceStorage) error
	// GetInstanceStorage returns ErrNotFound when the instance has no row in
	// env.
	GetInstanceStorage(ctx context.Context, instance, env string) (InstanceStorage, error)
	// ListInstanceStorage returns the rows of app's instances, or of the
	// instance named instance, or both; an empty app or instance matches
	// every one, so ("", "") lists all rows. Rows whose instance or app is
	// gone are included. By Instance and then Env in byte order; empty, not
	// nil, when there are none.
	ListInstanceStorage(ctx context.Context, app, instance string) ([]InstanceStorage, error)
	// DeleteInstanceStorage removes one row. ErrNotFound when there is none.
	DeleteInstanceStorage(ctx context.Context, instance, env string) error

	// PutBackupStorage stores the backup bucket and key of app s.App in
	// s.Env (ADR 0025), replacing the row there is but keeping its
	// CreatedAt. s.Instance is the name the driver made them under. These
	// rows are apart from the instance storage rows, and like them they
	// outlive the app until the driver has deleted the bucket and key
	// (ADR 0037).
	PutBackupStorage(ctx context.Context, s InstanceStorage) error
	// GetBackupStorage returns ErrNotFound when the app has no backup bucket
	// in env.
	GetBackupStorage(ctx context.Context, app, env string) (InstanceStorage, error)
	// ListBackupStorage returns every backup storage row, also those of
	// deleted apps, by App and then Env in byte order; empty, not nil, when
	// there are none.
	ListBackupStorage(ctx context.Context) ([]InstanceStorage, error)
	// DeleteBackupStorage removes one row. ErrNotFound when there is none.
	DeleteBackupStorage(ctx context.Context, app, env string) error

	// GetAppDatabase returns the database pair of an app (ADR 0030),
	// stored by CreateAppWithDatabase. ErrNotFound when it has none.
	GetAppDatabase(ctx context.Context, app string) (AppDatabase, error)
	// ListAppDatabases returns every app's database pair, by app in byte
	// order; empty, not nil, when there are none.
	ListAppDatabases(ctx context.Context) ([]AppDatabase, error)
	// NextPeerGeneration adds one to the PeerGeneration of app's database
	// pair and returns the new value (ADR 0043). ErrNotFound when it has
	// none.
	NextPeerGeneration(ctx context.Context, app string) (int, error)
	// SetAppDatabaseSourceKind changes the source_kind of app's database
	// from from to to (ADR 0042: external to peer once the cutover
	// succeeded). ErrNotFound without a pair, ErrConflict when its kind is
	// not from.
	SetAppDatabaseSourceKind(ctx context.Context, app, from, to string) error
	// SetAppDatabaseReplicationPassword replaces the sealed
	// hopcharlie_replicator password of app's database (ADR 0042: connect
	// rotated it). ErrNotFound without a pair.
	SetAppDatabaseReplicationPassword(ctx context.Context, app string, nonce, ciphertext []byte) error
}
