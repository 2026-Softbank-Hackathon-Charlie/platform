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
  - name: onprem
    provider: onprem
    kubeContext: k3s
    storage:
      driver: minio
      bucketPrefix: hopcharlie-op-
      endpoint: http://minio.minio.svc:9000
      adminAccessKeyEnv: MINIO_ADMIN_ACCESS_KEY
      adminSecretKeyEnv: MINIO_ADMIN_SECRET_KEY
  - name: gke
    provider: gcp
    kubeContext: gke
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	eks, onprem, gke := cfg.Environments[0], cfg.Environments[1], cfg.Environments[2]
	if eks.SealedSecretsCert != "/config/sealed-secrets/eks.pem" || eks.Storage == nil || eks.Storage.Driver != "aws" ||
		eks.Storage.PermissionsBoundary != "arn:aws:iam::123456789012:policy/hopcharlie-app-boundary" {
		t.Fatalf("eks %+v %+v", eks, eks.Storage)
	}
	if onprem.SealedSecretsCert != "" || onprem.Storage == nil || onprem.Storage.Endpoint != "http://minio.minio.svc:9000" || onprem.Storage.AdminSecretKeyEnv != "MINIO_ADMIN_SECRET_KEY" {
		t.Fatalf("onprem %+v %+v", onprem, onprem.Storage)
	}
	if gke.Storage != nil {
		t.Fatalf("gke storage %+v", gke.Storage)
	}
}

func TestLoadConfigStorageInvalid(t *testing.T) {
	base := "environments:\n  - name: eks\n    provider: aws\n    kubeContext: eks\n    storage:\n"
	cases := map[string]struct{ storage, want string }{
		"gcs":           {"      driver: gcs\n      bucketPrefix: hc-\n", `environment "eks": storage driver gcs is not implemented`},
		"unknown field": {"      driver: aws\n      bucket: x\n", "unknown field"},
		"long prefix":   {"      driver: minio\n      bucketPrefix: hopcharlie-onprem-\n", "longer than 15"},
		"aws missing":   {"      driver: aws\n      bucketPrefix: hc-\n", "region is required"},
	}
	for name, tc := range cases {
		_, err := LoadConfig(writeFile(t, "envs.yaml", base+tc.storage))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want it to mention %q", name, err, tc.want)
		}
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
	if drivers["eks"] != "aws" || drivers["onprem"] != "minio" || drivers["gke"] != "" {
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

	// A driver that cannot be built stops New.
	_, err = New(cfg, Options{NewStorage: func(storage.Config) (storage.Driver, error) { return nil, errors.New("admin key not set") }})
	if err == nil || !strings.Contains(err.Error(), `environment "eks": admin key not set`) {
		t.Fatalf("New with a broken driver: %v", err)
	}
}

func TestServiceStorageDefaultDriver(t *testing.T) {
	cfg := Config{Environments: []EnvConfig{{Name: "onprem", Provider: "onprem", KubeContext: "k3s", Storage: &storage.Config{
		Driver: "minio", BucketPrefix: "hc-", Endpoint: "http://127.0.0.1:9", AdminAccessKeyEnv: "HC_TEST_MINIO_AK", AdminSecretKeyEnv: "HC_TEST_MINIO_SK"}}}}
	if _, err := New(cfg, Options{}); err == nil || !strings.Contains(err.Error(), "HC_TEST_MINIO_AK") {
		t.Fatalf("without the admin key: %v", err)
	}
	t.Setenv("HC_TEST_MINIO_AK", "admin")
	t.Setenv("HC_TEST_MINIO_SK", "secret-key")
	s, err := New(cfg, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if d, err := s.Storage("onprem"); err != nil || d.Name() != "minio" || d.Bucket("moimroom") != "hc-moimroom" {
		t.Fatalf("Storage(onprem) = %v, %v", d, err)
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
	if err := os.Remove(certPath); err != nil {
		t.Fatal(err)
	}
	if again, err := s.Sealer("eks"); err != nil || again != first {
		t.Fatalf("kept sealer: %v, %v", again, err)
	}
	if _, err := s.Sealer("gke"); !errors.Is(err, ErrSealingNotConfigured) {
		t.Fatalf("Sealer(gke) err = %v", err)
	}
	if _, err := s.Sealer("aks"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Sealer(aks) err = %v", err)
	}
}
