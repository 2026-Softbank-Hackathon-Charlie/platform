package storage

import (
	"encoding/json"
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
	for name, c := range map[string]Config{"aws": validAWS(), "minio": validMinIO()} {
		if err := c.Validate(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	m := validMinIO()
	m.Region, m.Endpoint = "us-east-1", "https://minio.example.com/"
	if err := m.Validate(); err != nil {
		t.Errorf("minio with region and https: %v", err)
	}

	cases := map[string]struct {
		c    func() Config
		want string
	}{
		"no driver":       {func() Config { c := validAWS(); c.Driver = ""; return c }, "must be aws or minio"},
		"gcs":             {func() Config { c := validAWS(); c.Driver = "gcs"; return c }, "not implemented"},
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

func TestNewMinIOReadsAdminKey(t *testing.T) {
	env := map[string]string{"MINIO_ADMIN_ACCESS_KEY": "admin", "MINIO_ADMIN_SECRET_KEY": "secret-key"}
	d, err := New(t.Context(), validMinIO(), func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if d.Name() != DriverMinIO || d.Bucket("moimroom--dev") != "hopcharlie-op-moimroom--dev" {
		t.Fatalf("driver %s, bucket %s", d.Name(), d.Bucket("moimroom--dev"))
	}
	delete(env, "MINIO_ADMIN_SECRET_KEY")
	_, err = New(t.Context(), validMinIO(), func(k string) string { return env[k] })
	if err == nil || !strings.Contains(err.Error(), "MINIO_ADMIN_SECRET_KEY") || strings.Contains(err.Error(), "admin\"") {
		t.Fatalf("missing secret key: %v", err)
	}
	if _, err := New(t.Context(), Config{Driver: "gcs", BucketPrefix: "x-"}, nil); err == nil {
		t.Fatal("gcs: no error")
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
