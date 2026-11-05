// Package sealing encrypts values for a cluster's Bitnami sealed-secrets
// controller (ADR 0016, ADR 0019). The platform knows only each controller's
// public certificate; the controller in the cluster decrypts the values into
// a Secret. Values are sealed with strict scope, so they decrypt only into
// the Secret of the namespace and name they were sealed for.
package sealing

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"time"

	sscrypto "github.com/bitnami-labs/sealed-secrets/pkg/crypto"
)

// The Secrets the app chart renders from SealedSecrets.
const (
	// S3Secret holds the instance's bucket key (ADR 0016): accessKey and secretKey.
	S3Secret = "app-s3"
	// AppSecret holds the app's secrets an instance gets (ADR 0019), by name.
	AppSecret = "app-secrets"
	// PGBackupSecret holds the key of the app's backup bucket (ADR 0025):
	// accessKey and secretKey.
	PGBackupSecret = "app-pgbackup"
)

// MinKeyBits is the smallest RSA key ParseCert accepts. The controller
// makes 4096-bit keys by default.
const MinKeyBits = 2048

// Sealer seals values with one controller's public key. It is safe for
// concurrent use.
type Sealer struct {
	pub         *rsa.PublicKey
	fingerprint string
}

// ParseCert reads the controller's PEM certificate, as `kubeseal
// --fetch-cert` prints it. A PEM public key (PKIX) is accepted too. It
// refuses a certificate that has expired and an RSA key shorter than
// MinKeyBits.
func ParseCert(data []byte) (*Sealer, error) { return parseCert(data, time.Now()) }

func parseCert(data []byte, now time.Time) (*Sealer, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("sealed-secrets certificate: no PEM block")
	}
	var key any
	switch block.Type {
	case "CERTIFICATE":
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("sealed-secrets certificate: %w", err)
		}
		if now.After(cert.NotAfter) {
			return nil, fmt.Errorf("sealed-secrets certificate expired at %s; fetch the controller's current one (kubeseal --fetch-cert)", cert.NotAfter.UTC().Format(time.RFC3339))
		}
		key = cert.PublicKey
	case "PUBLIC KEY":
		k, err := x509.ParsePKIXPublicKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("sealed-secrets public key: %w", err)
		}
		key = k
	default:
		return nil, fmt.Errorf("sealed-secrets certificate: PEM block is %q, want CERTIFICATE", block.Type)
	}
	pub, ok := key.(*rsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("sealed-secrets certificate: key is %T, want RSA", key)
	}
	if bits := pub.N.BitLen(); bits < MinKeyBits {
		return nil, fmt.Errorf("sealed-secrets certificate: RSA key has %d bits, at least %d are required", bits, MinKeyBits)
	}
	fp, err := sscrypto.PublicKeyFingerprint(pub)
	if err != nil {
		return nil, fmt.Errorf("sealed-secrets certificate: %w", err)
	}
	return &Sealer{pub: pub, fingerprint: fp}, nil
}

// LoadCert reads the controller's certificate from a file (ParseCert).
func LoadCert(path string) (*Sealer, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseCert(data)
}

// Fingerprint identifies the public key, in the controller's own format
// (sealed-secrets PublicKeyFingerprint: "SHA256:" and the base64 SHA-256 of
// the SSH form of the key). Values sealed with the same fingerprint decrypt
// with the same controller key.
func (s *Sealer) Fingerprint() string { return s.fingerprint }

// Seal encrypts value for the key of the Secret name in namespace, with
// strict scope, and returns it base64-encoded as a SealedSecret's
// spec.encryptedData holds it. Every call gives a different result.
func (s *Sealer) Seal(namespace, name string, value []byte) (string, error) {
	if namespace == "" || name == "" {
		return "", errors.New("sealing: namespace and name are required for strict scope")
	}
	out, err := sscrypto.HybridEncrypt(rand.Reader, s.pub, value, Label(namespace, name))
	if err != nil {
		return "", fmt.Errorf("sealing: %w", err)
	}
	return base64.StdEncoding.EncodeToString(out), nil
}

// SealAll seals every value of values for the Secret name in namespace
// (Seal) and returns them by the same keys.
func (s *Sealer) SealAll(namespace, name string, values map[string]string) (map[string]string, error) {
	out := make(map[string]string, len(values))
	for k, v := range values {
		sealed, err := s.Seal(namespace, name, []byte(v))
		if err != nil {
			return nil, err
		}
		out[k] = sealed
	}
	return out, nil
}

// Label is the encryption label of strict scope, "<namespace>/<name>", as
// sealed-secrets' EncryptionLabel makes it. The controller decrypts with the
// label of the SealedSecret it reads, so a value sealed for one Secret does
// not decrypt into another.
func Label(namespace, name string) []byte { return []byte(namespace + "/" + name) }
