package storage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func validAWS() Config {
	return Config{Driver: DriverAWS, BucketPrefix: "hopcharlie-eks-", Region: "ap-northeast-2", IAMPath: "/hopcharlie-apps/",
		PermissionsBoundary: "arn:aws:iam::123456789012:policy/hopcharlie-app-boundary"}
}

func validMinIO() Config {
	return Config{Driver: DriverMinIO, BucketPrefix: "hopcharlie-op-", Endpoint: "http://minio.minio.svc:9000",
		AdminAccessKeyEnv: "MINIO_ADMIN_ACCESS_KEY", AdminSecretKeyEnv: "MINIO_ADMIN_SECRET_KEY"}
}

func TestConfigValidate(t *testing.T) {
	if err := validAWS().Validate(); err != nil {
		t.Errorf("aws: %v", err)
	}

	cases := map[string]struct {
		c    func() Config
		want string
	}{
		"no driver":       {func() Config { c := validAWS(); c.Driver = ""; return c }, "must be aws, minio or gcs"},
		"gcs no project":  {func() Config { c := validAWS(); c.Driver = "gcs"; return c }, "project"},
		"no prefix":       {func() Config { c := validAWS(); c.BucketPrefix = ""; return c }, "bucketPrefix is required"},
		"long prefix":     {func() Config { c := validAWS(); c.BucketPrefix = "hopcharlie-eks-x"; return c }, "longer than 15"},
		"upper prefix":    {func() Config { c := validAWS(); c.BucketPrefix = "HopCharlie-"; return c }, "lowercase"},
		"dot prefix":      {func() Config { c := validAWS(); c.BucketPrefix = "hop.charlie-"; return c }, "lowercase"},
		"dash first":      {func() Config { c := validAWS(); c.BucketPrefix = "-hop"; return c }, "lowercase"},
		"reserved prefix": {func() Config { c := validAWS(); c.BucketPrefix = "xn--hop-"; return c }, "reserves"},
		"bad region":      {func() Config { c := validAWS(); c.Region = "Seoul 1"; return c }, "not a region"},
		"aws no region":   {func() Config { c := validAWS(); c.Region = ""; return c }, "region is required"},
		"aws no path":     {func() Config { c := validAWS(); c.IAMPath = ""; return c }, "iamPath"},
		"aws bad path":    {func() Config { c := validAWS(); c.IAMPath = "hopcharlie-apps"; return c }, "iamPath"},
		"aws no boundary": {func() Config { c := validAWS(); c.PermissionsBoundary = ""; return c }, "permissionsBoundary"},
		"aws role arn": {func() Config {
			c := validAWS()
			c.PermissionsBoundary = "arn:aws:iam::123456789012:role/x"
			return c
		}, "permissionsBoundary"},
		"aws with endpoint": {func() Config { c := validAWS(); c.Endpoint = "http://x"; return c }, "endpoint is only for the minio driver"},
		"minio no endpoint": {func() Config { c := validMinIO(); c.Endpoint = ""; return c }, "endpoint"},
		"minio ftp":         {func() Config { c := validMinIO(); c.Endpoint = "ftp://minio:9000"; return c }, "endpoint"},
		"minio path":        {func() Config { c := validMinIO(); c.Endpoint = "http://minio:9000/s3"; return c }, "endpoint"},
		"minio userinfo":    {func() Config { c := validMinIO(); c.Endpoint = "http://a:b@minio:9000"; return c }, "endpoint"},
		"minio no key env":  {func() Config { c := validMinIO(); c.AdminAccessKeyEnv = ""; return c }, "adminAccessKeyEnv"},
		"minio bad key env": {func() Config { c := validMinIO(); c.AdminSecretKeyEnv = "MINIO-KEY"; return c }, "adminSecretKeyEnv"},
		"minio with path":   {func() Config { c := validMinIO(); c.IAMPath = "/x/"; return c }, "iamPath is only for the aws driver"},
	}
	for name, tc := range cases {
		err := tc.c().Validate()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want it to mention %q", name, err, tc.want)
		}
	}
}

func TestNewGCS(t *testing.T) {
	if _, err := New(t.Context(), Config{Driver: "gcs", BucketPrefix: "x-"}, nil); err == nil {
		t.Fatal("gcs: no error")
	}
}

func TestNewGCSWithADC(t *testing.T) {
	// Public WIF configuration; the token file is consumed only by API calls,
	// so constructing the driver must not fetch credentials or contact GCP.
	path := filepath.Join(t.TempDir(), "adc.json")
	raw, err := json.Marshal(map[string]any{
		"type":               "external_account",
		"audience":           "//iam.googleapis.com/projects/123456789012/locations/global/workloadIdentityPools/test/providers/eks",
		"subject_token_type": "urn:ietf:params:oauth:token-type:jwt",
		"token_url":          "https://sts.googleapis.com/v1/token",
		"credential_source":  map[string]string{"file": filepath.Join(t.TempDir(), "projected-token")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", path)
	c := Config{Driver: DriverGCS, BucketPrefix: "hopcharlie-gke-", Region: "asia-northeast3", Project: "example-project"}
	d, err := New(t.Context(), c, nil)
	if err != nil {
		t.Fatal(err)
	}
	if d.Name() != DriverGCS || d.Bucket("moimroom") != "hopcharlie-gke-moimroom" {
		t.Fatalf("driver %s, bucket %s", d.Name(), d.Bucket("moimroom"))
	}
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", filepath.Join(t.TempDir(), "missing-adc.json"))
	if _, err := New(t.Context(), c, nil); err == nil {
		t.Fatal("missing ADC accepted")
	}
}

func TestGCSConfig(t *testing.T) {
	base := map[string]string{"driver": "gcs", "bucketPrefix": "hopcharlie-gke-", "region": "asia-northeast3", "project": "example-project"}
	for name, tc := range map[string]struct{ field, value, want string }{
		"valid":             {},
		"missing project":   {"project", "", "project"},
		"numeric project":   {"project", "123456789012", "project"},
		"short project":     {"project", "short", "project"},
		"long project":      {"project", strings.Repeat("a", 31), "project"},
		"unsafe project":    {"project", "example/../other", "project"},
		"uppercase project": {"project", "Example-project", "project"},
		"trailing hyphen":   {"project", "example-project-", "project"},
		"missing region":    {"region", "", "region"},
		"multi-region":      {"region", "us", "single region"},
		"dual-region":       {"region", "nam4", "single region"},
		"aws path":          {"iamPath", "/apps/", "only for the aws driver"},
		"aws boundary":      {"permissionsBoundary", "arn:aws:iam::123456789012:policy/p", "only for the aws driver"},
		"minio endpoint":    {"endpoint", "http://minio:9000", "only for the minio driver"},
		"minio access key":  {"adminAccessKeyEnv", "MINIO_KEY", "only for the minio driver"},
		"minio secret key":  {"adminSecretKeyEnv", "MINIO_SECRET", "only for the minio driver"},
	} {
		t.Run(name, func(t *testing.T) {
			fields := make(map[string]string, len(base))
			for k, v := range base {
				fields[k] = v
			}
			if tc.field != "" {
				fields[tc.field] = tc.value
			}
			raw, _ := json.Marshal(fields)
			var c Config
			if err := json.Unmarshal(raw, &c); err != nil {
				t.Fatal(err)
			}
			err := c.Validate()
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				if c.Users() != "the GCP project example-project" {
					t.Fatalf("users = %q", c.Users())
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
	for _, c := range []Config{validAWS(), validMinIO()} {
		if err := json.Unmarshal([]byte(`{"project":"example-project"}`), &c); err != nil {
			t.Fatal(err)
		}
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "project is only for the gcs driver") {
			t.Errorf("%s with project: %v", c.Driver, err)
		}
	}
}

func TestNewAWS(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDEXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	d, err := New(t.Context(), validAWS(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if d.Name() != DriverAWS || d.Bucket("moimroom") != "hopcharlie-eks-moimroom" {
		t.Fatalf("driver %s, bucket %s", d.Name(), d.Bucket("moimroom"))
	}
}

func TestInstanceNames(t *testing.T) {
	n, err := instanceNames("hopcharlie-eks-", Instance{App: "moimroom", Name: "moimroom--dev"})
	if err != nil || n.bucket != "hopcharlie-eks-moimroom--dev" || n.user != "hopcharlie-app-moimroom--dev" {
		t.Fatalf("names %+v, %v", n, err)
	}
	// 15 + 48 = 63 is the longest bucket name.
	long := strings.Repeat("a", 48)
	if _, err := instanceNames("hopcharlie-eks-", Instance{App: "a", Name: long}); err != nil {
		t.Fatalf("48-character instance: %v", err)
	}
	// A short prefix leaves room for a longer bucket name, but IAM user
	// names stop at 64.
	if _, err := instanceNames("a-", Instance{App: "a", Name: strings.Repeat("a", 50)}); err == nil || !strings.Contains(err.Error(), "user name") {
		t.Fatalf("65-character user name: %v", err)
	}
	for name, in := range map[string]Instance{
		"too long":     {App: "a", Name: long + "b"},
		"no app":       {Name: "moimroom"},
		"no instance":  {App: "moimroom"},
		"upper":        {App: "a", Name: "Moimroom"},
		"reserved end": {App: "shop", Name: "shop--x-s3"},
	} {
		if _, err := instanceNames("hopcharlie-eks-", in); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestBucketPolicy(t *testing.T) {
	var doc struct {
		Version   string
		Statement []struct {
			Effect   string
			Action   []string
			Resource string
		}
	}
	if err := json.Unmarshal([]byte(bucketPolicy("arn:aws:s3:::hopcharlie-eks-moimroom")), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Version != "2012-10-17" || len(doc.Statement) != 2 {
		t.Fatalf("policy %+v", doc)
	}
	for _, s := range doc.Statement {
		if s.Effect != "Allow" || (s.Resource != "arn:aws:s3:::hopcharlie-eks-moimroom" && s.Resource != "arn:aws:s3:::hopcharlie-eks-moimroom/*") {
			t.Fatalf("statement %+v reaches beyond the bucket", s)
		}
		for _, a := range s.Action {
			if strings.Contains(a, "*") || !strings.HasPrefix(a, "s3:") || strings.Contains(a, "Bucket") && strings.Contains(a, "Delete") {
				t.Fatalf("action %s", a)
			}
		}
	}
	if partition("cn-north-1") != "aws-cn" || partition("us-gov-west-1") != "aws-us-gov" || partition("ap-northeast-2") != "aws" {
		t.Fatal("partition")
	}
}
