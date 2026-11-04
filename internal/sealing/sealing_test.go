package sealing

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	ssv1alpha1 "github.com/bitnami-labs/sealed-secrets/pkg/apis/sealedsecrets/v1alpha1"
	sscrypto "github.com/bitnami-labs/sealed-secrets/pkg/crypto"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
)

// controllerKey is one key pair and certificate made the way the controller
// makes its own (sealed-secrets GeneratePrivateKeyAndCert). 2048 bits keeps
// the tests fast; the controller's default is 4096.
var controllerKey = sync.OnceValues(func() (*rsa.PrivateKey, []byte) {
	key, cert, err := sscrypto.GeneratePrivateKeyAndCert(2048, time.Hour, "sealed-secret")
	if err != nil {
		panic(err)
	}
	return key, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})
})

func newSealer(t *testing.T) (*Sealer, *rsa.PrivateKey) {
	t.Helper()
	key, certPEM := controllerKey()
	s, err := ParseCert(certPEM)
	if err != nil {
		t.Fatal(err)
	}
	return s, key
}

// unseal decrypts a SealedSecret the way the controller does: the
// controller's own Unseal, with the label of the object's namespace and
// name and its default (strict) scope.
func unseal(t *testing.T, key *rsa.PrivateKey, namespace, name string, data map[string]string) (map[string][]byte, error) {
	t.Helper()
	ss := &ssv1alpha1.SealedSecret{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec: ssv1alpha1.SealedSecretSpec{
			EncryptedData: data,
			Template:      ssv1alpha1.SecretTemplateSpec{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name}},
		},
	}
	scheme := runtime.NewScheme()
	if err := ssv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	fp, err := sscrypto.PublicKeyFingerprint(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	secret, err := ss.Unseal(serializer.NewCodecFactory(scheme), map[string]*rsa.PrivateKey{fp: key})
	if err != nil {
		return nil, err
	}
	return secret.Data, nil
}

func TestSealUnsealsWithTheController(t *testing.T) {
	s, key := newSealer(t)
	values := map[string]string{"HOST_CODE": "4821", "ADMIN_TOKEN": "t0k\nen with spaces", "EMPTY": ""}
	sealed, err := s.SealAll("moimroom--dev", AppSecret, values)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range sealed {
		if strings.Contains(v, values[k]) && values[k] != "" {
			t.Fatalf("%s: sealed value contains the plaintext", k)
		}
		if _, err := base64.StdEncoding.DecodeString(v); err != nil {
			t.Fatalf("%s: not standard base64: %v", k, err)
		}
	}
	got, err := unseal(t, key, "moimroom--dev", AppSecret, sealed)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(values) {
		t.Fatalf("unsealed %d keys, want %d", len(got), len(values))
	}
	for k, v := range values {
		if string(got[k]) != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}

func TestSealIsStrictScope(t *testing.T) {
	s, key := newSealer(t)
	sealed, err := s.SealAll("moimroom", S3Secret, map[string]string{"accessKey": "AKIAEXAMPLE", "secretKey": "secret"})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ namespace, name string }{
		{"moimroom--dev", S3Secret}, // another namespace
		{"moimroom", AppSecret},     // another Secret
		{"shop", "app-s3"},
	} {
		if _, err := unseal(t, key, c.namespace, c.name, sealed); err == nil {
			t.Errorf("a value sealed for moimroom/app-s3 decrypted as %s/%s", c.namespace, c.name)
		}
	}
	// Moving a value to another key of the same Secret still decrypts: the
	// label is the namespace and name, as the controller defines it.
	if _, err := unseal(t, key, "moimroom", S3Secret, map[string]string{"other": sealed["accessKey"]}); err != nil {
		t.Fatalf("same Secret, other key: %v", err)
	}
}

func TestSealWrongKeyFails(t *testing.T) {
	s, _ := newSealer(t)
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := s.SealAll("shop", AppSecret, map[string]string{"A": "b"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := unseal(t, other, "shop", AppSecret, sealed); err == nil {
		t.Fatal("another controller key decrypted the value")
	}
}

func TestSealDiffersEveryTime(t *testing.T) {
	s, _ := newSealer(t)
	a, err := s.Seal("shop", AppSecret, []byte("same"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Seal("shop", AppSecret, []byte("same"))
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("two seals of one value are equal")
	}
	if _, err := s.Seal("", AppSecret, []byte("x")); err == nil {
		t.Fatal("sealed without a namespace")
	}
	if _, err := s.Seal("shop", "", []byte("x")); err == nil {
		t.Fatal("sealed without a name")
	}
}

func TestLabelMatchesController(t *testing.T) {
	got := string(Label("moimroom--dev", AppSecret))
	want := string(ssv1alpha1.EncryptionLabel("moimroom--dev", AppSecret, ssv1alpha1.StrictScope))
	if got != want {
		t.Fatalf("Label = %q, controller's strict label = %q", got, want)
	}
}

func TestFingerprint(t *testing.T) {
	s, key := newSealer(t)
	want, err := sscrypto.PublicKeyFingerprint(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	if s.Fingerprint() != want || !strings.HasPrefix(want, "SHA256:") {
		t.Fatalf("fingerprint %q, want %q", s.Fingerprint(), want)
	}
	// The same key as a PEM public key has the same fingerprint.
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	p, err := ParseCert(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
	if err != nil || p.Fingerprint() != want {
		t.Fatalf("public key PEM: %v, fingerprint %q", err, p.Fingerprint())
	}
}

func TestParseCertErrors(t *testing.T) {
	ecKey := "-----BEGIN PUBLIC KEY-----\nMFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAEEVs/o5+uQbTjL3chynL4wXgUg2R9\nq9UU8I5mEovUf86QZ7kOBIjJwqnzD1omageEHWwHdBO6B+dFabmdT9POxg==\n-----END PUBLIC KEY-----\n"
	for name, data := range map[string]string{
		"empty":       "",
		"not pem":     "hello",
		"private key": "-----BEGIN RSA PRIVATE KEY-----\nAAAA\n-----END RSA PRIVATE KEY-----\n",
		"bad cert":    "-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n",
		"ec key":      ecKey,
	} {
		if _, err := ParseCert([]byte(data)); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestParseCertRefusesExpiredAndShortKeys(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	cert := func(key *rsa.PrivateKey, notAfter time.Time) []byte {
		tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "sealed-secret"},
			NotBefore: notAfter.Add(-2 * time.Hour), NotAfter: notAfter}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
		if err != nil {
			t.Fatal(err)
		}
		return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	}
	if _, err := ParseCert(cert(key, time.Now().Add(-time.Minute))); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expired: %v", err)
	}
	if _, err := ParseCert(cert(key, time.Now().Add(time.Hour))); err != nil {
		t.Fatalf("valid: %v", err)
	}
	short, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseCert(cert(short, time.Now().Add(time.Hour))); err == nil || !strings.Contains(err.Error(), "1024 bits") {
		t.Fatalf("1024-bit key: %v", err)
	}
	der, _ := x509.MarshalPKIXPublicKey(&short.PublicKey)
	if _, err := ParseCert(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})); err == nil {
		t.Fatal("1024-bit public key accepted")
	}
}

func TestLoadCert(t *testing.T) {
	_, certPEM := controllerKey()
	path := filepath.Join(t.TempDir(), "eks.pem")
	if err := os.WriteFile(path, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCert(path); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCert(filepath.Join(t.TempDir(), "missing.pem")); !os.IsNotExist(err) {
		t.Fatalf("missing file: %v", err)
	}
}
