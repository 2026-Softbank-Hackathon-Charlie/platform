package apps

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/envs"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

func exampleCatalog(t *testing.T, gatewayURL string) *Catalog {
	t.Helper()
	envCfg, err := envs.LoadConfig(filepath.Join("..", "..", "examples", "envs.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(filepath.Join("..", "..", "examples", "apps.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	c, err := New(cfg, envCfg.Environments, gatewayURL)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestExampleFileLoads(t *testing.T) {
	c := exampleCatalog(t, "")
	want := []string{"EKS_MOIMROOM_PGPASSWORD", "GKE_MOIMROOM_PGPASSWORD", "ONPREM_MOIMROOM_PGPASSWORD"}
	if got := c.PasswordEnvs(); !reflect.DeepEqual(got, want) {
		t.Fatalf("PasswordEnvs = %v, want %v", got, want)
	}
	list := c.List()
	if len(list) != 1 || list[0].Name != "moimroom" || list[0].Database.Schemas[0] != "app" {
		t.Fatalf("apps = %+v", list)
	}
	var envNames []string
	for _, e := range list[0].Environments {
		envNames = append(envNames, e.Env+"="+e.DbHost)
	}
	// Environments-file order, default port filled in.
	wantEnvs := []string{
		"gke=moimroom-db-gke-rw.moimroom.gke.ts.net:5432",
		"eks=moimroom-db-eks-rw.moimroom.eks.ts.net:5432",
		"onprem=moimroom-db-onprem-rw.moimroom.onprem.ts.net:5432",
	}
	if !reflect.DeepEqual(envNames, wantEnvs) {
		t.Fatalf("environments = %v, want %v", envNames, wantEnvs)
	}
	b, _ := json.Marshal(list)
	if strings.Contains(string(b), "PGPASSWORD") {
		t.Fatalf("the app list names password variables: %s", b)
	}
}

func TestDraftFromExample(t *testing.T) {
	c := exampleCatalog(t, "http://gateway.ts.net:9901")
	p, err := c.Draft(apiv1.PlanDraftRequest{App: "moimroom", Source: "gke", Target: "eks", WrittenRecordsStream: ptr("moimroom-demo")})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := json.Marshal(p)
	want := `{"files":{"source":"gcs:moimroom-photos-gke","target":"s3:moimroom-photos-eks"},` +
		`"gateway":{"mode":"http","sourceBackend":"gke","targetBackend":"eks","token":"${GATEWAY_TOKEN}","url":"http://gateway.ts.net:9901"},` +
		`"id":"moimroom-gke-to-eks",` +
		`"source":{"cluster":"moimroom-db-gke","context":"gke-hopcharlie","dsn":"postgres://app:${GKE_MOIMROOM_PGPASSWORD}@moimroom-db-gke-rw.moimroom.gke.ts.net:5432/app?sslmode=require","env":"gke","namespace":"moimroom"},` +
		`"target":{"cluster":"moimroom-db-eks","context":"eks-hopcharlie","dsn":"postgres://app:${EKS_MOIMROOM_PGPASSWORD}@moimroom-db-eks-rw.moimroom.eks.ts.net:5432/app?sslmode=require","env":"eks","namespace":"moimroom"},` +
		`"verify":{"schemas":["app"],"writtenRecords":"stream:moimroom-demo"}}`
	if string(got) != want {
		t.Fatalf("draft:\n%s\nwant:\n%s", got, want)
	}

	// files: false, and no gateway on the server.
	c = exampleCatalog(t, "")
	p, err = c.Draft(apiv1.PlanDraftRequest{App: "moimroom", Source: "onprem", Target: "gke", Files: ptr(false)})
	if err != nil {
		t.Fatal(err)
	}
	if p.Files != nil || *p.Gateway.Mode != apiv1.EnginePlanGatewayModeNone || p.Gateway.Url != nil || p.Verify.WrittenRecords != nil {
		t.Fatalf("draft without files and gateway: %+v %+v %+v", p.Files, p.Gateway, p.Verify)
	}
}

func TestDraftRejects(t *testing.T) {
	c := exampleCatalog(t, "")
	cases := map[string]struct {
		req  apiv1.PlanDraftRequest
		want string
	}{
		"unknown app":        {apiv1.PlanDraftRequest{App: "shop", Source: "gke", Target: "eks"}, `unknown app "shop"`},
		"unknown env":        {apiv1.PlanDraftRequest{App: "moimroom", Source: "gke", Target: "aks"}, `unknown target environment "aks"`},
		"same env":           {apiv1.PlanDraftRequest{App: "moimroom", Source: "eks", Target: "eks"}, `both "eks"`},
		"bad stream":         {apiv1.PlanDraftRequest{App: "moimroom", Source: "gke", Target: "eks", WrittenRecordsStream: ptr("Bad_Name")}, "writtenRecordsStream"},
		"files without both": {apiv1.PlanDraftRequest{App: "solo", Source: "gke", Target: "eks", Files: ptr(true)}, "files needs a bucket"},
		"app not in env":     {apiv1.PlanDraftRequest{App: "solo", Source: "gke", Target: "onprem"}, `not configured in environment "onprem"`},
	}
	c.apps = append(c.apps, App{Name: "solo", Namespace: "solo", Database: Database{Name: "db", User: "u"}, Environments: map[string]Environment{
		"gke": {CnpgCluster: "solo-gke", DBHost: "solo-gke", DBPort: 5432, PasswordEnv: "SOLO_GKE", Bucket: "gcs:solo"},
		"eks": {CnpgCluster: "solo-eks", DBHost: "solo-eks", DBPort: 5432, PasswordEnv: "SOLO_EKS"},
	}})
	for name, tc := range cases {
		_, err := c.Draft(tc.req)
		if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
	// Without an explicit files: true, a missing bucket just leaves files out.
	p, err := c.Draft(apiv1.PlanDraftRequest{App: "solo", Source: "gke", Target: "eks"})
	if err != nil || p.Files != nil {
		t.Fatalf("draft = %+v, %v", p.Files, err)
	}
}

func TestConfigRejects(t *testing.T) {
	environments := []envs.EnvConfig{{Name: "gke"}, {Name: "eks"}}
	ok := func() App {
		return App{Name: "moimroom", Namespace: "moimroom", Database: Database{Name: "app", User: "app"},
			Environments: map[string]Environment{"gke": {CnpgCluster: "db", DBHost: "db.ts.net", PasswordEnv: "PW"}}}
	}
	cases := map[string]struct {
		edit func(*Config)
		want string
	}{
		"no apps":     {func(c *Config) { c.Apps = nil }, "no apps"},
		"bad name":    {func(c *Config) { c.Apps[0].Name = "Moim_Room" }, "name"},
		"duplicate":   {func(c *Config) { c.Apps = append(c.Apps, ok()) }, "duplicate"},
		"unknown env": {func(c *Config) { c.Apps[0].Environments["aks"] = c.Apps[0].Environments["gke"] }, `environment "aks"`},
		"bad host": {func(c *Config) {
			e := c.Apps[0].Environments["gke"]
			e.DBHost = "db/x@evil"
			c.Apps[0].Environments["gke"] = e
		}, "dbHost"},
		"bad password": {func(c *Config) {
			e := c.Apps[0].Environments["gke"]
			e.PasswordEnv = "PW}"
			c.Apps[0].Environments["gke"] = e
		}, "passwordEnv"},
		"bad user": {func(c *Config) { c.Apps[0].Database.User = "app:x@" }, "database"},
		"no envs":  {func(c *Config) { c.Apps[0].Environments = nil }, "no environments"},
	}
	for name, tc := range cases {
		cfg := Config{Apps: []App{ok()}}
		tc.edit(&cfg)
		if _, err := New(cfg, environments, ""); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "apps.yaml")
	if err := os.WriteFile(path, []byte("apps:\n  - name: x\n    namspace: typo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), "namspace") {
		t.Errorf("unknown field: err = %v", err)
	}
}
