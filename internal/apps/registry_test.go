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

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/plans"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/secrets"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store/storetest"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

const testHostTemplates = "gke=db-gke-{instance}.hopcharlie-db-network.svc.cluster.local,eks=db-eks-{instance}.hopcharlie-db-network.svc.cluster.local"

func TestParseHostTemplates(t *testing.T) {
	got, err := ParseHostTemplates(testHostTemplates, []string{"gke", "eks", "onprem"})
	if err != nil {
		t.Fatal(err)
	}
	if got["eks"] != "db-eks-{instance}.hopcharlie-db-network.svc.cluster.local" || len(got) != 2 {
		t.Fatalf("templates %v", got)
	}
	for _, bad := range []string{
		"", "gke", "gke=", "aks=db-{instance}.svc", "gke=db.svc", "gke=db-{app}-{instance}.svc", "gke=db {instance}",
		"gke=db-{instance}.svc,gke=db2-{instance}.svc", "gke=db-{instance}.svc,",
	} {
		if _, err := ParseHostTemplates(bad, []string{"gke", "eks"}); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}
	if got := RegistryPasswordEnv("eks", "moimroom-mig"); got != "EKS_MOIMROOM_MIG_PGPASSWORD" {
		t.Errorf("RegistryPasswordEnv = %s", got)
	}
}

// registryFixture is the example catalog (moimroom from the apps file) with
// a registry over a memory store.
type registryFixture struct {
	t      *testing.T
	cat    *Catalog
	store  store.Store
	cipher *secrets.Cipher
}

func newRegistryFixture(t *testing.T) *registryFixture {
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
	cat := exampleCatalog(t, "http://gateway.ts.net:9901").WithRegistry(&Registry{Store: st, Secrets: c, HostTemplates: tmpls})
	return &registryFixture{t: t, cat: cat, store: st, cipher: c}
}

// register registers app in gke and eks with a postgres resource db and a
// database pair whose password is password; without a pair when password
// is empty.
func (f *registryFixture) register(app, password string) {
	f.t.Helper()
	spec, _ := json.Marshal(map[string]any{"name": app, "resources": []map[string]string{{"name": "db", "type": "postgres"}}})
	a := store.App{Name: app, Repo: "org/" + app, Branch: "main", Environments: []string{"gke", "eks"}, Spec: spec, CreatedAt: time.Now().UTC()}
	if password == "" {
		if err := f.store.CreateApp(f.t.Context(), a); err != nil {
			f.t.Fatal(err)
		}
		return
	}
	row, err := f.cipher.DatabaseRow(app, "gke", "eks", password, "CERT", "KEY")
	if err != nil {
		f.t.Fatal(err)
	}
	if err := f.store.CreateAppWithDatabase(f.t.Context(), a, row); err != nil {
		f.t.Fatal(err)
	}
}

func TestDraftForRegisteredApp(t *testing.T) {
	f := newRegistryFixture(t)
	t.Setenv("GKE_MOIMROOM_MIG_PGPASSWORD", "")
	t.Setenv("EKS_MOIMROOM_MIG_PGPASSWORD", "")
	os.Unsetenv("GKE_MOIMROOM_MIG_PGPASSWORD")
	os.Unsetenv("EKS_MOIMROOM_MIG_PGPASSWORD")

	req := apiv1.PlanDraftRequest{App: "moimroom-mig", Source: "gke", Target: "eks"}
	if _, err := f.cat.Draft(t.Context(), req); !errors.Is(err, ErrInvalid) {
		t.Fatalf("draft before registration: %v, want ErrInvalid", err)
	}
	// Registered after the catalog was made: read now.
	f.register("moimroom-mig", "pw-of-mig")
	p, err := f.cat.Draft(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if p.Id != "moimroom-mig-gke-to-eks" || p.Source.Namespace != "moimroom-mig" || p.Source.Cluster != "moimroom-mig-db" || p.Target.Cluster != "moimroom-mig-db" {
		t.Fatalf("plan %+v", p)
	}
	wantSrc := "postgres://app:${GKE_MOIMROOM_MIG_PGPASSWORD}@db-gke-moimroom-mig.hopcharlie-db-network.svc.cluster.local:5432/app?sslmode=require"
	wantDst := "postgres://app:${EKS_MOIMROOM_MIG_PGPASSWORD}@db-eks-moimroom-mig.hopcharlie-db-network.svc.cluster.local:5432/app?sslmode=require"
	if p.Source.Dsn != wantSrc || p.Target.Dsn != wantDst {
		t.Fatalf("DSNs\n%s\n%s", p.Source.Dsn, p.Target.Dsn)
	}
	if p.Files != nil || p.Verify != nil || *p.Gateway.Url != "http://gateway.ts.net:9901/apps/moimroom-mig" {
		t.Fatalf("files %+v, verify %+v, gateway %s", p.Files, p.Verify, *p.Gateway.Url)
	}
	// The draft put the password into the environment, read from the pair.
	for _, name := range []string{"GKE_MOIMROOM_MIG_PGPASSWORD", "EKS_MOIMROOM_MIG_PGPASSWORD"} {
		if got := os.Getenv(name); got != "pw-of-mig" {
			t.Errorf("%s = %q after the draft", name, got)
		}
	}
	if _, err := f.cat.Draft(t.Context(), apiv1.PlanDraftRequest{App: "moimroom-mig", Source: "gke", Target: "onprem"}); !errors.Is(err, ErrInvalid) {
		t.Errorf("draft to an environment of no pair: %v", err)
	}

	// An app without a pair (registered before ADR 0030) is not drafted.
	f.register("old", "")
	if _, err := f.cat.Draft(t.Context(), apiv1.PlanDraftRequest{App: "old", Source: "gke", Target: "eks"}); !errors.Is(err, ErrInvalid) {
		t.Errorf("draft of an app without a pair: %v", err)
	}
	// The apps file wins: a registered moimroom is drafted from the file.
	f.register("moimroom", "pw-of-registered")
	p, err = f.cat.Draft(t.Context(), apiv1.PlanDraftRequest{App: "moimroom", Source: "gke", Target: "eks"})
	if err != nil || !strings.Contains(p.Source.Dsn, "${GKE_MOIMROOM_PGPASSWORD}@moimroom-db-gke-rw.moimroom.gke.ts.net") {
		t.Fatalf("file app: %+v %v", p.Source, err)
	}
}

func TestRegistryHosts(t *testing.T) {
	f := newRegistryFixture(t)
	f.register("moimroom-mig", "pw")
	f.register("moimroom", "pw") // the file's moimroom wins
	pw, apps, err := f.cat.RegistryHosts(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	wantPW := map[string][]string{
		"GKE_MOIMROOM_MIG_PGPASSWORD": {"db-gke-moimroom-mig.hopcharlie-db-network.svc.cluster.local:5432"},
		"EKS_MOIMROOM_MIG_PGPASSWORD": {"db-eks-moimroom-mig.hopcharlie-db-network.svc.cluster.local:5432"},
	}
	wantApps := map[string][]string{"moimroom-mig": {
		"db-eks-moimroom-mig.hopcharlie-db-network.svc.cluster.local:5432",
		"db-gke-moimroom-mig.hopcharlie-db-network.svc.cluster.local:5432",
	}}
	if !reflect.DeepEqual(pw, wantPW) || !reflect.DeepEqual(apps, wantApps) {
		t.Fatalf("RegistryHosts = %v, %v", pw, apps)
	}
	// Without a registry: nothing.
	pw, apps, err = exampleCatalog(t, "").RegistryHosts(t.Context())
	if err != nil || len(pw) != 0 || len(apps) != 0 {
		t.Fatalf("no registry: %v %v %v", pw, apps, err)
	}
}

// TestRegisteredAppPlanPassesThePolicy submits drafted plans to a plan
// service with the catalog as its AppDatabases: the registered app's
// variables and hosts are allowed per request, its gateway scope is
// required, and its password is put into the environment at parse time.
func TestRegisteredAppPlanPassesThePolicy(t *testing.T) {
	f := newRegistryFixture(t)
	t.Setenv("GKE_MOIMROOM_MIG_PGPASSWORD", "")
	os.Unsetenv("GKE_MOIMROOM_MIG_PGPASSWORD")
	policy := plans.DefaultPolicy()
	policy.GatewayURL = "http://gateway.ts.net:9901"
	policy.HostPasswordEnvs, policy.AppDBHosts = f.cat.PasswordHosts(), f.cat.AppHosts()
	policy.DSNHostSuffixes = []string{".ts.net", ".hopcharlie-db-network.svc.cluster.local"}
	svc := plans.New(policy, f.store)
	svc.SetAppDatabases(f.cat)

	f.register("moimroom-mig", "pw-of-mig")
	p, err := f.cat.Draft(t.Context(), apiv1.PlanDraftRequest{App: "moimroom-mig", Source: "gke", Target: "eks"})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(p)
	os.Unsetenv("GKE_MOIMROOM_MIG_PGPASSWORD") // the parse puts it back
	if _, err := svc.Submit(t.Context(), raw, "op"); err != nil {
		t.Fatalf("submit the drafted plan: %v", err)
	}
	if got := os.Getenv("GKE_MOIMROOM_MIG_PGPASSWORD"); got != "pw-of-mig" {
		t.Fatalf("password after the parse = %q", got)
	}

	for name, change := range map[string]func(s string) string{
		// The app's databases need the app's gateway scope.
		"gateway root": func(s string) string {
			return strings.Replace(s, `"http://gateway.ts.net:9901/apps/moimroom-mig"`, `"http://gateway.ts.net:9901"`, 1)
		},
		// Its password goes to its own database only.
		"password to another host": func(s string) string {
			return strings.Replace(s, "${EKS_MOIMROOM_MIG_PGPASSWORD}", "${GKE_MOIMROOM_MIG_PGPASSWORD}", 1)
		},
		// Mixed with the file's app.
		"another app's host": func(s string) string {
			return strings.Replace(s, "${EKS_MOIMROOM_MIG_PGPASSWORD}@db-eks-moimroom-mig.hopcharlie-db-network.svc.cluster.local", "${EKS_MOIMROOM_PGPASSWORD}@moimroom-db-eks-rw.moimroom.eks.ts.net", 1)
		},
	} {
		if _, err := svc.Submit(t.Context(), []byte(change(string(raw))), "op"); !errors.Is(err, plans.ErrInvalid) {
			t.Errorf("%s: %v, want ErrInvalid", name, err)
		}
	}

	// Once the app is deleted, its plans no longer pass.
	if err := f.store.DeleteApp(t.Context(), "moimroom-mig"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Submit(t.Context(), raw, "op"); !errors.Is(err, plans.ErrInvalid) {
		t.Fatalf("plan of a deleted app: %v, want ErrInvalid", err)
	}
}
