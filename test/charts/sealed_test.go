package charts

import (
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	"sigs.k8s.io/yaml"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/deploy"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/appspec"
)

// TestPlatformSealedFile renders a gitops file as the platform writes it,
// with sealed values (ADR 0016, ADR 0019), through the chart with the eks
// profile: the contract between internal/deploy and the chart.
func TestPlatformSealedFile(t *testing.T) {
	spec, errs := appspec.Check([]byte(`name: moimroom
services:
  - name: web
    port: 8080
    public: true
    env: {LOG_LEVEL: info, HOST_CODE: "from platform.yaml"}
resources:
  - {name: photos, type: s3}
`))
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	content, err := deploy.Render(deploy.File{
		App: "moimroom", Instance: "moimroom--dev", Env: "eks", Commit: strings.Repeat("a", 40), Deployment: "dep_1", Spec: *spec,
		Images: map[string]string{"web": "r/hopcharlie/apps/moimroom/web@sha256:" + strings.Repeat("b", 64)},
		Sealed: &deploy.Sealed{
			S3:          &deploy.SealedS3{AccessKey: "AgBhY2Nlc3M=", SecretKey: "AgBzZWNyZXQ="},
			App:         map[string]string{"HOST_CODE": "AgBob3N0", "ADMIN_TOKEN": "AgBhZG1pbg=="},
			AppChecksum: strings.Repeat("c", 64),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Values map[string]any `json:"values"`
	}
	if err := yaml.Unmarshal(content, &file); err != nil {
		t.Fatal(err)
	}
	values, _ := yaml.Marshal(map[string]any{"values": file.Values})
	// The s3 part of the eks profile (deploy/argocd/ci/eks-values.yaml).
	profile := `profile:
  s3: {endpoint: https://s3.ap-northeast-2.amazonaws.com, region: ap-northeast-2, bucketPrefix: hopcharlie-eks-, credentialsSecret: app-s3}
`
	docs := mustRender(t, appChart, "moimroom--dev", writeValues(t, string(values)), writeValues(t, profile))
	for _, k := range []string{"SealedSecret/app-s3", "SealedSecret/app-secrets"} {
		if _, ok := docs[k]; !ok {
			t.Errorf("no %s; rendered %v", k, keys(docs))
		}
	}
	var d appsv1.Deployment
	decode(t, docs, "Deployment/moimroom-web", &d)
	env := envByName(d.Spec.Template.Spec.Containers[0])
	if env["S3_BUCKET"].Value != "hopcharlie-eks-moimroom--dev" || env["LOG_LEVEL"].Value != "info" {
		t.Errorf("env %v", env)
	}
	if _, ok := env["HOST_CODE"]; ok {
		t.Error("platform.yaml's HOST_CODE is in env; the secret must win")
	}
	if d.Spec.Template.Annotations["hopcharlie.dev/secrets-checksum"] != strings.Repeat("c", 64) {
		t.Errorf("pod annotations %v", d.Spec.Template.Annotations)
	}
}
