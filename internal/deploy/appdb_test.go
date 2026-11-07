package deploy

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/x509"
	"encoding/pem"
	"regexp"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/secrets"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/appspec"
)

func TestNewAppDatabase(t *testing.T) {
	c, err := secrets.NewCipher(bytes.Repeat([]byte{5}, secrets.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)
	for _, envs := range [][]string{nil, {"gke"}, {"gke", "gke"}, {"gke", "eks", "gke"}, {"gke", "eks", "onprem", "fourth"}} {
		if _, err := NewAppDatabase(c, "shop", envs, now); err == nil {
			t.Errorf("environments %q: no error", envs)
		}
	}
	third, err := NewAppDatabase(c, "three", []string{"gke", "eks", "onprem"}, now)
	if err != nil || third.PrimaryEnv != "gke" || third.PeerEnv != "eks" {
		t.Fatalf("three-member group did not preserve its birth anchors: %v", err)
	}
	row, err := NewAppDatabase(c, "shop", []string{"gke", "eks"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if row.App != "shop" || row.PrimaryEnv != "gke" || row.PeerEnv != "eks" || !row.CreatedAt.Equal(now) {
		t.Fatalf("row %+v", row)
	}
	pw, keyPEM, err := c.OpenDatabase(row)
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[A-Za-z0-9]{32}$`).MatchString(pw) {
		t.Errorf("password %q is not 32 letters and digits", pw)
	}

	// The CA: ECDSA P-256, self-signed, a CA for ten years, its key SEC1.
	cb, _ := pem.Decode([]byte(row.CACert))
	if cb == nil || cb.Type != "CERTIFICATE" {
		t.Fatalf("CA certificate PEM %q", row.CACert)
	}
	cert, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if !cert.IsCA || !cert.BasicConstraintsValid || cert.KeyUsage != x509.KeyUsageCertSign|x509.KeyUsageCRLSign {
		t.Errorf("CA: IsCA %v, basic constraints %v, key usage %v", cert.IsCA, cert.BasicConstraintsValid, cert.KeyUsage)
	}
	if err := cert.CheckSignatureFrom(cert); err != nil {
		t.Errorf("not self-signed: %v", err)
	}
	if got := cert.NotAfter.Sub(now); got != AppDBCAValidity {
		t.Errorf("valid for %v after registration, want %v", got, AppDBCAValidity)
	}
	if !cert.NotBefore.Before(now) {
		t.Errorf("not valid at registration: NotBefore %v", cert.NotBefore)
	}
	kb, _ := pem.Decode([]byte(keyPEM))
	if kb == nil || kb.Type != "EC PRIVATE KEY" {
		t.Fatalf("CA key is not a PEM EC PRIVATE KEY")
	}
	key, err := x509.ParseECPrivateKey(kb.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	pub, ok := cert.PublicKey.(*ecdsa.PublicKey)
	if !ok || pub.Curve != elliptic.P256() || !pub.Equal(&key.PublicKey) {
		t.Error("the CA key is not the certificate's P-256 key")
	}

	// Every registration gets its own.
	again, err := NewAppDatabase(c, "shop", []string{"gke", "eks"}, now)
	if err != nil {
		t.Fatal(err)
	}
	pw2, key2, _ := c.OpenDatabase(again)
	if pw2 == pw || key2 == keyPEM || again.CACert == row.CACert {
		t.Error("a second pair repeats the first one's password, key or certificate")
	}
}

func TestPGAppSecret(t *testing.T) {
	got := pgAppSecret("moimroom-mig-db", "moimroom-mig", "Pw123")
	want := map[string]string{
		"username": "app", "password": "Pw123", "dbname": "app", "host": "moimroom-mig-db-rw", "port": "5432",
		"uri":      "postgresql://app:Pw123@moimroom-mig-db-rw.moimroom-mig:5432/app",
		"jdbc-uri": "jdbc:postgresql://moimroom-mig-db-rw.moimroom-mig:5432/app?password=Pw123&user=app",
	}
	if len(got) != len(want) {
		t.Fatalf("keys %v", sortedKeys(got))
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}

func TestRenderDBTopology(t *testing.T) {
	spec, errs := appspec.Check([]byte(pgSpec))
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	base := File{App: "shop", Instance: "shop", Env: "eks", Commit: "c", Deployment: "dep_1", Spec: *spec, Images: map[string]string{"web": "r/web@sha256:aa"}}
	before, err := Render(base)
	if err != nil {
		t.Fatal(err)
	}

	f := base
	f.DBTopology = &DBTopology{Primary: "gke", Peer: "eks"} // eks is the born replica
	f.Sealed = &Sealed{
		PGApp:    map[string]string{"username": "Ag1", "password": "Ag2", "dbname": "Ag3", "host": "Ag4", "port": "Ag5", "uri": "Ag6", "jdbc-uri": "Ag7"},
		PGCA:     map[string]string{"ca.crt": "Ag8", "ca.key": "Ag9"},
		PGBackup: &SealedBackup{SealedS3: SealedS3{AccessKey: "AgA3", SecretKey: "AgA4"}, Bucket: "b", Path: "p"},
	}
	got, err := Render(f)
	if err != nil {
		t.Fatal(err)
	}
	// The contract with the app chart (ADR 0030): dbTopology after
	// resources, pgApp and pgCA in sealedSecrets before pgBackup.
	want := `  resources:
    - name: "db"
      type: "postgres"
  dbTopology:
    primary: "gke"
    peer: "eks"
  sealedSecrets:
    pgApp:
      "dbname": "Ag3"
      "host": "Ag4"
      "jdbc-uri": "Ag7"
      "password": "Ag2"
      "port": "Ag5"
      "uri": "Ag6"
      "username": "Ag1"
    pgCA:
      "ca.crt": "Ag8"
      "ca.key": "Ag9"
    pgBackup:
      accessKey: "AgA3"
      secretKey: "AgA4"
  backup:
    bucket: "b"
    path: "p"
`
	if !strings.HasSuffix(string(got), want) || !bytes.HasPrefix(got, before[:bytes.Index(before, []byte("  resources:"))]) {
		t.Fatalf("rendered:\n%s", got)
	}
	var parsed struct {
		Values struct {
			DBTopology    map[string]string            `json:"dbTopology"`
			SealedSecrets map[string]map[string]string `json:"sealedSecrets"`
		} `json:"values"`
	}
	if err := yaml.Unmarshal(got, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.Values.DBTopology["peer"] != "eks" || len(parsed.Values.SealedSecrets["pgApp"]) != 7 || len(parsed.Values.SealedSecrets["pgCA"]) != 2 {
		t.Fatalf("parsed %+v", parsed.Values)
	}

	for name, bad := range map[string]func(f *File){
		"no postgres":      func(f *File) { f.Spec.Resources = nil },
		"no pgApp":         func(f *File) { f.Sealed = &Sealed{PGCA: f.Sealed.PGCA} },
		"no pgCA":          func(f *File) { f.Sealed = &Sealed{PGApp: f.Sealed.PGApp} },
		"nothing sealed":   func(f *File) { f.Sealed = nil },
		"same envs":        func(f *File) { f.DBTopology = &DBTopology{Primary: "eks", Peer: "eks"} },
		"not this env":     func(f *File) { f.DBTopology = &DBTopology{Primary: "gke", Peer: "onprem"} },
		"neither this env": func(f *File) { f.Env = "onprem" },
	} {
		g := f
		g.Sealed = &Sealed{PGApp: f.Sealed.PGApp, PGCA: f.Sealed.PGCA}
		bad(&g)
		if _, err := Render(g); err == nil {
			t.Errorf("%s: rendered", name)
		}
	}
}

// registerWithDatabase registers app in envs with a database pair, as
// POST /v1/apps does with PLATFORM_APP_DB_TOPOLOGY.
func (se *storageEnv) registerWithDatabase(name string, envs ...string) store.AppDatabase {
	se.t.Helper()
	spec, errs := appspec.Check([]byte(pgSpec))
	if len(errs) > 0 {
		se.t.Fatal(errs)
	}
	now := time.Now().UTC()
	row, err := NewAppDatabase(se.cipher, name, envs, now)
	if err != nil {
		se.t.Fatal(err)
	}
	err = se.store.CreateAppWithDatabase(context.Background(), store.App{
		Name: name, Repo: "org/" + name, Branch: "main", Environments: envs, AutoDeploy: true,
		Spec: specJSON(*spec), CreatedBy: "op", CreatedAt: now,
	}, row)
	if err != nil {
		se.t.Fatal(err)
	}
	return row
}

func TestDeploySealsAppDatabase(t *testing.T) {
	se := newStorageEnv(t)
	se.github.push("main", sha("p1"), map[string]string{"platform.yaml": pgSpec, "Dockerfile": "FROM scratch\n"})
	row := se.registerWithDatabase("shop", "gke", "eks")
	se.start()
	password, caKey, err := se.cipher.OpenDatabase(row)
	if err != nil {
		t.Fatal(err)
	}

	d := se.deploy(sha("p1"), "gke", "eks")
	if d.Status != store.DeployHealthy {
		t.Fatalf("deployment %s %q\n%s", d.Status, d.Error, se.dump(d.ID))
	}
	type dbFile struct {
		Values struct {
			DBTopology    *struct{ Primary, Peer string } `json:"dbTopology"`
			SealedSecrets struct {
				PGApp map[string]string `json:"pgApp"`
				PGCA  map[string]string `json:"pgCA"`
			} `json:"sealedSecrets"`
			Backup any `json:"backup"`
		} `json:"values"`
	}
	read := func(instance, env string) dbFile {
		t.Helper()
		files, _ := se.git.snapshot()
		var f dbFile
		if err := yaml.Unmarshal(files[FilePath(instance, env)], &f); err != nil {
			t.Fatal(err)
		}
		return f
	}
	wantApp := pgAppSecret("shop-db", "shop", password)
	for _, env := range []string{"gke", "eks"} {
		f := read("shop", env)
		if tp := f.Values.DBTopology; tp == nil || tp.Primary != "gke" || tp.Peer != "eks" {
			t.Fatalf("%s dbTopology %+v, want primary gke, peer eks", env, tp)
		}
		ct := se.ctrl[env]
		ss := f.Values.SealedSecrets
		if len(ss.PGApp) != len(wantApp) || len(ss.PGCA) != 2 {
			t.Fatalf("%s pgApp keys %v, pgCA keys %v", env, sortedKeys(ss.PGApp), sortedKeys(ss.PGCA))
		}
		// The same values in both environments, each sealed for its own
		// cluster, strict scope, namespace shop.
		for k, v := range wantApp {
			if got := ct.open(t, "shop", "shop-db-app", ss.PGApp[k]); got != v {
				t.Errorf("%s pgApp %s opens to %q, want %q", env, k, got, v)
			}
		}
		if got := ct.open(t, "shop", "app-pg-ca", ss.PGCA["ca.crt"]); got != row.CACert {
			t.Errorf("%s ca.crt opens to %q", env, got)
		}
		if got := ct.open(t, "shop", "app-pg-ca", ss.PGCA["ca.key"]); got != caKey {
			t.Errorf("%s ca.key does not open to the stored key", env)
		}
	}
	// The backup bucket of aws environments still comes with it.
	if read("shop", "eks").Values.Backup == nil {
		t.Error("eks lost its backup values")
	}

	// A branch instance keeps its own database: nothing of the pair.
	branch := se.addBranch("shop", "dev")
	se.github.push("dev", sha("dev1"), map[string]string{"platform.yaml": pgSpec, "Dockerfile": "FROM scratch\n"})
	dd := se.deploymentTo("shop", "dev", branch, sha("dev1"), "eks")
	se.enqueue(dd.ID)
	if got := se.wait(dd.ID); got.Status != store.DeployHealthy {
		t.Fatalf("dev: %s %q", got.Status, got.Error)
	}
	files, _ := se.git.snapshot()
	if b := string(files[FilePath(branch, "eks")]); strings.Contains(b, "dbTopology") || strings.Contains(b, "pgApp") || strings.Contains(b, "pgCA") {
		t.Fatalf("branch file has the database pair:\n%s", b)
	}
}

func TestDeployAppDatabaseNeedsTheSecretsKey(t *testing.T) {
	se := newStorageEnv(t)
	se.github.push("main", sha("p1"), map[string]string{"platform.yaml": pgSpec, "Dockerfile": "FROM scratch\n"})
	se.registerWithDatabase("shop", "gke", "eks")
	se.cfg.Secrets = nil
	se.start()
	d := se.deploy(sha("p1"), "gke", "eks")
	if d.Status != store.DeployFailed || !strings.Contains(d.Error, "no PLATFORM_SECRETS_KEY") {
		t.Fatalf("deployment %s %q", d.Status, d.Error)
	}
}

// TestDeployWithoutAppDatabaseIsUnchanged: an app without a database pair
// (registered before ADR 0030, or with PLATFORM_APP_DB_TOPOLOGY off) gets
// exactly the file Render made before: no topology and nothing sealed.
func TestDeployWithoutAppDatabaseIsUnchanged(t *testing.T) {
	te := newTestEnv(t) // shop has a postgres resource
	te.register("shop", "gke", "eks")
	te.start()
	d := te.deployment("shop", sha("c1"), "gke", "eks")
	te.enqueue(d.ID)
	got := te.wait(d.ID)
	if got.Status != store.DeployHealthy {
		t.Fatalf("%s %q", got.Status, got.Error)
	}
	spec, errs := appspec.Check([]byte(shopSpec))
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	images := map[string]string{}
	for _, im := range got.Images {
		images[im.Service] = im.Image
	}
	files, _ := te.git.snapshot()
	for _, env := range []string{"gke", "eks"} {
		want, err := Render(File{App: "shop", Instance: "shop", Env: env, Commit: sha("c1"), Deployment: d.ID, Spec: *spec, Images: images})
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(files[FilePath("shop", env)], want) {
			t.Errorf("%s file:\n%s\nwant:\n%s", env, files[FilePath("shop", env)], want)
		}
	}
}
