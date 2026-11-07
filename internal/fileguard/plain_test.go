package fileguard

import (
	"context"
	"slices"
	"strings"
	"testing"
)

// TestPlainEnvironment: an onboarding's copy keeps the server's remotes and
// cloud credentials (the target bucket's), adds the legacy remote, and
// never carries a database password or rclone's global flags.
func TestPlainEnvironment(t *testing.T) {
	for k, v := range map[string]string{
		"RCLONE_CONFIG_EKS_TYPE": "s3", "AWS_ROLE_ARN": "arn:x", "AWS_CONTAINER_CREDENTIALS_FULL_URI": "http://169.254.170.23/v1/credentials",
		"GOOGLE_APPLICATION_CREDENTIALS": "/var/run/gcp.json", "LEGACY_PGPASSWORD": "fence", "GKE_SHOP_PGPASSWORD": "pw", "RCLONE_DRY_RUN": "true",
	} {
		t.Setenv(k, v)
	}
	p := NewPlain([]string{"RCLONE_CONFIG_LEGACY_TYPE=s3", "RCLONE_CONFIG_LEGACY_ACCESS_KEY_ID=AKIA"})
	env := p.Environment()
	for _, want := range []string{"RCLONE_CONFIG_EKS_TYPE=s3", "AWS_ROLE_ARN=arn:x", "AWS_CONTAINER_CREDENTIALS_FULL_URI=http://169.254.170.23/v1/credentials",
		"GOOGLE_APPLICATION_CREDENTIALS=/var/run/gcp.json", "RCLONE_CONFIG_LEGACY_TYPE=s3", "RCLONE_CONFIG_LEGACY_ACCESS_KEY_ID=AKIA"} {
		if !slices.Contains(env, want) {
			t.Errorf("no %s", want)
		}
	}
	for _, v := range env {
		if strings.Contains(v, "PGPASSWORD") || strings.HasPrefix(v, "RCLONE_DRY_RUN=") {
			t.Errorf("carries %s", v)
		}
	}
	var args []string
	p.exec = func(_ context.Context, _ []string, a ...string) (string, error) {
		args = a
		return "2023/01/01 NOTICE: 3 differences found\n", nil
	}
	res, err := p.Check(context.Background(), "legacy:b", "eks:b")
	if err != nil || res.Differences != 3 || !slices.Contains(args, "--config") {
		t.Fatalf("check %+v %v %v", res, err, args)
	}
}
