package deploy

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testGKERegistry = "asia-northeast3-docker.pkg.dev/project-b47cdba3-db5b-42a2-9a6/hopcharlie-apps"
const testGKECredentialConfig = `{"type":"external_account","credential_source":{"environment_id":"aws1"}}`

func TestFromEnvGKEImages(t *testing.T) {
	// Only test credentials and a local, unused Kubernetes endpoint are loaded.
	dir := t.TempDir()
	config := filepath.Join(dir, "kubeconfig")
	if err := os.WriteFile(config, []byte(`apiVersion: v1
kind: Config
clusters:
- name: test
  cluster:
    server: https://127.0.0.1:1
contexts:
- name: test
  context:
    cluster: test
    user: test
users:
- name: test
  user:
    token: test-only
current-context: test
`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", config)
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(dir, "no-aws-config"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(dir, "no-aws-credentials"))
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_ACCESS_KEY_ID", "test-only")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test-only")
	t.Setenv("AWS_SESSION_TOKEN", "")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	for _, tc := range []struct {
		name, builder, registry, credentials, wantErr string
	}{
		{name: "legacy ECR only", builder: "codebuild"},
		{name: "infra PR 56 pair", builder: "codebuild", registry: testGKERegistry, credentials: testGKECredentialConfig},
		{name: "registry only", builder: "codebuild", registry: testGKERegistry, wantErr: "together"},
		{name: "credentials only", builder: "codebuild", credentials: testGKECredentialConfig, wantErr: "together"},
		{name: "invalid JSON", builder: "codebuild", registry: testGKERegistry, credentials: "not-json-sensitive-marker", wantErr: "PLATFORM_GKE_IMAGE_CREDENTIAL_CONFIG"},
		{name: "service account key rejected", builder: "codebuild", registry: testGKERegistry, credentials: `{"type":"service_account","private_key":"sensitive-marker"}`, wantErr: "aws1"},
		{name: "API GCS ADC rejected", builder: "codebuild", registry: testGKERegistry, credentials: `{"type":"external_account","credential_source":{"file":"/var/run/secrets/gcp/token"}}`, wantErr: "aws1"},
		{name: "Docker builder rejected", builder: "docker", registry: testGKERegistry, credentials: testGKECredentialConfig, wantErr: "codebuild"},
		{name: "other registry rejected", builder: "codebuild", registry: "attacker.example/project/repo", credentials: testGKECredentialConfig, wantErr: "Artifact Registry"},
		{name: "userinfo rejected", builder: "codebuild", registry: "attacker.example@asia-northeast3-docker.pkg.dev/project/repo", credentials: testGKECredentialConfig, wantErr: "Artifact Registry"},
		{name: "path traversal rejected", builder: "codebuild", registry: "asia-northeast3-docker.pkg.dev/project/..", credentials: testGKECredentialConfig, wantErr: "Artifact Registry"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			values := map[string]string{
				"PLATFORM_DEPLOY_BUILDER":              tc.builder,
				"PLATFORM_CODEBUILD_PROJECT":           "hopcharlie-app-build",
				"PLATFORM_IMAGE_REGISTRY":              "registry.test",
				"PLATFORM_ARGOCD_KUBECONTEXT":          "test",
				"PLATFORM_GKE_IMAGE_REGISTRY":          tc.registry,
				"PLATFORM_GKE_IMAGE_CREDENTIAL_CONFIG": tc.credentials,
			}
			te := newTestEnv(t)
			ex, err := FromEnv(context.Background(), func(key string) string { return values[key] }, EnvOptions{Store: te.store, GitHub: te.github})
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error %v, want %q", err, tc.wantErr)
				}
				if strings.Contains(err.Error(), "sensitive-marker") {
					t.Fatal("configuration error exposed input credentials")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			builder, ok := ex.cfg.Builder.(*CodeBuildBuilder)
			if !ok || builder.GKERegistry != tc.registry || builder.GKECredentialConfig != tc.credentials || ex.cfg.GKEImageRegistry != tc.registry {
				t.Fatal("GKE image configuration was not passed to the builder and executor")
			}
		})
	}
}
