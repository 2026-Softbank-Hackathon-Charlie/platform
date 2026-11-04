// Package secrets encrypts the app secrets operators put in (ADR 0019) for
// the platform database, and computes the checksum that makes pods restart
// when the secrets an instance gets change.
//
// Values are encrypted with AES-256-GCM under PLATFORM_SECRETS_KEY with a
// random nonce, and the additional data is "<app>/<instance>/<name>", so a
// ciphertext moved to another row does not decrypt.
package secrets

import (
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

// checksumLabel derives the checksum key from the secrets key (HKDF info),
// so the checksum never uses the encryption key itself.
const checksumLabel = "hopcharlie app-secrets checksum v1"

// Cipher encrypts and decrypts app secrets with one key. It is safe for
// concurrent use.
type Cipher struct {
	aead     cipher.AEAD
	checksum []byte // HMAC-SHA256 key of Checksum
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
	return &Cipher{aead: aead, checksum: mk}, nil
}

// additionalData binds a ciphertext to its row. App names, instance names
// and secret names have no "/", so the form is unambiguous.
func additionalData(app, instance, name string) []byte {
	return []byte(app + "/" + instance + "/" + name)
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

// Row encrypts value into a stored secret row (Seal), with the given
// updatedBy and no UpdatedAt; the caller sets the time.
func (c *Cipher) Row(app, instance, name, value, updatedBy string) (store.AppSecret, error) {
	nonce, ct, v, err := c.Seal(app, instance, name, value)
	if err != nil {
		return store.AppSecret{}, err
	}
	return store.AppSecret{App: app, Instance: instance, Name: name, KeyVersion: v, Nonce: nonce, Ciphertext: ct, UpdatedBy: updatedBy}, nil
}

// OpenRow decrypts a stored secret row (Open).
func (c *Cipher) OpenRow(r store.AppSecret) (string, error) {
	return c.Open(r.App, r.Instance, r.Name, r.KeyVersion, r.Nonce, r.Ciphertext)
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

// Checksum is the HMAC-SHA256, in hex, of the set of names and values, for
// values.sealedSecrets.appChecksum (ADR 0019): the chart puts it on the pod
// template, so pods restart when the secrets change and only then (sealing
// differs every time, so the sealed values cannot tell). Its key is derived
// from the secrets key, so the checksum reveals nothing about the values to
// anyone without the key. Each name and value is length-prefixed in name
// order, so no two different sets encode alike.
func (c *Cipher) Checksum(values map[string]string) string {
	names := make([]string, 0, len(values))
	for n := range values {
		names = append(names, n)
	}
	slices.Sort(names)
	mac := hmac.New(sha256.New, c.checksum)
	var n [8]byte
	write := func(s string) {
		binary.BigEndian.PutUint64(n[:], uint64(len(s)))
		mac.Write(n[:])
		mac.Write([]byte(s))
	}
	for _, name := range names {
		write(name)
		write(values[name])
	}
	return hex.EncodeToString(mac.Sum(nil))
}
