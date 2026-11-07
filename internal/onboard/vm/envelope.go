// Package vm runs the fixed SSM documents of ADR 0042 on a source VM
// (HopCharlie-Inspect, HopCharlie-Connect, HopCharlie-Disconnect, whose
// scripts are in infra terraform/platform/aws/onboarding) and reads what
// they print: the inspection document, reassembled from its chunks, and the
// values the VM encrypted for the platform.
//
// # The envelope
//
// The VM never prints a secret value in clear. Each run of a document gets
// a one-time RSA key pair (Key); the VM encrypts with its public key in a
// pinned openssl container, and Key.Open decrypts:
//
//	pass     = 64 random hex characters
//	body     = openssl enc -aes-256-cbc -pbkdf2 -iter 10000 -md sha256 -salt:
//	           "Salted__" || salt (8 bytes) || AES-256-CBC (PKCS#7) ciphertext,
//	           key || iv = PBKDF2-HMAC-SHA256(pass, salt, 10000, 32 + 16)
//	wrapped  = RSA-OAEP (SHA-256, MGF1 SHA-256) of pass || SHA-256(plaintext)
//	envelope = "hcenc1." || base64(wrapped) || "." || base64(body)
//
// The SHA-256 of the plaintext under the RSA layer catches a damaged or cut
// envelope. Nothing authenticates the sender (anyone with the public key can
// make an envelope); SSM's own authentication of the command output does.
// openssl enc has no AEAD mode, and the construction keeps every key off
// command lines on the VM (the passphrase is a file in a tmpfs).
package vm

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"strings"
)

// EnvelopePrefix starts every envelope of this construction.
const EnvelopePrefix = "hcenc1."

const (
	keyBits    = 2048
	pbkdf2Iter = 10000
	passLen    = 64 // hex characters
)

// Key is one run's RSA key pair. It is not stored: the private key lives
// only as long as the run.
type Key struct {
	priv *rsa.PrivateKey
}

// NewKey makes a key pair.
func NewKey() (*Key, error) {
	priv, err := rsa.GenerateKey(rand.Reader, keyBits)
	if err != nil {
		return nil, err
	}
	return &Key{priv: priv}, nil
}

// PublicKey is the public key as the documents take it: base64 (standard,
// padded) of its DER SubjectPublicKeyInfo.
func (k *Key) PublicKey() string {
	der, err := x509.MarshalPKIXPublicKey(&k.priv.PublicKey)
	if err != nil { // an RSA public key always marshals
		panic(err)
	}
	return base64.StdEncoding.EncodeToString(der)
}

// errEnvelope is every failure of Open: the errors never say more, and
// never contain any part of the plaintext.
var errEnvelope = errors.New("the VM's encrypted value does not decrypt with this run's key")

// Open decrypts an envelope.
func (k *Key) Open(envelope string) ([]byte, error) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(envelope), EnvelopePrefix)
	if !ok {
		return nil, errors.New("the VM's encrypted value is not an hcenc1 envelope")
	}
	w64, b64, ok := strings.Cut(rest, ".")
	if !ok {
		return nil, errEnvelope
	}
	wrapped, err1 := base64.StdEncoding.DecodeString(w64)
	body, err2 := base64.StdEncoding.DecodeString(b64)
	if err1 != nil || err2 != nil {
		return nil, errEnvelope
	}
	inner, err := rsa.DecryptOAEP(sha256.New(), nil, k.priv, wrapped, nil)
	if err != nil || len(inner) != passLen+sha256.Size {
		return nil, errEnvelope
	}
	pass, digest := inner[:passLen], inner[passLen:]
	if len(body) < 16+aes.BlockSize || !bytes.HasPrefix(body, []byte("Salted__")) || (len(body)-16)%aes.BlockSize != 0 {
		return nil, errEnvelope
	}
	salt, ct := body[8:16], body[16:]
	kiv, err := pbkdf2.Key(sha256.New, string(pass), salt, pbkdf2Iter, 32+aes.BlockSize)
	if err != nil {
		return nil, errEnvelope
	}
	block, err := aes.NewCipher(kiv[:32])
	if err != nil {
		return nil, errEnvelope
	}
	plain := make([]byte, len(ct))
	cipher.NewCBCDecrypter(block, kiv[32:]).CryptBlocks(plain, ct)
	n := int(plain[len(plain)-1])
	if n < 1 || n > aes.BlockSize {
		return nil, errEnvelope
	}
	for _, b := range plain[len(plain)-n:] {
		if int(b) != n {
			return nil, errEnvelope
		}
	}
	plain = plain[:len(plain)-n]
	sum := sha256.Sum256(plain)
	if subtle.ConstantTimeCompare(sum[:], digest) != 1 {
		return nil, errEnvelope
	}
	return plain, nil
}

// ParseKey reads a PKCS#8 DER RSA private key (tests keep one with a
// recorded run).
func ParseKey(der []byte) (*Key, error) {
	k, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return nil, err
	}
	priv, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("not an RSA key")
	}
	return &Key{priv: priv}, nil
}
