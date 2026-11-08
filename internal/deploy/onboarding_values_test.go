package deploy

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/gateway"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/secrets"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/appspec"
	"sigs.k8s.io/yaml"
)

// onboardingFile is an onboarded app's primary file while it is syncing
// (ADR 0042): born from the external source, cold.
func onboardingFile(t *testing.T) File {
	t.Helper()
	spec, errs := appspec.Check([]byte(pgSpec))
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	return File{App: "shop", Instance: "shop", Env: "eks", Commit: "c", Deployment: "dep_1", Spec: *spec,
		Images: map[string]string{"web": "r/web@sha256:aa"},
		Sealed: &Sealed{PGApp: map[string]string{"password": "Ag1"}, PGCA: map[string]string{"ca.crt": "Ag2", "ca.key": "Ag3"},
			PGLegacy: map[string]string{"password": "Ag4", "username": "Ag5"}},
		Postgres: &PostgresValues{Image: "ghcr.io/cloudnative-pg/postgresql:17.11-standard-bookworm@sha256:bb",
			Database: "moim", Owner: "moim", StorageSize: "2Gi", Parameters: map[string]string{"max_wal_senders": "10", "max_connections": "200"}},
		DBTopology: &DBTopology{Primary: "eks", Peer: "gke", PeerAbsent: true,
			External: &ExternalSource{Name: "legacy", Host: "hop-db-legacy-shop", User: "hopcharlie_replicator",
				PasswordSecret: "shop-db-legacy", SSLMode: "prefer"}},
		OnboardingCold: true}
}

// TestRenderOnboardingGolden: the values of an onboarding are where the
// contract (gitops.go) puts them.
func TestRenderOnboardingGolden(t *testing.T) {
	out, err := Render(onboardingFile(t))
	if err != nil {
		t.Fatal(err)
	}
	want := fileHeader + `app: "shop"
env: "eks"
namespace: "shop"
commit: "c"
deployment: "dep_1"
values:
  app: "shop"
  services:
    - name: "web"
      image: "r/web@sha256:aa"
      port: 8080
      healthCheck: "/healthz"
      public: true
      replicas: 0
  resources:
    - name: "db"
      type: "postgres"
  postgres:
    image: "ghcr.io/cloudnative-pg/postgresql:17.11-standard-bookworm@sha256:bb"
    database: "moim"
    owner: "moim"
    storageSize: "2Gi"
    parameters:
      "max_connections": "200"
      "max_wal_senders": "10"
  dbTopology:
    primary: "eks"
    peer: "gke"
    peerPresent: false
    external:
      name: "legacy"
      host: "hop-db-legacy-shop"
      user: "hopcharlie_replicator"
      passwordSecret: "shop-db-legacy"
      sslmode: "prefer"
  onboardingCold: true
  sealedSecrets:
    pgApp:
      "password": "Ag1"
    pgCA:
      "ca.crt": "Ag2"
      "ca.key": "Ag3"
    pgLegacy:
      "password": "Ag4"
      "username": "Ag5"
`
	if string(out) != want {
		t.Fatalf("rendered:\n%s\nwant:\n%s", out, want)
	}
	var parsed struct {
		Values map[string]any `json:"values"`
	}
	if err := yaml.Unmarshal(out, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.Values["onboardingCold"] != true {
		t.Fatalf("onboardingCold parses as %#v", parsed.Values["onboardingCold"])
	}
}

// TestRenderAfterCutover: once the cutover succeeded the file keeps the
// image, database and the legacy password Secret, and drops the external
// source and cold; the replicas are the spec's again.
func TestRenderAfterCutover(t *testing.T) {
	f := onboardingFile(t)
	f.DBTopology.External, f.OnboardingCold = nil, false
	out, err := Render(f)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, gone := range []string{"external:", "onboardingCold", "replicas: 0"} {
		if strings.Contains(s, gone) {
			t.Errorf("after the cutover the file still has %q:\n%s", gone, s)
		}
	}
	for _, kept := range []string{"replicas: 1\n", "  postgres:\n", "    pgLegacy:\n"} {
		if !strings.Contains(s, kept) {
			t.Errorf("after the cutover the file lost %q:\n%s", kept, s)
		}
	}
}

// TestRenderExternalOnlyInPrimary: the peer's file never has the source.
func TestRenderExternalOnlyInPrimary(t *testing.T) {
	f := onboardingFile(t)
	f.Env = "gke"
	f.DBTopology.PeerAbsent = false
	if _, err := Render(f); err == nil {
		t.Fatal("the peer's file rendered the external source")
	}
	f = onboardingFile(t)
	f.DBTopology.External.Host = ""
	if _, err := Render(f); err == nil {
		t.Fatal("an external source without its host rendered")
	}
}

// TestRenderEmptyPostgresValues: an empty values.postgres is left out, so
// a file of an app without them is byte for byte the same.
func TestRenderEmptyPostgresValues(t *testing.T) {
	f := onboardingFile(t)
	f.DBTopology.External, f.OnboardingCold, f.Sealed.PGLegacy = nil, false, nil
	f.Postgres = nil
	without, err := Render(f)
	if err != nil {
		t.Fatal(err)
	}
	f.Postgres = &PostgresValues{}
	empty, err := Render(f)
	if err != nil {
		t.Fatal(err)
	}
	if string(without) != string(empty) || strings.Contains(string(empty), "postgres:\n") {
		t.Fatalf("an empty values.postgres changed the file:\n%s", empty)
	}
}

// TestDeployRendersOnboardingFromTheRow: an app whose database row has
// source_kind external (ADR 0042) is rendered cold, born from the source,
// with the row's image, database and owner, whatever the deployment.
func TestDeployRendersOnboardingFromTheRow(t *testing.T) {
	se := newStorageEnv(t)
	se.github.push("main", sha("p1"), map[string]string{"platform.yaml": pgSpec, "Dockerfile": "FROM scratch\n"})
	spec, errs := appspec.Check([]byte(pgSpec))
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	now := time.Now().UTC()
	row, err := NewAppDatabase(se.cipher, "shop", []string{"eks", "gke"}, now)
	if err != nil {
		t.Fatal(err)
	}
	row.SourceKind, row.Image, row.Database, row.Owner, row.StorageSize = store.DBSourceExternal, "img@sha256:cc", "moim", "moim", "2Gi"
	row.ExternalHost = "hop-db-legacy-shop"
	if row.ReplicationPasswordNonce, row.ReplicationPasswordCiphertext, err = se.cipher.SealDatabaseField("shop", secrets.DatabaseReplicationPassword, "Rp1"); err != nil {
		t.Fatal(err)
	}
	if err := se.store.CreateAppWithDatabase(context.Background(), store.App{
		Name: "shop", Repo: "org/shop", Branch: "main", Environments: []string{"eks"}, AutoDeploy: true,
		Spec: specJSON(*spec), CreatedBy: "op", CreatedAt: now,
	}, row); err != nil {
		t.Fatal(err)
	}
	se.start()
	password, _, err := se.cipher.OpenDatabase(row)
	if err != nil {
		t.Fatal(err)
	}
	d := se.deploy(sha("p1"), "eks")
	if d.Status != store.DeployHealthy {
		t.Fatalf("deployment %s %q\n%s", d.Status, d.Error, se.dump(d.ID))
	}
	files, _ := se.git.snapshot()
	var f struct {
		Values struct {
			Services []struct {
				Replicas int `json:"replicas"`
			} `json:"services"`
			Postgres   PostgresValues `json:"postgres"`
			DBTopology struct {
				External map[string]string `json:"external"`
			} `json:"dbTopology"`
			OnboardingCold bool `json:"onboardingCold"`
			SealedSecrets  struct {
				PGApp    map[string]string `json:"pgApp"`
				PGLegacy map[string]string `json:"pgLegacy"`
			} `json:"sealedSecrets"`
		} `json:"values"`
	}
	if err := yaml.Unmarshal(files[FilePath("shop", "eks")], &f); err != nil {
		t.Fatal(err)
	}
	v := f.Values
	if !v.OnboardingCold || len(v.Services) != 1 || v.Services[0].Replicas != 0 {
		t.Fatalf("not cold: %+v", v)
	}
	if v.Postgres.Image != "img@sha256:cc" || v.Postgres.Database != "moim" || v.Postgres.Owner != "moim" || v.Postgres.StorageSize != "2Gi" {
		t.Fatalf("postgres %+v", v.Postgres)
	}
	if x := v.DBTopology.External; x["host"] != "hop-db-legacy-shop" || x["passwordSecret"] != "shop-db-legacy" || x["user"] != "hopcharlie_replicator" {
		t.Fatalf("external %v", x)
	}
	ct := se.ctrl["eks"]
	if got := ct.open(t, "shop", "shop-db-legacy", v.SealedSecrets.PGLegacy["password"]); got != "Rp1" {
		t.Fatalf("pgLegacy password opens to %q", got)
	}
	if got := ct.open(t, "shop", "shop-db-app", v.SealedSecrets.PGApp["uri"]); got != "postgresql://moim:"+password+"@shop-db-rw.shop:5432/moim" {
		t.Fatalf("pgApp uri opens to %q", got)
	}
}

// TestGatewaySpecKeepsDial: a deployment that does not set the legacy
// backend keeps its dial address; one that sets a backend drops it.
func TestGatewaySpecKeepsDial(t *testing.T) {
	cur := &gateway.AppInfo{Backends: map[string]string{"legacy": "https://shop.hopcharlie.dev", "eks": "http://old"},
		WriteBackend: "legacy", Dial: map[string]string{"legacy": "203.0.113.10:443", "eks": "10.0.0.1:80"}}
	got := gatewaySpec(cur, []string{"eks"}, map[string]string{"eks": "http://new"})
	if got.WriteBackend != "legacy" || got.Dial["legacy"] != "203.0.113.10:443" || len(got.Dial) != 1 {
		t.Fatalf("spec %+v", got)
	}
	if got := gatewaySpec(nil, []string{"eks"}, map[string]string{"eks": "http://new"}); got.Dial != nil {
		t.Fatalf("dial without a current app: %v", got.Dial)
	}
}
