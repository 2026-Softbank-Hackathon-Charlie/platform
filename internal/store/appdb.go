package store

import (
	"bytes"
	"time"
)

// AppDatabase is the database pair the platform made for the default
// instance of an app registered in exactly two environments (ADR 0030):
// the database is born primary in PrimaryEnv and a replica of it in
// PeerEnv. Both clusters get the same password of the app account and
// issue their certificates with the same CA. The password and the CA key
// are only stored encrypted (secrets.Cipher.DatabaseRow); the store never
// sees them.
type AppDatabase struct {
	App        string
	PrimaryEnv string // the app's first environment
	PeerEnv    string // the other one
	// KeyVersion and KeyCheck are those of the secrets key that encrypted
	// the password and the CA key, as in AppSecret.
	KeyVersion         int
	KeyCheck           string
	PasswordNonce      []byte
	PasswordCiphertext []byte
	CACert             string // PEM; public
	CAKeyNonce         []byte
	CAKeyCiphertext    []byte
	// PeerGeneration counts the removals of PeerEnv from the app (ADR
	// 0037). The peer's backup path has it once it is not 0, so a peer
	// added back archives to a new, empty path.
	PeerGeneration int
	CreatedAt      time.Time
}

// Clean returns d with every string passed through CleanText and its byte
// slices copied. Stores apply it on every write.
func (d AppDatabase) Clean() AppDatabase {
	for _, f := range []*string{&d.App, &d.PrimaryEnv, &d.PeerEnv, &d.KeyCheck, &d.CACert} {
		*f = CleanText(*f)
	}
	d.PasswordNonce, d.PasswordCiphertext = bytes.Clone(d.PasswordNonce), bytes.Clone(d.PasswordCiphertext)
	d.CAKeyNonce, d.CAKeyCiphertext = bytes.Clone(d.CAKeyNonce), bytes.Clone(d.CAKeyCiphertext)
	return d
}
