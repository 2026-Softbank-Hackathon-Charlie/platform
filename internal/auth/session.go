package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"math/big"
	"net/http"
	"strings"
)

// Prefixes of the opaque secrets the platform hands out (ADR 0018). Only
// their SHA-256 (HashSecret) is ever stored.
const (
	// SessionPrefix starts every console session and CLI token.
	SessionPrefix = "hcs_"
	// DeviceCodePrefix starts every device code.
	DeviceCodePrefix = "hcd_"
)

// NewSessionToken returns a new console session or CLI token and its
// SHA-256.
func NewSessionToken() (string, []byte) { return newSecret(SessionPrefix) }

// NewDeviceCode returns a new device code and its SHA-256.
func NewDeviceCode() (string, []byte) { return newSecret(DeviceCodePrefix) }

func newSecret(prefix string) (string, []byte) {
	b := make([]byte, 32)
	_, _ = rand.Read(b) // crypto/rand.Read never returns an error
	s := prefix + base64.RawURLEncoding.EncodeToString(b)
	return s, HashSecret(s)
}

// HashSecret returns the SHA-256 of a session token or device code, the form
// the store keeps.
func HashSecret(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}

// BearerToken returns the request's bearer token.
func BearerToken(r *http.Request) (string, bool) {
	raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return raw, ok && raw != ""
}

// userCodeAlphabet has no vowels, so a code never spells a word, and no
// letter that looks like a digit.
const userCodeAlphabet = "BCDFGHJKLMNPQRSTVWXZ"

// UserCodeLength is the number of letters in a user code.
const UserCodeLength = 8

// NewUserCode returns UserCodeLength letters drawn uniformly from
// userCodeAlphabet, in the stored form (no dash).
func NewUserCode() string {
	b := make([]byte, UserCodeLength)
	n := big.NewInt(int64(len(userCodeAlphabet)))
	for i := range b {
		k, _ := rand.Int(rand.Reader, n)
		b[i] = userCodeAlphabet[k.Int64()]
	}
	return string(b)
}

// NormalizeUserCode turns a user code as a person typed it into the stored
// form: upper case, without spaces and '-'. ok is false when the result is
// not a user code at all.
func NormalizeUserCode(s string) (string, bool) {
	s = strings.ToUpper(strings.NewReplacer(" ", "", "-", "").Replace(s))
	if len(s) != UserCodeLength {
		return "", false
	}
	for _, c := range s {
		if !strings.ContainsRune(userCodeAlphabet, c) {
			return "", false
		}
	}
	return s, true
}

// FormatUserCode shows a stored user code as two groups of four, XXXX-XXXX.
func FormatUserCode(code string) string {
	if len(code) != UserCodeLength {
		return code
	}
	return code[:4] + "-" + code[4:]
}
