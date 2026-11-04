//go:build !minio

package envs

import (
	"strings"
	"testing"
)

func TestLoadConfigMinIONotBuilt(t *testing.T) {
	_, err := LoadConfig(writeFile(t, "envs.yaml", `
environments:
  - name: onprem
    provider: onprem
    kubeContext: k3s
    storage:
      driver: minio
      bucketPrefix: hopcharlie-op-
      endpoint: http://minio.minio.svc:9000
      adminAccessKeyEnv: MINIO_ADMIN_ACCESS_KEY
      adminSecretKeyEnv: MINIO_ADMIN_SECRET_KEY
`))
	if err == nil || !strings.Contains(err.Error(), `environment "onprem"`) || !strings.Contains(err.Error(), "-tags minio") {
		t.Fatalf("a minio block in the default build: %v", err)
	}
}
