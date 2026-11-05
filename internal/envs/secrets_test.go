package envs

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io/fs"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/storage"
)

// certPEM is a self-signed certificate like the sealed-secrets controller's.
func certPEM(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "sealed-secret"},
		NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func TestLoadConfigStorageAndCert(t *testing.T) {
	path := writeFile(t, "envs.yaml", `
environments:
  - name: eks
    provider: aws
    kubeContext: eks
    sealedSecretsCert: /config/sealed-secrets/eks.pem
    storage:
      driver: aws
      bucketPrefix: hopcharlie-eks-
      region: ap-northeast-2
      iamPath: /hopcharlie-apps/
      permissionsBoundary: arn:aws:iam::123456789012:policy/hopcharlie-app-boundary
  - name: gke
    provider: gcp
    kubeContext: gke
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	eks, gke := cfg.Environments[0], cfg.Environments[1]
	if eks.SealedSecretsCert != "/config/sealed-secrets/eks.pem" || eks.Storage == nil || eks.Storage.Driver != "aws" ||
		eks.Storage.PermissionsBoundary != "arn:aws:iam::123456789012:policy/hopcharlie-app-boundary" {
		t.Fatalf("eks %+v %+v", eks, eks.Storage)
	}
	if gke.SealedSecretsCert != "" || gke.Storage != nil {
		t.Fatalf("gke storage %+v", gke.Storage)
	}
}

func TestLoadConfigStorageInvalid(t *testing.T) {
	base := "environments:\n  - name: eks\n    provider: aws\n    kubeContext: eks\n    storage:\n"
	cases := map[string]struct{ storage, want string }{
		"gcs no project": {"      driver: gcs\n      bucketPrefix: hc-\n", `environment "eks": storage project`},
		"unknown field":  {"      driver: aws\n      bucket: x\n", "unknown field"},
		"long prefix":    {"      driver: minio\n      bucketPrefix: hopcharlie-onprem-\n", "longer than 15"},
		"aws missing":    {"      driver: aws\n      bucketPrefix: hc-\n", "region is required"},
	}
	for name, tc := range cases {
		_, err := LoadConfig(writeFile(t, "envs.yaml", base+tc.storage))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want it to mention %q", name, err, tc.want)
		}
	}
}

// storageEnvs is an envs.yaml with environments a and b and their storage
// blocks.
func storageEnvs(a, b string) string {
	env := func(name, storage string) string {
		return "  - name: " + name + "\n    provider: onprem\n    kubeContext: " + name + "\n    storage:\n" + storage
	}
	return "environments:\n" + env("a", a) + env("b", b)
}

func awsStorage(prefix string) string {
	return "      driver: aws\n      bucketPrefix: " + prefix + "\n      region: ap-northeast-2\n      iamPath: /hc/\n      permissionsBoundary: arn:aws:iam::123456789012:policy/b\n"
}

// checkStorageUsers loads each case's envs.yaml; want is what the error
// mentions, or "" for none.
func checkStorageUsers(t *testing.T, cases map[string]struct{ yaml, want string }) {
	t.Helper()
	for name, tc := range cases {
		_, err := LoadConfig(writeFile(t, "envs.yaml", tc.yaml))
		if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
			t.Errorf("%s: err = %v, want it to mention %q", name, err, tc.want)
		}
	}
}

func TestLoadConfigStorageSharedUsers(t *testing.T) {
	checkStorageUsers(t, map[string]struct{ yaml, want string }{
		"two aws":                   {storageEnvs(awsStorage("hc-a-"), awsStorage("hc-b-")), `environments "a" and "b" both keep their storage users in the AWS account`},
		"two gcs in one project":    {storageEnvs(gcsStorage("hc-a-", "example-project"), gcsStorage("hc-b-", "example-project")), `both keep their storage users in the GCP project example-project`},
		"gcs in different projects": {storageEnvs(gcsStorage("hc-a-", "example-project-a"), gcsStorage("hc-b-", "example-project-b")), ""},
		"aws and gcs":               {storageEnvs(awsStorage("hc-a-"), gcsStorage("hc-b-", "example-project")), ""},
	})
}

func gcsStorage(prefix, project string) string {
	return "      driver: gcs\n      bucketPrefix: " + prefix + "\n      region: asia-northeast3\n      project: " + project + "\n"
}

func TestLoadGCSStorageConfig(t *testing.T) {
	path := writeFile(t, "envs.yaml", "environments:\n  - name: gke\n    provider: gcp\n    kubeContext: gke\n    sealedSecretsCert: /config/sealed-secrets/gke.pem\n    storage:\n"+gcsStorage("hopcharlie-gke-", "example-project"))
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	e := cfg.Environments[0]
	if e.Storage == nil || e.Storage.Driver != storage.DriverGCS || e.Storage.Project != "example-project" || e.SealedSecretsCert != "/config/sealed-secrets/gke.pem" {
		t.Fatalf("GKE configuration = %+v", e)
	}
}

func TestLoadExampleConfigStorage(t *testing.T) {
	cfg, err := LoadConfig(filepath.Join("..", "..", "examples", "envs.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	drivers := map[string]string{}
	for _, e := range cfg.Environments {
		if e.Storage != nil {
			drivers[e.Name] = e.Storage.Driver
		}
	}
	// The onprem minio block is commented out: it needs -tags minio.
	if drivers["eks"] != "aws" || drivers["onprem"] != "" || drivers["gke"] != "" {
		t.Fatalf("example storage drivers %v", drivers)
	}
}

// fakeDriver is a storage.Driver that only knows its config.
type fakeDriver struct {
	storage.Driver
	cfg storage.Config
}

func TestServiceStorage(t *testing.T) {
	aws := &storage.Config{Driver: "aws", BucketPrefix: "hopcharlie-eks-", Region: "ap-northeast-2", IAMPath: "/hopcharlie-apps/",
		PermissionsBoundary: "arn:aws:iam::123456789012:policy/b"}
	cfg := Config{Environments: []EnvConfig{
		{Name: "eks", Provider: "aws", KubeContext: "eks", Storage: aws},
		{Name: "gke", Provider: "gcp", KubeContext: "gke"},
	}}
	var built []string
	s, err := New(cfg, Options{NewStorage: func(c storage.Config) (storage.Driver, error) {
		built = append(built, c.BucketPrefix)
		return fakeDriver{cfg: c}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(built) != 1 || built[0] != "hopcharlie-eks-" {
		t.Fatalf("built drivers %v", built)
	}
	d, err := s.Storage("eks")
	if err != nil || d.(fakeDriver).cfg.Region != "ap-northeast-2" {
		t.Fatalf("Storage(eks) = %v, %v", d, err)
	}
	if _, err := s.Storage("gke"); !errors.Is(err, ErrStorageNotConfigured) {
		t.Fatalf("Storage(gke) err = %v", err)
	}
	if _, err := s.Storage("aks"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Storage(aks) err = %v", err)
	}

	// A driver that cannot be built does not stop New; Storage says why.
	s, err = New(cfg, Options{NewStorage: func(storage.Config) (storage.Driver, error) { return nil, errors.New("admin key not set") }})
	if err != nil {
		t.Fatalf("New with a broken driver: %v", err)
	}
	if d, err := s.Storage("eks"); d != nil || err == nil || !strings.Contains(err.Error(), `environment "eks": admin key not set`) {
		t.Fatalf("Storage(eks) with a broken driver = %v, %v", d, err)
	}
	if _, err := s.Storage("gke"); !errors.Is(err, ErrStorageNotConfigured) {
		t.Fatalf("Storage(gke) with a broken eks driver err = %v", err)
	}
}

func TestServiceStorageDefaultDriver(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDEXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	cfg := Config{Environments: []EnvConfig{{Name: "eks", Provider: "aws", KubeContext: "eks", Storage: &storage.Config{
		Driver: "aws", BucketPrefix: "hc-", Region: "ap-northeast-2", IAMPath: "/hopcharlie-apps/", PermissionsBoundary: "arn:aws:iam::123456789012:policy/b"}}}}
	s, err := New(cfg, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if d, err := s.Storage("eks"); err != nil || d.Name() != "aws" || d.Bucket("moimroom") != "hc-moimroom" {
		t.Fatalf("Storage(eks) = %v, %v", d, err)
	}
}

func TestServiceSealer(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "eks.pem")
	cfg := Config{Environments: []EnvConfig{
		{Name: "eks", Provider: "aws", KubeContext: "eks", SealedSecretsCert: certPath},
		{Name: "gke", Provider: "gcp", KubeContext: "gke"},
	}}
	s, err := New(cfg, Options{})
	if err != nil {
		t.Fatal(err)
	}
	// Missing: an error without the path, and New did not fail.
	_, err = s.Sealer("eks")
	if !errors.Is(err, fs.ErrNotExist) || strings.Contains(err.Error(), dir) {
		t.Fatalf("missing cert: %v", err)
	}
	// Not a certificate.
	if err := os.WriteFile(certPath, []byte("not pem"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Sealer("eks"); err == nil || strings.Contains(err.Error(), dir) {
		t.Fatalf("bad cert: %v", err)
	}
	// Added later: used without a restart, then kept.
	if err := os.WriteFile(certPath, certPEM(t), 0o600); err != nil {
		t.Fatal(err)
	}
	first, err := s.Sealer("eks")
	if err != nil || !strings.HasPrefix(first.Fingerprint(), "SHA256:") {
		t.Fatalf("cert added later: %v, %v", first, err)
	}
	if again, err := s.Sealer("eks"); err != nil || again != first {
		t.Fatalf("kept sealer: %v, %v", again, err)
	}
	// Touched without a change: the same sealer.
	later := time.Now().Add(time.Minute)
	if err := os.Chtimes(certPath, later, later); err != nil {
		t.Fatal(err)
	}
	if again, err := s.Sealer("eks"); err != nil || again != first {
		t.Fatalf("touched cert: %v, %v", again, err)
	}
	// Replaced: the new certificate is used.
	if err := os.WriteFile(certPath, certPEM(t), 0o600); err != nil {
		t.Fatal(err)
	}
	evenLater := later.Add(time.Minute)
	if err := os.Chtimes(certPath, evenLater, evenLater); err != nil {
		t.Fatal(err)
	}
	second, err := s.Sealer("eks")
	if err != nil || second == first || second.Fingerprint() == first.Fingerprint() {
		t.Fatalf("replaced cert: %v, %v", second, err)
	}
	// Removed: an error, not the old sealer.
	if err := os.Remove(certPath); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Sealer("eks"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("removed cert: %v", err)
	}
	if _, err := s.Sealer("gke"); !errors.Is(err, ErrSealingNotConfigured) {
		t.Fatalf("Sealer(gke) err = %v", err)
	}
	if _, err := s.Sealer("aks"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Sealer(aks) err = %v", err)
	}
}
