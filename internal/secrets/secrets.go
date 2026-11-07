// Package secrets encrypts the app secrets operators put in (ADR 0019) for
// the platform database, and computes the checksum that makes pods restart
// when the secrets an instance gets change.
//
// Values are encrypted with AES-256-GCM under PLATFORM_SECRETS_KEY with a
// random nonce, and the additional data is the row's app, instance and name,
// so a ciphertext moved to another row does not decrypt. Every row also
// keeps a key check, a short value derived from the key, so a server started
// with another key says so instead of failing to decrypt.
package secrets

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
)

// KeyEnv is the environment variable that holds the key, base64.
const KeyEnv = "PLATFORM_SECRETS_KEY"

// KeySize is the key length in bytes.
const KeySize = 32

// KeyVersion is the version stored with every value sealed under the
// current key. Rotating the key later adds a version; Open refuses versions
// it has no key for.
const KeyVersion = 1

// HKDF info labels of the keys derived from the secrets key, so neither the
// checksum nor the key check uses the encryption key itself.
const (
	checksumLabel = "hopcharlie app-secrets checksum v1"
	keyCheckLabel = "hopcharlie app-secrets key check v1"
)

// ErrKeyMismatch means a stored secret was written with another secrets
// key than the server's.
var ErrKeyMismatch = errors.New("the secret was stored with another PLATFORM_SECRETS_KEY")

// Cipher encrypts and decrypts app secrets with one key. It is safe for
// concurrent use.
type Cipher struct {
	aead     cipher.AEAD
	checksum []byte // HMAC-SHA256 key of Checksum
	keyCheck string // 12 hex digits derived from the key
}

// FromEnv reads KeyEnv with getenv. An unset or empty variable returns nil
// and no error: secrets are off. A value that is not base64 of 32 bytes is
// an error, so a typo stops the server instead of turning secrets off.
func FromEnv(getenv func(string) string) (*Cipher, error) {
	v := strings.TrimSpace(getenv(KeyEnv))
	if v == "" {
		return nil, nil
	}
	return ParseKey(v)
}

// ParseKey returns a Cipher for a base64 (standard, padded) 32-byte key.
// The error never contains the key.
func ParseKey(b64 string) (*Cipher, error) {
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
	if err != nil {
		return nil, fmt.Errorf("%s is not base64", KeyEnv)
	}
	if len(key) != KeySize {
		return nil, fmt.Errorf("%s must be %d bytes, base64-encoded; it is %d bytes", KeyEnv, KeySize, len(key))
	}
	return NewCipher(key)
}

// NewCipher returns a Cipher for a 32-byte key.
func NewCipher(key []byte) (*Cipher, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("secrets key must be %d bytes", KeySize)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	mk, err := hkdf.Key(sha256.New, key, nil, checksumLabel, sha256.Size)
	if err != nil {
		return nil, err
	}
	kc, err := hkdf.Key(sha256.New, key, nil, keyCheckLabel, 6)
	if err != nil {
		return nil, err
	}
	return &Cipher{aead: aead, checksum: mk, keyCheck: hex.EncodeToString(kc)}, nil
}

// KeyCheck identifies the key without revealing it: 12 hex digits derived
// from it with HKDF. Rows store it, and logs may show it.
func (c *Cipher) KeyCheck() string { return c.keyCheck }

// lengthPrefixed joins parts, each after its length, so no two different
// lists encode alike.
func lengthPrefixed(parts ...string) []byte {
	var out []byte
	for _, p := range parts {
		out = binary.BigEndian.AppendUint32(out, uint32(len(p)))
		out = append(out, p...)
	}
	return out
}

// additionalData binds a ciphertext to its row.
func additionalData(app, instance, name string) []byte {
	return lengthPrefixed(app, instance, name)
}

// Seal encrypts value for the secret name of app, app-wide when instance is
// empty or else the instance's override, and returns the nonce, the
// ciphertext and the key version to store with them.
func (c *Cipher) Seal(app, instance, name, value string) (nonce, ciphertext []byte, version int, err error) {
	nonce = make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, 0, err
	}
	return nonce, c.aead.Seal(nil, nonce, []byte(value), additionalData(app, instance, name)), KeyVersion, nil
}

// Open decrypts a stored value. It fails when the key version is unknown,
// the key is not the one that sealed it, or the row's app, instance or name
// differ from the ones it was sealed for. The error never contains the
// value.
func (c *Cipher) Open(app, instance, name string, version int, nonce, ciphertext []byte) (string, error) {
	if version != KeyVersion {
		return "", fmt.Errorf("secret %s: key version %d is not the current key (%d)", name, version, KeyVersion)
	}
	if len(nonce) != c.aead.NonceSize() {
		return "", fmt.Errorf("secret %s: stored nonce has %d bytes", name, len(nonce))
	}
	out, err := c.aead.Open(nil, nonce, ciphertext, additionalData(app, instance, name))
	if err != nil {
		return "", fmt.Errorf("secret %s: does not decrypt with the secrets key", name)
	}
	return string(out), nil
}

// Row encrypts value into a stored secret row (Seal) with the key check,
// the given updatedBy and no UpdatedAt; the caller sets the time.
func (c *Cipher) Row(app, instance, name, value, updatedBy string) (store.AppSecret, error) {
	nonce, ct, v, err := c.Seal(app, instance, name, value)
	if err != nil {
		return store.AppSecret{}, err
	}
	return store.AppSecret{App: app, Instance: instance, Name: name, KeyVersion: v, KeyCheck: c.keyCheck,
		Nonce: nonce, Ciphertext: ct, UpdatedBy: updatedBy}, nil
}

// OpenRow decrypts a stored secret row (Open). A row whose key check is not
// this key's is ErrKeyMismatch, with both checks in the message.
func (c *Cipher) OpenRow(r store.AppSecret) (string, error) {
	if r.KeyCheck != c.keyCheck {
		return "", fmt.Errorf("secret %s of %s: %w (stored with key check %q, the server's key has %q)", r.Name, r.App, ErrKeyMismatch, r.KeyCheck, c.keyCheck)
	}
	return c.Open(r.App, r.Instance, r.Name, r.KeyVersion, r.Nonce, r.Ciphertext)
}

// Lister is the part of the store OtherKeyRows reads.
type Lister interface {
	ListApps(ctx context.Context) ([]store.App, error)
	ListAppSecrets(ctx context.Context, app string) ([]store.AppSecret, error)
}

// OtherKeyRows counts the stored secrets written with another key, for a
// warning at startup: they cannot be read or deployed with this key.
func (c *Cipher) OtherKeyRows(ctx context.Context, st Lister) (int, error) {
	apps, err := st.ListApps(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, a := range apps {
		rows, err := st.ListAppSecrets(ctx, a.Name)
		if err != nil {
			return 0, err
		}
		for _, r := range rows {
			if r.KeyCheck != c.keyCheck {
				n++
			}
		}
	}
	return n, nil
}

// InstanceValues returns the secrets instance gets, decrypted: the
// app-wide rows of rows (Instance "") with instance's overrides on top.
// rows are one app's secrets, as store.ListAppSecrets returns them; the
// other instances' overrides are ignored. The result is empty, not nil,
// when the instance gets none.
func (c *Cipher) InstanceValues(rows []store.AppSecret, instance string) (map[string]string, error) {
	if instance == "" {
		return nil, errors.New("secrets: an instance name is required")
	}
	out := map[string]string{}
	// App-wide first, so the overrides replace them.
	for _, scope := range []string{"", instance} {
		for _, r := range rows {
			if r.Instance != scope {
				continue
			}
			v, err := c.OpenRow(r)
			if err != nil {
				return nil, err
			}
			out[r.Name] = v
		}
	}
	return out, nil
}

// Checksum is the HMAC-SHA256, in lowercase hex, of the app, the instance
// and the set of names and values the instance gets, for
// values.sealedSecrets.appChecksum (ADR 0019): the chart puts it on the pod
// template, so pods restart when the secrets change and only then (sealing
// differs every time, so the sealed values cannot tell). Its key is derived
// from the secrets key, so the checksum reveals nothing about the values to
// anyone without the key. Everything is length-prefixed, names in order, so
// no two different inputs encode alike.
func (c *Cipher) Checksum(app, instance string, values map[string]string) string {
	names := make([]string, 0, len(values))
	for n := range values {
		names = append(names, n)
	}
	slices.Sort(names)
	parts := []string{app, instance}
	for _, name := range names {
		parts = append(parts, name, values[name])
	}
	mac := hmac.New(sha256.New, c.checksum)
	mac.Write(lengthPrefixed(parts...))
	return hex.EncodeToString(mac.Sum(nil))
}

// The fields of an app's database pair (ADR 0030) that are stored
// encrypted.
const (
	DatabasePassword = "password"
	DatabaseCAKey    = "ca.key"
	// DatabaseReplicationPassword is hopcharlie_replicator's password of an
	// onboarded app's database (ADR 0042), with databaseData's additional
	// data like the other two.
	DatabaseReplicationPassword = "replication.password"
)

// The fields of an onboarding (ADR 0042) that are stored encrypted. Their
// additional data is ("onboardings", id, field, ""), like databaseData's
// with the table's name.
const (
	OnboardingSecrets             = "secrets" // {"<container>": {"<NAME>": "<value>"}}
	OnboardingSuperuserPassword   = "superuser.password"
	OnboardingReplicationPassword = "replication.password"
)

// databaseData binds a database pair's ciphertext to its app and field. The
// label and the fourth part keep it from ever encoding like an app secret's
// additional data.
func databaseData(app, field string) []byte {
	return lengthPrefixed("app_databases", app, field, "")
}

// DatabaseRow encrypts the password and the CA key of app's database pair
// (ADR 0030) into a row with the key check and no CreatedAt; the caller
// sets the time.
func (c *Cipher) DatabaseRow(app, primaryEnv, peerEnv, password, caCert, caKey string) (store.AppDatabase, error) {
	seal := func(field, value string) ([]byte, []byte, error) {
		nonce := make([]byte, c.aead.NonceSize())
		if _, err := rand.Read(nonce); err != nil {
			return nil, nil, err
		}
		return nonce, c.aead.Seal(nil, nonce, []byte(value), databaseData(app, field)), nil
	}
	pwNonce, pw, err := seal(DatabasePassword, password)
	if err != nil {
		return store.AppDatabase{}, err
	}
	keyNonce, key, err := seal(DatabaseCAKey, caKey)
	if err != nil {
		return store.AppDatabase{}, err
	}
	return store.AppDatabase{
		App: app, PrimaryEnv: primaryEnv, PeerEnv: peerEnv, KeyVersion: KeyVersion, KeyCheck: c.keyCheck,
		PasswordNonce: pwNonce, PasswordCiphertext: pw, CACert: caCert, CAKeyNonce: keyNonce, CAKeyCiphertext: key,
	}, nil
}

// OpenDatabase decrypts the password and the CA key of a stored database
// pair. A row of another key is ErrKeyMismatch; the errors never contain a
// value.
func (c *Cipher) OpenDatabase(d store.AppDatabase) (password, caKey string, err error) {
	if d.KeyCheck != c.keyCheck {
		return "", "", fmt.Errorf("database pair of %s: %w (stored with key check %q, the server's key has %q)", d.App, ErrKeyMismatch, d.KeyCheck, c.keyCheck)
	}
	if d.KeyVersion != KeyVersion {
		return "", "", fmt.Errorf("database pair of %s: key version %d is not the current key (%d)", d.App, d.KeyVersion, KeyVersion)
	}
	open := func(field string, nonce, ct []byte) (string, error) {
		if len(nonce) != c.aead.NonceSize() {
			return "", fmt.Errorf("database pair of %s: stored %s nonce has %d bytes", d.App, field, len(nonce))
		}
		out, err := c.aead.Open(nil, nonce, ct, databaseData(d.App, field))
		if err != nil {
			return "", fmt.Errorf("database pair of %s: the %s does not decrypt with the secrets key", d.App, field)
		}
		return string(out), nil
	}
	if password, err = open(DatabasePassword, d.PasswordNonce, d.PasswordCiphertext); err != nil {
		return "", "", err
	}
	if caKey, err = open(DatabaseCAKey, d.CAKeyNonce, d.CAKeyCiphertext); err != nil {
		return "", "", err
	}
	return password, caKey, nil
}

// onboardingData binds an onboarding's ciphertext to its id and field:
// ("onboardings", id, field, ""), like databaseData's with the table's name.
func onboardingData(id, field string) []byte {
	return lengthPrefixed("onboardings", id, field, "")
}

// SealOnboarding encrypts one field of an onboarding (OnboardingSecrets,
// OnboardingSuperuserPassword or OnboardingReplicationPassword) for the
// row's nonce and ciphertext columns; the row's KeyVersion and KeyCheck are
// KeyVersion and KeyCheck().
func (c *Cipher) SealOnboarding(id, field, value string) (nonce, ciphertext []byte, err error) {
	nonce = make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, err
	}
	return nonce, c.aead.Seal(nil, nonce, []byte(value), onboardingData(id, field)), nil
}

// OpenOnboarding decrypts one field of a stored onboarding. A row of
// another key is ErrKeyMismatch; the errors never contain a value.
func (c *Cipher) OpenOnboarding(o store.Onboarding, field string, nonce, ciphertext []byte) (string, error) {
	if o.KeyCheck != c.keyCheck {
		return "", fmt.Errorf("onboarding %s: %w (stored with key check %q, the server's key has %q)", o.ID, ErrKeyMismatch, o.KeyCheck, c.keyCheck)
	}
	if o.KeyVersion != KeyVersion {
		return "", fmt.Errorf("onboarding %s: key version %d is not the current key (%d)", o.ID, o.KeyVersion, KeyVersion)
	}
	if len(nonce) != c.aead.NonceSize() {
		return "", fmt.Errorf("onboarding %s: stored %s nonce has %d bytes", o.ID, field, len(nonce))
	}
	out, err := c.aead.Open(nil, nonce, ciphertext, onboardingData(o.ID, field))
	if err != nil {
		return "", fmt.Errorf("onboarding %s: the %s does not decrypt with the secrets key", o.ID, field)
	}
	return string(out), nil
}
