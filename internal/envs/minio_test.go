//go:build minio

package envs

import (
	"strings"
	"testing"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/storage"
)

func TestLoadConfigMinIOSharedUsers(t *testing.T) {
	minio := func(prefix, endpoint string) string {
		return "      driver: minio\n      bucketPrefix: " + prefix + "\n      endpoint: " + endpoint + "\n      adminAccessKeyEnv: AK\n      adminSecretKeyEnv: SK\n"
	}
	checkStorageUsers(t, map[string]struct{ yaml, want string }{
		"one server":    {storageEnvs(minio("hc-a-", "http://minio:9000"), minio("hc-b-", "https://MinIO:9000/")), `environments "a" and "b" both keep their storage users in the MinIO server minio:9000`},
		"two servers":   {storageEnvs(minio("hc-a-", "http://minio-a:9000"), minio("hc-b-", "http://minio-b:9000")), ""},
		"aws and minio": {storageEnvs(awsStorage("hc-a-"), minio("hc-b-", "http://minio:9000")), ""},
	})
}

func TestServiceStorageMinIO(t *testing.T) {
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
