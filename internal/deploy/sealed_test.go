package deploy

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	sscrypto "github.com/bitnami-labs/sealed-secrets/pkg/crypto"
	"sigs.k8s.io/yaml"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/sealing"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/secrets"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/storage"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/appspec"
)

// controller is one sealed-secrets controller key pair and its sealer.
type controller struct {
	key    *rsa.PrivateKey
	sealer *sealing.Sealer
}

func newController(t *testing.T) controller {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "sealed-secret"}, NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	sl, err := sealing.ParseCert(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	if err != nil {
		t.Fatal(err)
	}
	return controller{key: key, sealer: sl}
}

// open decrypts a sealed value as the controller does for namespace/name.
func (c controller) open(t *testing.T, namespace, name, sealed string) string {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(sealed)
	if err != nil {
		t.Fatal(err)
	}
	out, err := sscrypto.HybridDecrypt(rand.Reader, map[string]*rsa.PrivateKey{"k": c.key}, raw, sealing.Label(namespace, name))
	if err != nil {
		t.Fatalf("the controller cannot open a value sealed for %s/%s: %v", namespace, name, err)
	}
	return string(out)
}

// fakeDriver is a storage driver in memory: one user per instance with at
// most two keys.
type fakeDriver struct {
	mu      sync.Mutex
	keys    map[string][]string // instance -> key ids
	buckets map[string]bool
	n       int
	deleted []string
	err     error // Delete fails with it
}

func newFakeDriver() *fakeDriver {
	return &fakeDriver{keys: map[string][]string{}, buckets: map[string]bool{}}
}

func (f *fakeDriver) Name() string                  { return "aws" }
func (f *fakeDriver) Bucket(instance string) string { return "hc-" + instance }

func (f *fakeDriver) Ensure(_ context.Context, in storage.Instance, newKey bool, current, old string) (storage.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	res := storage.Result{Bucket: "hc-" + in.Name}
	keys, existed := f.keys[in.Name]
	f.buckets[in.Name] = true
	if existed && !newKey && (current == "" || slices.Contains(keys, current)) {
		return res, nil
	}
	kept := slices.DeleteFunc(slices.Clone(keys), func(k string) bool { return k != current && k != old })
	if len(kept) >= 2 {
		return storage.Result{}, storage.ErrKeyLimit
	}
	f.n++
	id := fmt.Sprintf("AKIA%04d", f.n)
	f.keys[in.Name] = append(kept, id)
	res.Key = &storage.Key{ID: id, Secret: "secret-" + id}
	return res, nil
}

func (f *fakeDriver) DeleteKey(_ context.Context, in storage.Instance, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keys[in.Name] = slices.DeleteFunc(f.keys[in.Name], func(k string) bool { return k == id })
	return nil
}

func (f *fakeDriver) Delete(_ context.Context, in storage.Instance) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	delete(f.keys, in.Name)
	delete(f.buckets, in.Name)
	f.deleted = append(f.deleted, in.Name)
	return nil
}

func (f *fakeDriver) keysOf(instance string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.keys[instance])
}

const s3Spec = `name: shop
services:
  - name: web
    port: 8080
    healthCheck: /healthz
    public: true
    env:
      LOG_LEVEL: info
      HOST_CODE: "from platform.yaml"
resources:
  - {name: photos, type: s3}
`

// storageEnv is a testEnv whose app has an s3 resource, with a cipher, a
// sealer per environment and a storage driver.
type storageEnv struct {
	*testEnv
	cipher *secrets.Cipher
	ctrl   map[string]controller
	drv    *fakeDriver // gke's; every environment has its own
	drvs   map[string]*fakeDriver
}

func newStorageEnv(t *testing.T) *storageEnv {
	t.Helper()
	te := newTestEnv(t)
	te.github.push("main", sha("s1"), map[string]string{"platform.yaml": s3Spec, "Dockerfile": "FROM scratch\n"})
	c, err := secrets.NewCipher(bytes.Repeat([]byte{5}, secrets.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	se := &storageEnv{testEnv: te, cipher: c, ctrl: map[string]controller{"gke": newController(t), "eks": newController(t)},
		drvs: map[string]*fakeDriver{"gke": newFakeDriver(), "eks": newFakeDriver()}}
	se.drv = se.drvs["gke"]
	te.cfg.Secrets = c
	te.cfg.Sealer = func(env string) (*sealing.Sealer, error) {
		ct, ok := se.ctrl[env]
		if !ok {
			return nil, errors.New("environment " + env + ": no certificate")
		}
		return ct.sealer, nil
	}
	te.cfg.Storage = func(env string) (storage.Driver, error) {
		drv, ok := se.drvs[env]
		if !ok {
			return nil, errors.New("environment " + env + " has no storage")
		}
		return drv, nil
	}
	te.cfg.CleanupInterval = time.Hour
	return se
}

func (se *storageEnv) setSecrets(instance string, values map[string]string) {
	se.t.Helper()
	var rows []store.AppSecret
	for name, v := range values {
		r, err := se.cipher.Row("shop", instance, name, v, "op")
		if err != nil {
			se.t.Fatal(err)
		}
		r.UpdatedAt = time.Now().UTC()
		rows = append(rows, r)
	}
	if err := se.store.ChangeAppSecrets(context.Background(), "shop", instance, rows, nil, 50); err != nil {
		se.t.Fatal(err)
	}
}

// gitopsFile is a committed gitops file, decoded.
type gitopsFile struct {
	Values struct {
		Services []struct {
			Name string            `json:"name"`
			Env  map[string]string `json:"env"`
		} `json:"services"`
		SealedSecrets *struct {
			S3          *struct{ AccessKey, SecretKey string } `json:"s3"`
			App         map[string]string                      `json:"app"`
			AppChecksum string                                 `json:"appChecksum"`
		} `json:"sealedSecrets"`
	} `json:"values"`
}

func (se *storageEnv) file(instance, env string) gitopsFile {
	se.t.Helper()
	files, _ := se.git.snapshot()
	raw, ok := files[FilePath(instance, env)]
	if !ok {
		se.t.Fatalf("no gitops file %s", FilePath(instance, env))
	}
	var f gitopsFile
	if err := yaml.Unmarshal(raw, &f); err != nil {
		se.t.Fatal(err)
	}
	return f
}

func (se *storageEnv) deploy(commit string, envs ...string) store.Deployment {
	se.t.Helper()
	d := se.deployment("shop", commit, envs...)
	se.enqueue(d.ID)
	return se.wait(d.ID)
}

func TestDeploySealsBucketKeyAndSecrets(t *testing.T) {
	se := newStorageEnv(t)
	se.register("shop", "gke", "eks")
	se.setSecrets("", map[string]string{"HOST_CODE": "4821", "ADMIN_TOKEN": "t0ken"})
	se.setSecrets("shop", map[string]string{"ADMIN_TOKEN": "default-override"})
	se.addBranch("shop", "other")
	se.setSecrets("shop--other", map[string]string{"NOT_MINE": "x"}) // another instance's override
	se.start()

	d := se.deploy(sha("s1"), "gke", "eks")
	if d.Status != store.DeployHealthy {
		t.Fatalf("deployment %s %q\n%s", d.Status, d.Error, se.dump(d.ID))
	}
	ctx := context.Background()
	for _, env := range []string{"gke", "eks"} {
		f := se.file("shop", env)
		ss := f.Values.SealedSecrets
		if ss == nil || ss.S3 == nil || len(ss.App) != 2 || len(ss.AppChecksum) != 64 {
			t.Fatalf("%s sealedSecrets %+v", env, ss)
		}
		ct := se.ctrl[env]
		row, err := se.store.GetInstanceStorage(ctx, "shop", env)
		if err != nil {
			t.Fatal(err)
		}
		if got := ct.open(t, "shop", "app-s3", ss.S3.AccessKey); got != row.KeyID {
			t.Errorf("%s accessKey opens to %q, want %q", env, got, row.KeyID)
		}
		if got := ct.open(t, "shop", "app-s3", ss.S3.SecretKey); got != "secret-"+row.KeyID {
			t.Errorf("%s secretKey opens to %q", env, got)
		}
		if got := ct.open(t, "shop", "app-secrets", ss.App["ADMIN_TOKEN"]); got != "default-override" {
			t.Errorf("%s ADMIN_TOKEN opens to %q", env, got)
		}
		if got := ct.open(t, "shop", "app-secrets", ss.App["HOST_CODE"]); got != "4821" {
			t.Errorf("%s HOST_CODE opens to %q", env, got)
		}
		// The secret wins: platform.yaml's HOST_CODE is not in env.
		env0 := f.Values.Services[0].Env
		if _, ok := env0["HOST_CODE"]; ok || env0["LOG_LEVEL"] != "info" {
			t.Errorf("%s service env %v", env, env0)
		}
		if row.App != "shop" || row.Driver != "aws" || row.Bucket != "hc-shop" || row.CertFingerprint != ct.sealer.Fingerprint() ||
			row.SealedAccessKey != ss.S3.AccessKey {
			t.Errorf("%s row %+v", env, row)
		}
	}
	gke1 := se.file("shop", "gke").Values.SealedSecrets
	stored, _ := se.store.GetDeployment(ctx, d.ID)
	if len(stored.Spec) == 0 {
		t.Error("the deployment did not record its spec")
	}

	// Again: the stored bucket key is reused as sealed; the secrets are
	// sealed again (other ciphertexts) with the same checksum.
	d2 := se.deploy(sha("s1"), "gke", "eks")
	if d2.Status != store.DeployHealthy {
		t.Fatalf("second deployment %s %q", d2.Status, d2.Error)
	}
	gke2 := se.file("shop", "gke").Values.SealedSecrets
	if gke2.S3.AccessKey != gke1.S3.AccessKey || gke2.App["HOST_CODE"] == gke1.App["HOST_CODE"] || gke2.AppChecksum != gke1.AppChecksum {
		t.Fatalf("second deployment's sealed values: s3 same %v, secret same %v, checksum same %v",
			gke2.S3.AccessKey == gke1.S3.AccessKey, gke2.App["HOST_CODE"] == gke1.App["HOST_CODE"], gke2.AppChecksum == gke1.AppChecksum)
	}
	if k := se.drv.keysOf("shop"); len(k) != 1 {
		t.Fatalf("keys after two deployments %v", k)
	}

	// A changed secret changes the checksum, so the pods restart.
	se.setSecrets("", map[string]string{"HOST_CODE": "9999"})
	se.deploy(sha("s1"), "gke", "eks")
	if gke3 := se.file("shop", "gke").Values.SealedSecrets; gke3.AppChecksum == gke1.AppChecksum {
		t.Fatal("the checksum did not change with a secret")
	}
}

func TestDeployBucketKeyRotation(t *testing.T) {
	se := newStorageEnv(t)
	se.register("shop", "gke")
	se.start()
	ctx := context.Background()
	row := func() store.InstanceStorage {
		t.Helper()
		r, err := se.store.GetInstanceStorage(ctx, "shop", "gke")
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	shop := storage.Instance{App: "shop", Name: "shop"}

	se.deploy(sha("s1"), "gke")
	row1 := row()
	if f := se.file("shop", "gke"); f.Values.SealedSecrets.App != nil || f.Values.SealedSecrets.AppChecksum != "" {
		t.Fatalf("no secrets, but sealedSecrets %+v", f.Values.SealedSecrets)
	}

	// The key is deleted outside the platform: the next deployment makes a
	// new one.
	if err := se.drv.DeleteKey(ctx, shop, row1.KeyID); err != nil {
		t.Fatal(err)
	}
	d := se.deploy(sha("s1"), "gke")
	row2 := row()
	if d.Status != store.DeployHealthy || row2.KeyID == row1.KeyID || row2.OldKeyID != "" || !hasEvent(se.events(d.ID), EventLog, "was gone") {
		t.Fatalf("after the key was deleted: %s, %+v\n%s", d.Status, row2, se.dump(d.ID))
	}
	if !row2.CreatedAt.Equal(row1.CreatedAt) {
		t.Errorf("the row's created time moved: %s -> %s", row1.CreatedAt, row2.CreatedAt)
	}

	// A new controller certificate: a new key; the old one is recorded and
	// kept while the pods may use it (no secrets, so they do not restart).
	se.ctrl["gke"] = newController(t)
	d = se.deploy(sha("s1"), "gke")
	row3 := row()
	if d.Status != store.DeployHealthy || row3.KeyID == row2.KeyID || row3.OldKeyID != row2.KeyID || row3.CertFingerprint != se.ctrl["gke"].sealer.Fingerprint() {
		t.Fatalf("after a new certificate: %s %+v", d.Status, row3)
	}
	if k := se.drv.keysOf("shop"); !slices.Equal(k, []string{row2.KeyID, row3.KeyID}) || !hasEvent(se.events(d.ID), EventLog, "kept the old bucket key ..."+row2.KeyID[4:]) {
		t.Fatalf("keys %v\n%s", k, se.dump(d.ID))
	}
	if dump := se.dump(d.ID); strings.Contains(dump, row2.KeyID) || strings.Contains(dump, row3.KeyID) {
		t.Fatalf("events show whole key ids:\n%s", dump)
	}
	got := se.ctrl["gke"].open(t, "shop", "app-s3", se.file("shop", "gke").Values.SealedSecrets.S3.AccessKey)
	if got != row3.KeyID {
		t.Fatalf("the new certificate opens %q", got)
	}

	// Another certificate while the old key may be in use: refused, as
	// that key must stay; nothing changes.
	se.ctrl["gke"] = newController(t)
	d = se.deploy(sha("s1"), "gke")
	if d.Status != store.DeployFailed || !strings.Contains(d.Error, "may still be used by pods that did not restart") {
		t.Fatalf("a second new certificate: %s %q", d.Status, d.Error)
	}
	if k := se.drv.keysOf("shop"); !slices.Equal(k, []string{row2.KeyID, row3.KeyID}) || row() != row3 {
		t.Fatalf("a refused new key changed the keys %v or the row %+v", k, row())
	}
	// An operator restarted the pods and deleted the old key: it goes on.
	if err := se.drv.DeleteKey(ctx, shop, row3.OldKeyID); err != nil {
		t.Fatal(err)
	}
	d = se.deploy(sha("s1"), "gke")
	row4 := row()
	if d.Status != store.DeployHealthy || row4.OldKeyID != row3.KeyID || !slices.Equal(se.drv.keysOf("shop"), []string{row3.KeyID, row4.KeyID}) {
		t.Fatalf("after the old key was deleted: %s %+v, keys %v\n%s", d.Status, row4, se.drv.keysOf("shop"), se.dump(d.ID))
	}

	// With secrets the pods restart (the key id is in the checksum), so any
	// healthy deployment deletes the old key, though it made no new one.
	se.setSecrets("", map[string]string{"A": "1"})
	d = se.deploy(sha("s1"), "gke")
	if k := se.drv.keysOf("shop"); !slices.Equal(k, []string{row4.KeyID}) || row().OldKeyID != "" || !hasEvent(se.events(d.ID), EventLog, "deleted the old bucket key") {
		t.Fatalf("keys %v, row %+v after a deployment with secrets\n%s", k, row(), se.dump(d.ID))
	}
	sum1 := se.file("shop", "gke").Values.SealedSecrets.AppChecksum

	// A new certificate with secrets: the old key goes once healthy.
	se.ctrl["gke"] = newController(t)
	d = se.deploy(sha("s1"), "gke")
	row5 := row()
	if k := se.drv.keysOf("shop"); !slices.Equal(k, []string{row5.KeyID}) || row5.OldKeyID != "" || !hasEvent(se.events(d.ID), EventLog, "deleted the old bucket key") {
		t.Fatalf("keys %v, row %+v after a rotation with secrets\n%s", k, row5, se.dump(d.ID))
	}
	if se.file("shop", "gke").Values.SealedSecrets.AppChecksum == sum1 {
		t.Fatal("a new bucket key did not change the checksum")
	}
}

func TestDeployRefusesAnotherDriversBucket(t *testing.T) {
	se := newStorageEnv(t)
	se.register("shop", "gke")
	se.start()
	ctx := context.Background()
	// gke had the minio driver when the bucket was made.
	before := store.InstanceStorage{Instance: "shop", Env: "gke", App: "shop", Driver: "minio", Bucket: "hc-shop", KeyID: "SVC0001",
		SealedAccessKey: "AgBhaw==", SealedSecretKey: "AgBzaw==", CertFingerprint: se.ctrl["gke"].sealer.Fingerprint(), CreatedAt: time.Now().UTC()}
	if err := se.store.PutInstanceStorage(ctx, before); err != nil {
		t.Fatal(err)
	}
	d := se.deploy(sha("s1"), "gke")
	if d.Status != store.DeployFailed || !strings.Contains(d.Error, "made by the minio driver, but gke now has the aws driver") {
		t.Fatalf("deployment %s %q", d.Status, d.Error)
	}
	after, err := se.store.GetInstanceStorage(ctx, "shop", "gke")
	if err != nil || after.KeyID != before.KeyID || after.Driver != "minio" || len(se.drv.keysOf("shop")) != 0 {
		t.Fatalf("the row %+v (%v) or the aws keys %v changed", after, err, se.drv.keysOf("shop"))
	}
}

func TestDeployWithoutSealerOrStorage(t *testing.T) {
	cases := map[string]struct {
		setup func(se *storageEnv)
		want  string
	}{
		"no certificate": {func(se *storageEnv) { delete(se.ctrl, "gke") }, "no usable sealed-secrets certificate"},
		"no sealers":     {func(se *storageEnv) { se.cfg.Sealer = nil }, "no sealed-secrets certificates"},
		"no driver": {func(se *storageEnv) {
			se.cfg.Storage = func(env string) (storage.Driver, error) { return nil, errors.New("no storage driver for " + env) }
		}, "needs a bucket in gke: no storage driver"},
		"no secrets key": {func(se *storageEnv) {
			se.setSecrets("", map[string]string{"A": "1"})
			se.cfg.Secrets = nil
		}, "no PLATFORM_SECRETS_KEY"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			se := newStorageEnv(t)
			se.register("shop", "gke")
			tc.setup(se)
			se.start()
			d := se.deploy(sha("s1"), "gke")
			if d.Status != store.DeployFailed || !strings.Contains(d.Error, tc.want) {
				t.Fatalf("%s %q, want failed with %q", d.Status, d.Error, tc.want)
			}
			if files, _ := se.git.snapshot(); len(files) != 0 {
				t.Fatalf("committed %d files", len(files))
			}
		})
	}
}

// A secret stored before its name became one the platform sets (ADR 0024)
// is left out with a note, so the app still deploys; the others are sealed.
func TestDeploySkipsReservedSecrets(t *testing.T) {
	se := newStorageEnv(t)
	se.register("shop", "gke")
	se.setSecrets("", map[string]string{"HOST_CODE": "4821", "PGHOST": "db", "JDBC_DATABASE_URL": "jdbc:x"})
	se.start()

	d := se.deploy(sha("s1"), "gke")
	if d.Status != store.DeployHealthy {
		t.Fatalf("deployment %s %q\n%s", d.Status, d.Error, se.dump(d.ID))
	}
	ss := se.file("shop", "gke").Values.SealedSecrets
	if ss == nil || len(ss.App) != 1 || ss.App["HOST_CODE"] == "" {
		t.Fatalf("sealed app secrets %+v, want only HOST_CODE", ss)
	}
	if !hasEvent(se.events(d.ID), EventLog, "not sealing secrets JDBC_DATABASE_URL, PGHOST: the platform sets these variables now") {
		t.Errorf("no note about the reserved secrets\n%s", se.dump(d.ID))
	}
}

func TestDeployNothingToSeal(t *testing.T) {
	te := newTestEnv(t) // shop has a postgres resource only, and no secrets
	te.register("shop", "gke")
	te.start()
	d := te.deployment("shop", sha("c1"), "gke")
	te.enqueue(d.ID)
	if got := te.wait(d.ID); got.Status != store.DeployHealthy {
		t.Fatalf("%s %q", got.Status, got.Error)
	}
	files, _ := te.git.snapshot()
	if strings.Contains(string(files[FilePath("shop", "gke")]), "sealedSecrets") {
		t.Fatal("sealedSecrets without anything sealed")
	}
}

func TestRedeploySkipsBuildAndGitHub(t *testing.T) {
	se := newStorageEnv(t)
	se.register("shop", "gke")
	se.setSecrets("", map[string]string{"HOST_CODE": "1"})
	se.start()
	first := se.deploy(sha("s1"), "gke")
	stored, _ := se.store.GetDeployment(context.Background(), first.ID)
	builds := se.builder.count()

	// GitHub no longer has the commit: a redeploy must not read it.
	se.github.mu.Lock()
	se.github.files = map[string]map[string]string{}
	se.github.mu.Unlock()
	se.setSecrets("", map[string]string{"HOST_CODE": "2"})
	got := se.redeploy(stored)
	if got.Status != store.DeployHealthy || se.builder.count() != builds {
		t.Fatalf("redeploy %s %q, builds %d -> %d\n%s", got.Status, got.Error, builds, se.builder.count(), se.dump(got.ID))
	}
	if !hasEvent(se.events(got.ID), EventLog, "using the images of the last healthy deployment") {
		t.Fatalf("events:\n%s", se.dump(got.ID))
	}
	if se.ctrl["gke"].open(t, "shop", "app-secrets", se.file("shop", "gke").Values.SealedSecrets.App["HOST_CODE"]) != "2" {
		t.Fatal("the redeploy did not seal the current secret")
	}
}

// redeploy stores and runs a redeploy of d to gke, as the API makes it.
func (se *storageEnv) redeploy(d store.Deployment) store.Deployment {
	se.t.Helper()
	r := se.redeployment(d, "gke")
	se.enqueue(r.ID)
	return se.wait(r.ID)
}

func TestDeployDegradedFailsEarly(t *testing.T) {
	te := newTestEnv(t)
	te.cfg.SyncTimeout = 5 * time.Second
	te.cfg.DegradedTimeout = 100 * time.Millisecond
	te.argo.health["shop-gke"] = HealthDegraded
	te.register("shop", "gke")
	te.start()
	d := te.deployment("shop", sha("c1"), "gke")
	start := time.Now()
	te.enqueue(d.ID)
	got := te.wait(d.ID)
	if got.Status != store.DeployFailed || !strings.Contains(got.Error, "Degraded for 100ms") || got.Environments[0].Status != EnvDegraded {
		t.Fatalf("%s %q %+v", got.Status, got.Error, got.Environments)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("took %s; the degraded environment should fail it well before the 5s sync timeout", took)
	}
}

// failingBackends never finds an address.
type failingBackends struct{}

func (failingBackends) URL(context.Context, string, string, string, appspec.Service) (string, error) {
	return "", errors.New("no load balancer address yet")
}

func TestBackendLookupUsesTheSyncTimeout(t *testing.T) {
	te := newTestEnv(t)
	te.cfg.SyncTimeout = 600 * time.Millisecond
	te.cfg.Backends = failingBackends{}
	te.argo.health["shop-gke"] = "Progressing"
	te.register("shop", "gke")
	te.start()
	d := te.deployment("shop", sha("c1"), "gke")
	te.enqueue(d.ID)
	// Healthy after about half of the sync timeout; the address lookup gets
	// only what is left.
	time.Sleep(300 * time.Millisecond)
	te.argo.mu.Lock()
	te.argo.health["shop-gke"] = HealthHealthy
	te.argo.mu.Unlock()
	got := te.wait(d.ID)
	if got.Status != store.DeployFailed || !strings.Contains(got.Error, "find the address") {
		t.Fatalf("%s %q", got.Status, got.Error)
	}
	var committed, done time.Time
	for _, e := range te.events(d.ID) {
		if e.Type == EventPhase && e.Phase == store.DeploySyncing {
			committed = e.Time
		}
		if e.Type == EventDone {
			done = e.Time
		}
	}
	if took := done.Sub(committed); took > 900*time.Millisecond {
		t.Fatalf("syncing and the address lookup took %s; together they get the 600ms sync timeout", took)
	}
}

func TestCleanupStorage(t *testing.T) {
	se := newStorageEnv(t)
	se.register("shop", "gke")
	ex := se.start()
	ctx := context.Background()
	branch := se.addBranch("shop", "dev")
	se.github.push("dev", sha("dev1"), map[string]string{"platform.yaml": s3Spec, "Dockerfile": "FROM scratch\n"})
	se.deploy(sha("s1"), "gke")
	dd := se.deploymentTo("shop", "dev", branch, sha("dev1"), "gke")
	se.enqueue(dd.ID)
	if got := se.wait(dd.ID); got.Status != store.DeployHealthy {
		t.Fatalf("dev: %s %q", got.Status, got.Error)
	}
	if rows, _ := se.store.ListInstanceStorage(ctx, "shop", ""); len(rows) != 2 {
		t.Fatalf("rows %+v", rows)
	}
	// Live instances are kept.
	if n, err := ex.CleanupStorage(ctx); err != nil || n != 0 {
		t.Fatalf("cleanup with live instances: %d, %v", n, err)
	}

	// The instance is removed: its bucket and key go, the app's stay.
	if err := ex.RemoveInstance(ctx, "shop", "dev"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		rows, _ := se.store.ListInstanceStorage(ctx, "", branch)
		return len(rows) == 0
	})
	if !slices.Contains(se.drv.deleted, branch) || len(se.drv.keysOf("shop")) != 1 {
		t.Fatalf("deleted %v, the app's keys %v", se.drv.deleted, se.drv.keysOf("shop"))
	}

	// A failing driver keeps the row for the next try.
	se.drv.mu.Lock()
	se.drv.err = errors.New("bucket not empty yet")
	se.drv.mu.Unlock()
	if err := ex.RemoveApp(ctx, "shop"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if n, err := ex.CleanupStorage(ctx); err != nil || n != 0 {
		t.Fatalf("failing cleanup: %d, %v", n, err)
	}
	if rows, _ := se.store.ListInstanceStorage(ctx, "shop", ""); len(rows) != 1 {
		t.Fatalf("rows after a failed cleanup %+v", rows)
	}
	se.drv.mu.Lock()
	se.drv.err = nil
	se.drv.mu.Unlock()
	if n, err := ex.CleanupStorage(ctx); err != nil || n != 1 {
		t.Fatalf("retried cleanup: %d, %v", n, err)
	}
}

// logBuffer collects log lines from several goroutines.
type logBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func TestCleanupStorageStuckRows(t *testing.T) {
	se := newStorageEnv(t)
	logs := &logBuffer{}
	se.cfg.Log = slog.New(slog.NewTextHandler(logs, nil))
	ex := se.start()
	ctx := context.Background()
	// Rows of a deleted instance that the platform cannot delete: its
	// environment is gone, or has another driver now.
	for _, r := range []store.InstanceStorage{
		{Instance: "gone", Env: "onprem", App: "gone", Driver: "minio", Bucket: "hc-gone", KeyID: "SVC0001", CreatedAt: time.Now().UTC()},
		{Instance: "gone", Env: "gke", App: "gone", Driver: "minio", Bucket: "hc-gone", KeyID: "SVC0002", CreatedAt: time.Now().UTC()},
	} {
		if err := se.store.PutInstanceStorage(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := ex.CleanupStorage(ctx); err != nil || n != 0 {
		t.Fatalf("cleanup: %d, %v", n, err)
	}
	if rows, _ := se.store.ListInstanceStorage(ctx, "gone", ""); len(rows) != 2 || len(se.drv.deleted) != 0 {
		t.Fatalf("rows %+v, deleted %v", rows, se.drv.deleted)
	}
	for _, env := range []string{"onprem", "gke"} {
		found := slices.ContainsFunc(strings.Split(logs.String(), "\n"), func(l string) bool {
			return strings.Contains(l, "level=ERROR") && strings.Contains(l, "instance=gone") && strings.Contains(l, "env="+env) && strings.Contains(l, "an operator deletes them")
		})
		if !found {
			t.Fatalf("no error log for gone in %s:\n%s", env, logs)
		}
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestRenderSealed(t *testing.T) {
	spec, errs := appspec.Check([]byte(s3Spec))
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	f := File{App: "shop", Instance: "shop", Env: "eks", Commit: "c", Deployment: "dep_1", Spec: *spec, Images: map[string]string{"web": "r/web@sha256:aa"},
		Sealed: &Sealed{S3: &SealedS3{AccessKey: "AgA1", SecretKey: "AgA2"}, App: map[string]string{"HOST_CODE": "AgB1", "ADMIN_TOKEN": "AgB2"}, AppChecksum: strings.Repeat("a", 64)}}
	got, err := Render(f)
	if err != nil {
		t.Fatal(err)
	}
	want := `  resources:
    - name: "photos"
      type: "s3"
  sealedSecrets:
    s3:
      accessKey: "AgA1"
      secretKey: "AgA2"
    app:
      "ADMIN_TOKEN": "AgB2"
      "HOST_CODE": "AgB1"
    appChecksum: "` + strings.Repeat("a", 64) + `"
`
	if !strings.HasSuffix(string(got), want) || strings.Contains(string(got), `"from platform.yaml"`) || !strings.Contains(string(got), `"LOG_LEVEL": "info"`) {
		t.Fatalf("rendered:\n%s", got)
	}
	f.Sealed = &Sealed{}
	got, _ = Render(f)
	if strings.Contains(string(got), "sealedSecrets") || !strings.Contains(string(got), `"from platform.yaml"`) {
		t.Fatalf("empty sealed values rendered:\n%s", got)
	}
}
