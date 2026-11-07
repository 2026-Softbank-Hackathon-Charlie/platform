package apps

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/envs"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/plans"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/secrets"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/storage"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store/storetest"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

var fileEnvs = []envs.EnvConfig{
	{Name: "gke", Storage: &storage.Config{Driver: storage.DriverGCS, BucketPrefix: "hopcharlie-gke-", Region: "asia-northeast3", Project: "my-project-1"}},
	{Name: "eks", Storage: &storage.Config{Driver: storage.DriverAWS, BucketPrefix: "hopcharlie-eks-", Region: "ap-northeast-2"}},
	{Name: "bare"},
}

func TestEnableFiles(t *testing.T) {
	set := map[string]string{}
	have := map[string]string{}
	lookup := func(k string) (string, bool) { v, ok := have[k]; return v, ok }
	setenv := func(k, v string) error { set[k] = v; return nil }

	for _, off := range []string{"", "0"} {
		if f, err := EnableFiles(off, fileEnvs, lookup, setenv); f != nil || err != nil || len(set) != 0 {
			t.Fatalf("%q: %v %v, set %v", off, f, err, set)
		}
	}
	if _, err := EnableFiles("yes", fileEnvs, lookup, setenv); err == nil {
		t.Fatal("yes: no error")
	}
	if _, err := EnableFiles("1", fileEnvs[2:], lookup, setenv); err == nil {
		t.Fatal("no supported storage environment: no error")
	}

	f, err := EnableFiles("1", fileEnvs, lookup, setenv)
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]string{"gke": "hopcharlie-gke-", "eks": "hopcharlie-eks-"}; !reflect.DeepEqual(f.BucketPrefixes, want) {
		t.Errorf("BucketPrefixes = %v", f.BucketPrefixes)
	}
	want := map[string]string{
		"RCLONE_CONFIG_GKE_TYPE":               "google cloud storage",
		"RCLONE_CONFIG_GKE_ENV_AUTH":           "true",
		"RCLONE_CONFIG_GKE_BUCKET_POLICY_ONLY": "true",
		"RCLONE_CONFIG_GKE_NO_CHECK_BUCKET":    "true",
		"RCLONE_CONFIG_EKS_TYPE":               "s3",
		"RCLONE_CONFIG_EKS_PROVIDER":           "AWS",
		"RCLONE_CONFIG_EKS_ENV_AUTH":           "true",
		"RCLONE_CONFIG_EKS_REGION":             "ap-northeast-2",
		"RCLONE_CONFIG_EKS_NO_CHECK_BUCKET":    "true",
	}
	if !reflect.DeepEqual(set, want) {
		t.Errorf("set %v", set)
	}
	if !reflect.DeepEqual(f.Set, []string{"gke", "eks"}) || len(f.Kept) != 0 {
		t.Errorf("Set %v Kept %v", f.Set, f.Kept)
	}

	// A remote the server already has (rclone-env) is left alone.
	clear(set)
	have["RCLONE_CONFIG_EKS_TYPE"] = "s3"
	f, err = EnableFiles("1", fileEnvs, lookup, setenv)
	if err != nil {
		t.Fatal(err)
	}
	for k := range set {
		if strings.HasPrefix(k, "RCLONE_CONFIG_EKS_") {
			t.Errorf("%s set over the server's own remote", k)
		}
	}
	if !reflect.DeepEqual(f.Set, []string{"gke"}) || !reflect.DeepEqual(f.Kept, []string{"eks"}) || f.BucketPrefixes["eks"] != "hopcharlie-eks-" {
		t.Errorf("Set %v Kept %v prefixes %v", f.Set, f.Kept, f.BucketPrefixes)
	}

	if _, err := EnableFiles("1", fileEnvs, lookup, func(string, string) error { return errors.New("no") }); err == nil {
		t.Error("setenv failure: no error")
	}
}

func minioFileEnv() envs.EnvConfig {
	return envs.EnvConfig{Name: "onprem", Storage: &storage.Config{
		Driver: storage.DriverMinIO, BucketPrefix: "hopcharlie-op-", Endpoint: "https://minio.internal:9000",
		Region: "onprem-1", AdminAccessKeyEnv: "MINIO_ADMIN_ACCESS", AdminSecretKeyEnv: "MINIO_ADMIN_SECRET",
	}}
}

func TestEnableFilesMinIO(t *testing.T) {
	e := minioFileEnv()
	have := map[string]string{"MINIO_ADMIN_ACCESS": "test-access", "MINIO_ADMIN_SECRET": "test-secret"}
	lookup := func(k string) (string, bool) { v, ok := have[k]; return v, ok }
	set := map[string]string{}
	f, err := EnableFiles("1", append(fileEnvs, e), lookup, func(k, v string) error { set[k] = v; return nil })
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"TYPE": "s3", "PROVIDER": "Minio", "ENV_AUTH": "false", "REGION": "onprem-1",
		"ENDPOINT": "https://minio.internal:9000", "FORCE_PATH_STYLE": "true", "NO_CHECK_BUCKET": "true",
		"ACCESS_KEY_ID": "test-access", "SECRET_ACCESS_KEY": "test-secret",
	}
	for k, v := range want {
		if set["RCLONE_CONFIG_ONPREM_"+k] != v {
			t.Errorf("wrong MinIO variable %s", k)
		}
	}
	for k := range set {
		if !strings.HasPrefix(k, "RCLONE_CONFIG_") {
			t.Errorf("unexpected global variable %s", k)
		}
	}
	if f.BucketPrefixes["onprem"] != e.Storage.BucketPrefix || !reflect.DeepEqual(f.Set, []string{"gke", "eks", "onprem"}) {
		t.Fatalf("files metadata %+v", f)
	}
	// Public metadata and the returned Files struct contain no key values.
	for _, v := range RemoteEnv(e) {
		if v == "test-access" || v == "test-secret" {
			t.Fatal("RemoteEnv contains a key")
		}
	}
	raw, _ := json.Marshal(f)
	if strings.Contains(string(raw), "test-secret") || strings.Contains(string(raw), "test-access") {
		t.Fatal("Files metadata contains a key")
	}
	r := &Registry{BucketPrefixes: f.BucketPrefixes}
	if got := r.fileBucket("onprem", "mig"); got != "onprem:hopcharlie-op-mig" {
		t.Fatalf("onprem bucket %q", got)
	}
}

func TestEnableFilesMinIOInvalidConfigFailsBeforeAnySet(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*envs.EnvConfig, map[string]string)
	}{
		{"missing endpoint", func(e *envs.EnvConfig, _ map[string]string) { e.Storage.Endpoint = "" }},
		{"bad endpoint", func(e *envs.EnvConfig, _ map[string]string) { e.Storage.Endpoint = "https://test-secret@[bad" }},
		{"userinfo", func(e *envs.EnvConfig, _ map[string]string) {
			e.Storage.Endpoint = "https://test-secret@minio.internal"
		}},
		{"query", func(e *envs.EnvConfig, _ map[string]string) { e.Storage.Endpoint += "?token=test-secret" }},
		{"fragment", func(e *envs.EnvConfig, _ map[string]string) { e.Storage.Endpoint += "#test-secret" }},
		{"path", func(e *envs.EnvConfig, _ map[string]string) { e.Storage.Endpoint += "/test-secret" }},
		{"bad scheme", func(e *envs.EnvConfig, _ map[string]string) { e.Storage.Endpoint = "file:///test-secret" }},
		{"missing prefix", func(e *envs.EnvConfig, _ map[string]string) { e.Storage.BucketPrefix = "" }},
		{"path prefix", func(e *envs.EnvConfig, _ map[string]string) { e.Storage.BucketPrefix = "a/path-" }},
		{"uppercase prefix", func(e *envs.EnvConfig, _ map[string]string) { e.Storage.BucketPrefix = "Bad-" }},
		{"bad reference", func(e *envs.EnvConfig, _ map[string]string) { e.Storage.AdminSecretKeyEnv = "test-secret" }},
		{"missing credential", func(_ *envs.EnvConfig, have map[string]string) { delete(have, "MINIO_ADMIN_ACCESS") }},
		{"blank credential", func(_ *envs.EnvConfig, have map[string]string) { have["MINIO_ADMIN_SECRET"] = " \t" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := minioFileEnv()
			have := map[string]string{"MINIO_ADMIN_ACCESS": "test-access", "MINIO_ADMIN_SECRET": "test-secret"}
			tc.change(&e, have)
			sets := 0
			f, err := EnableFiles("1", append(fileEnvs, e), func(k string) (string, bool) { v, ok := have[k]; return v, ok }, func(string, string) error { sets++; return nil })
			if f != nil || err == nil || sets != 0 {
				t.Fatalf("f=%v err=%v sets=%d", f, err, sets)
			}
			if strings.Contains(err.Error(), "test-secret") || strings.Contains(err.Error(), "test-access") {
				t.Fatal("error exposed input credentials")
			}
		})
	}
}

func TestEnableFilesPreservesExistingMinIORemote(t *testing.T) {
	e := minioFileEnv()
	e.Storage.Endpoint, e.Storage.AdminAccessKeyEnv, e.Storage.AdminSecretKeyEnv = "", "", ""
	lookups := []string{}
	f, err := EnableFiles("1", []envs.EnvConfig{e}, func(k string) (string, bool) {
		lookups = append(lookups, k)
		return "s3", k == "RCLONE_CONFIG_ONPREM_TYPE"
	}, func(string, string) error { t.Fatal("overwrote existing remote"); return nil })
	if err != nil || !reflect.DeepEqual(f.Kept, []string{"onprem"}) || !reflect.DeepEqual(lookups, []string{"RCLONE_CONFIG_ONPREM_TYPE"}) {
		t.Fatalf("f=%+v err=%v lookups=%v", f, err, lookups)
	}
}

func TestEnableFilesSetterErrorDoesNotExposeSecret(t *testing.T) {
	sets := 0
	_, err := EnableFiles("1", []envs.EnvConfig{minioFileEnv()}, func(k string) (string, bool) { return "test-secret", k != "RCLONE_CONFIG_ONPREM_TYPE" }, func(_, v string) error { sets++; return errors.New(v) })
	if err == nil || strings.Contains(err.Error(), "test-secret") {
		t.Fatalf("credential error was not redacted: %v", err)
	}
	if sets != 1 {
		t.Fatalf("setter was not tested: %d calls", sets)
	}
}

// fileFixture is a registry fixture whose registry moves files in gke and
// eks (PLATFORM_APP_FILES=1).
func newFileFixture(t *testing.T) *registryFixture {
	t.Helper()
	c, err := secrets.NewCipher(bytes.Repeat([]byte{3}, secrets.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	tmpls, err := ParseHostTemplates(testHostTemplates, []string{"gke", "eks", "onprem"})
	if err != nil {
		t.Fatal(err)
	}
	st := storetest.NewMemory()
	reg := &Registry{Store: st, Secrets: c, HostTemplates: tmpls, BucketPrefixes: map[string]string{"gke": "hopcharlie-gke-", "eks": "hopcharlie-eks-"}}
	cat := exampleCatalog(t, "http://gateway.ts.net:9901").WithRegistry(reg)
	return &registryFixture{t: t, cat: cat, store: st, cipher: c}
}

// registerResources registers app in gke and eks with a database pair and
// the resources given as name=type.
func (f *registryFixture) registerResources(app string, resources ...string) {
	f.t.Helper()
	var rs []map[string]string
	for _, r := range resources {
		name, typ, _ := strings.Cut(r, "=")
		rs = append(rs, map[string]string{"name": name, "type": typ})
	}
	spec, _ := json.Marshal(map[string]any{"name": app, "resources": rs})
	a := store.App{Name: app, Repo: "org/" + app, Branch: "main", Environments: []string{"gke", "eks"}, Spec: spec, CreatedAt: time.Now().UTC()}
	row, err := f.cipher.DatabaseRow(app, "gke", "eks", "pw", "CERT", "KEY")
	if err != nil {
		f.t.Fatal(err)
	}
	if err := f.store.CreateAppWithDatabase(f.t.Context(), a, row); err != nil {
		f.t.Fatal(err)
	}
}

func TestDraftMovesRegisteredAppFiles(t *testing.T) {
	f := newFileFixture(t)
	f.registerResources("mig", "db=postgres", "files=s3")
	f.registerResources("nofiles", "db=postgres")

	p, err := f.cat.Draft(t.Context(), apiv1.PlanDraftRequest{App: "mig", Source: "gke", Target: "eks"})
	if err != nil {
		t.Fatal(err)
	}
	if p.Files == nil || p.Files.Source != "gke:hopcharlie-gke-mig" || p.Files.Target != "eks:hopcharlie-eks-mig" {
		t.Fatalf("files %+v", p.Files)
	}
	if p.Files.Rclone != nil {
		t.Errorf("rclone %v: the engine's default is the server's rclone", *p.Files.Rclone)
	}
	no := false
	if p, err := f.cat.Draft(t.Context(), apiv1.PlanDraftRequest{App: "mig", Source: "gke", Target: "eks", Files: &no}); err != nil || p.Files != nil {
		t.Errorf("files false: %+v %v", p.Files, err)
	}
	// No s3 resource: no bucket, so no files, and asking for them fails.
	if p, err := f.cat.Draft(t.Context(), apiv1.PlanDraftRequest{App: "nofiles", Source: "gke", Target: "eks"}); err != nil || p.Files != nil {
		t.Errorf("no s3 resource: %+v %v", p.Files, err)
	}
	yes := true
	if _, err := f.cat.Draft(t.Context(), apiv1.PlanDraftRequest{App: "nofiles", Source: "gke", Target: "eks", Files: &yes}); !errors.Is(err, ErrInvalid) {
		t.Errorf("files true without a bucket: %v", err)
	}
	// The view shows the buckets.
	a, _, err := f.cat.Lookup(t.Context(), "mig")
	if err != nil || a.Environments[0].Bucket == nil || *a.Environments[0].Bucket != "gke:hopcharlie-gke-mig" {
		t.Fatalf("Lookup %+v %v", a.Environments, err)
	}

	// PLATFORM_APP_FILES off: the registry moves no files.
	off := newRegistryFixture(t)
	off.registerResources("mig", "db=postgres", "files=s3")
	if p, err := off.cat.Draft(t.Context(), apiv1.PlanDraftRequest{App: "mig", Source: "gke", Target: "eks"}); err != nil || p.Files != nil {
		t.Errorf("files off: %+v %v", p.Files, err)
	}
	if b, err := off.cat.RegistryBuckets(t.Context()); err != nil || len(b) != 0 {
		t.Errorf("files off: RegistryBuckets = %v %v", b, err)
	}
}

func TestRegistryBuckets(t *testing.T) {
	f := newFileFixture(t)
	f.registerResources("mig", "db=postgres", "files=s3")
	f.registerResources("moimroom", "db=postgres", "files=s3") // the file's moimroom wins
	got, err := f.cat.RegistryBuckets(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"gke:hopcharlie-gke-mig", "eks:hopcharlie-eks-mig"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("RegistryBuckets = %v", got)
	}
}

// TestRegisteredAppFilesPassThePolicy submits a drafted plan with files to
// a plan service that knew no bucket of the app when it started.
func TestRegisteredAppFilesPassThePolicy(t *testing.T) {
	f := newFileFixture(t)
	t.Setenv("GKE_MIG_PGPASSWORD", "")
	os.Unsetenv("GKE_MIG_PGPASSWORD")
	policy := plans.DefaultPolicy()
	policy.GatewayURL = "http://gateway.ts.net:9901"
	policy.HostPasswordEnvs, policy.AppDBHosts, policy.Buckets = f.cat.PasswordHosts(), f.cat.AppHosts(), f.cat.Buckets()
	svc := plans.New(policy, f.store)
	svc.SetAppDatabases(f.cat)

	f.registerResources("mig", "db=postgres", "files=s3")
	p, err := f.cat.Draft(t.Context(), apiv1.PlanDraftRequest{App: "mig", Source: "gke", Target: "eks"})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(p)
	if _, err := svc.Submit(t.Context(), raw, "op"); err != nil {
		t.Fatalf("submit the drafted plan: %v", err)
	}
	// Another remote or bucket is still refused.
	for _, bad := range []string{`"eks:hopcharlie-eks-other"`, `":s3,env_auth=true:hopcharlie-eks-mig"`} {
		changed := strings.Replace(string(raw), `"eks:hopcharlie-eks-mig"`, bad, 1)
		if _, err := svc.Submit(t.Context(), []byte(changed), "op"); !errors.Is(err, plans.ErrInvalid) {
			t.Errorf("%s: %v, want ErrInvalid", bad, err)
		}
	}
}
