package api

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/apps"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/runs"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

func withAppDBTopology() option {
	return func(h *harness, _ *runs.Config, _ *Deps) { h.appDBTopology = true }
}

func TestCreateAppMakesDatabasePair(t *testing.T) {
	dep := &fakeDeployer{}
	h := newHarness(t, withApps(""), withGitHub(shopGitHub(), dep), withSecrets(testCipher(t)), withAppDBTopology())
	code, b := h.do("POST", "/v1/apps", opToken, map[string]any{"repo": shopRepo, "environments": []string{"eks", "gke"}})
	if code != 201 {
		t.Fatalf("create: %d %s", code, b)
	}
	// The environments are in envs-file order, gke first: gke is born primary.
	want := apiv1.AppDBTopology{Primary: "gke", Replica: "eks", ReplicaPresent: true}
	if got := decode[apiv1.AppCreated](t, b).App.Deploy.DbTopology; got == nil || *got != want {
		t.Fatalf("created dbTopology %+v, want %+v", got, want)
	}
	row, err := h.store.GetAppDatabase(t.Context(), "shop")
	if err != nil || row.PrimaryEnv != "gke" || row.PeerEnv != "eks" || row.CACert == "" {
		t.Fatalf("stored pair %+v, %v", row, err)
	}
	if _, _, err := testCipher(t).OpenDatabase(row); err != nil {
		t.Fatalf("the stored pair does not open with the server's key: %v", err)
	}
	code, b = h.do("GET", "/v1/apps/shop", viewerToken, nil)
	// Only GET /v1/apps/{name} reads the clusters for the readiness.
	got := decode[apiv1.App](t, b).Deploy.DbTopology
	if code != 200 || got == nil || got.Readiness == nil {
		t.Fatalf("get: %d %s", code, b)
	}
	if got.Readiness = nil; *got != want {
		t.Fatalf("get: %d %s", code, b)
	}
	code, b = h.do("GET", "/v1/apps", viewerToken, nil)
	for _, a := range decode[apiv1.AppList](t, b).Apps {
		if a.Name == "shop" && (a.Deploy.DbTopology == nil || *a.Deploy.DbTopology != want) {
			t.Fatalf("list: %d %s", code, b)
		}
	}
}

func TestCreateAppWithoutDatabasePair(t *testing.T) {
	for name, tc := range map[string]struct {
		envs []string
		opts []option
	}{
		"one environment of three templates": {[]string{"gke"}, []option{withSecrets(testCipher(t)), withAppDBTopology(), withAppDBEnvs("gke", "eks", "onprem"), withSealing("gke")}},
		"one environment without sealing":    {[]string{"gke"}, []option{withSecrets(testCipher(t)), withAppDBTopology(), withSealing("eks")}},
		"one environment without a template": {[]string{"onprem"}, []option{withSecrets(testCipher(t)), withAppDBTopology()}},
		"one environment, the only template": {[]string{"gke"}, []option{withSecrets(testCipher(t)), withAppDBTopology(), withAppDBEnvs("gke")}},
		"three environments":                 {[]string{"gke", "eks", "onprem"}, []option{withSecrets(testCipher(t)), withAppDBTopology()}},
		"setting off":                        {[]string{"gke", "eks"}, []option{withSecrets(testCipher(t))}},
		"no host template":                   {[]string{"gke", "onprem"}, []option{withSecrets(testCipher(t)), withAppDBTopology()}},
		"no postgres":                        {[]string{"gke", "eks"}, []option{withSecrets(testCipher(t)), withAppDBTopology(), withoutPostgres()}},
		"no secrets key":                     {[]string{"gke", "eks"}, []option{withAppDBTopology()}},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, append([]option{withApps(""), withGitHub(shopGitHub(), &fakeDeployer{})}, tc.opts...)...)
			code, b := h.do("POST", "/v1/apps", opToken, map[string]any{"repo": shopRepo, "environments": tc.envs})
			if code != 201 {
				t.Fatalf("create: %d %s", code, b)
			}
			if got := decode[apiv1.AppCreated](t, b).App.Deploy.DbTopology; got != nil {
				t.Fatalf("dbTopology %+v", got)
			}
			if _, err := h.store.GetAppDatabase(t.Context(), "shop"); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("stored pair: %v, want ErrNotFound", err)
			}
		})
	}
}

// withSealing makes envs the environments with a usable sealed-secrets
// certificate.
func withSealing(envs ...string) option {
	return func(_ *harness, _ *runs.Config, d *Deps) {
		d.SealingConfigured = func(env string) bool { return slices.Contains(envs, env) }
	}
}

func withAppDBEnvs(envs ...string) option {
	return func(h *harness, _ *runs.Config, _ *Deps) { h.appDBEnvs = envs }
}

// TestCreateAppInOneEnvironmentMakesDatabasePair: an app registered in one
// environment while exactly one other has a host template is born with
// the pair, the other environment its peer (ADR 0043).
func TestCreateAppInOneEnvironmentMakesDatabasePair(t *testing.T) {
	for _, tc := range []struct{ env, peer string }{{"gke", "eks"}, {"eks", "gke"}} {
		t.Run(tc.env, func(t *testing.T) {
			routes := &fakeDBRoutes{}
			h := newHarness(t, withApps(""), withGitHub(shopGitHub(), &fakeDeployer{}), withSecrets(testCipher(t)), withAppDBTopology(), withDBRoutes(routes), withSealing(tc.env))
			code, b := h.do("POST", "/v1/apps", opToken, map[string]any{"repo": shopRepo, "environments": []string{tc.env}})
			if code != 201 {
				t.Fatalf("create: %d %s", code, b)
			}
			created := decode[apiv1.AppCreated](t, b)
			want := apiv1.AppDBTopology{Primary: tc.env, Replica: tc.peer}
			if got := created.App.Deploy.DbTopology; got == nil || *got != want {
				t.Fatalf("dbTopology %+v, want %+v", got, want)
			}
			if envs := created.Deployment.Environments; len(envs) != 1 || envs[0].Env != tc.env {
				t.Fatalf("first deployment goes to %+v, want only %s", envs, tc.env)
			}
			row, err := h.store.GetAppDatabase(t.Context(), "shop")
			if err != nil || row.PrimaryEnv != tc.env || row.PeerEnv != tc.peer || row.CACert == "" {
				t.Fatalf("stored pair %+v, %v", row, err)
			}
			if _, _, err := testCipher(t).OpenDatabase(row); err != nil {
				t.Fatalf("the stored pair does not open: %v", err)
			}
			if want := []string{"shop:" + tc.env + "," + tc.peer}; !reflect.DeepEqual(routes.added, want) {
				t.Fatalf("routes added %v, want %v", routes.added, want)
			}
			// The peer is not the app's yet, so readiness says so first.
			code, b = h.do("GET", "/v1/apps/shop", viewerToken, nil)
			r := decode[apiv1.App](t, b).Deploy.DbTopology.Readiness
			if code != 200 || r == nil || r.Ready || r.Reason != "replica_missing" || len(r.Environments) != 2 || r.Environments[1].InApp {
				t.Fatalf("get: %d %s", code, b)
			}
		})
	}
}

// withoutPostgres serves shop's platform.yaml without its postgres resource.
func withoutPostgres() option {
	return func(_ *harness, _ *runs.Config, d *Deps) {
		gh := newFakeGitHub()
		gh.repo(shopRepo, "main", headSHA, strings.Replace(shopSpec, "resources:\n  - {name: db, type: postgres}\n", "", 1), "Dockerfile", "worker/Dockerfile")
		d.GitHub = gh
	}
}

// pairFails is a store that cannot store a database pair: the registration
// fails as a whole.
type pairFails struct{ store.Store }

func (pairFails) CreateAppWithDatabase(context.Context, store.App, store.AppDatabase) error {
	return errors.New("the database is down")
}

func TestCreateAppFailsWithItsDatabasePair(t *testing.T) {
	dep := &fakeDeployer{}
	h := newHarness(t, withApps(""), withGitHub(shopGitHub(), dep), withSecrets(testCipher(t)), withAppDBTopology(),
		func(_ *harness, _ *runs.Config, d *Deps) { d.Store = pairFails{d.Store} })
	if code, b := h.do("POST", "/v1/apps", opToken, map[string]any{"repo": shopRepo, "environments": []string{"gke", "eks"}}); code != 500 {
		t.Fatalf("create: %d %s, want 500", code, b)
	}
	if _, err := h.store.GetApp(t.Context(), "shop"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("app after a failed registration: %v", err)
	}
	if ids := dep.enqueued(); len(ids) != 0 {
		t.Fatalf("enqueued %v", ids)
	}
}

// withRegistry adds the registered apps with a database pair to the
// catalog withApps made (ADR 0030), with the host templates of the
// management cluster.
func withRegistry() option {
	return func(h *harness, _ *runs.Config, d *Deps) {
		tmpls, err := apps.ParseHostTemplates("gke=db-gke-{instance}.hopcharlie-db-network.svc.cluster.local,eks=db-eks-{instance}.hopcharlie-db-network.svc.cluster.local", []string{"gke", "eks", "onprem"})
		if err != nil {
			h.t.Fatal(err)
		}
		d.Apps = d.Apps.WithRegistry(&apps.Registry{Store: h.store, Secrets: d.Secrets, HostTemplates: tmpls})
	}
}

// TestRegisteredAppInTheCatalog: a registered app with a database pair is
// listed, read, watched and handed to the AI runner like an app of the
// apps file, with the environments of its pair and nothing secret.
func TestRegisteredAppInTheCatalog(t *testing.T) {
	var asked []string
	runner := &fakeRuns{}
	h := newHarness(t, withApps(gatewayURL), withGitHub(shopGitHub(), &fakeDeployer{}), withSecrets(testCipher(t)), withAppDBTopology(), withRegistry(), withAIRuns(runner),
		func(h *harness, _ *runs.Config, d *Deps) {
			d.TrafficApp = func(app string) Traffic { asked = append(asked, app); return h.traffic }
		})
	// Not yet registered: unknown everywhere.
	if code, b := h.do("GET", "/v1/traffic?app=shop", viewerToken, nil); code != 400 {
		t.Fatalf("traffic before registration: %d %s", code, b)
	}
	if code, b := h.do("POST", "/v1/apps", opToken, map[string]any{"repo": shopRepo, "environments": []string{"gke", "eks"}}); code != 201 {
		t.Fatalf("create: %d %s", code, b)
	}

	want := []apiv1.AppEnvironment{
		{Env: "gke", CnpgCluster: "shop-db", DbHost: "db-gke-shop.hopcharlie-db-network.svc.cluster.local:5432"},
		{Env: "eks", CnpgCluster: "shop-db", DbHost: "db-eks-shop.hopcharlie-db-network.svc.cluster.local:5432"},
	}
	check := func(what string, a apiv1.App, b []byte) {
		t.Helper()
		if a.Name != "shop" || a.Namespace != "shop" || a.Database.Name != "app" || a.Database.User != "app" || len(a.Database.Schemas) != 0 ||
			!reflect.DeepEqual(a.Environments, want) || a.Deploy == nil || a.Deploy.DbTopology == nil {
			t.Fatalf("%s: %+v", what, a)
		}
		for _, secret := range []string{"PGPASSWORD", "CERTIFICATE", "PRIVATE KEY"} {
			if strings.Contains(string(b), secret) {
				t.Fatalf("%s shows %s: %s", what, secret, b)
			}
		}
	}
	code, b := h.do("GET", "/v1/apps", viewerToken, nil)
	list := decode[apiv1.AppList](t, b).Apps
	if code != 200 || len(list) != 2 || list[0].Name != "moimroom" {
		t.Fatalf("list: %d %s", code, b)
	}
	check("list", list[1], b)
	code, b = h.do("GET", "/v1/apps/shop", viewerToken, nil)
	if code != 200 {
		t.Fatalf("get: %d %s", code, b)
	}
	check("get", decode[apiv1.App](t, b), b)

	if code, b := h.do("GET", "/v1/traffic?app=shop", viewerToken, nil); code != 200 || fmt.Sprint(asked) != "[shop]" {
		t.Fatalf("traffic: %d %s, asked %v", code, b, asked)
	}

	if code, b := h.do("POST", "/v1/ai-runs", opToken, apiv1.AIRunRequest{App: "shop", Source: "gke", Target: "eks"}); code != 202 {
		t.Fatalf("ai run: %d %s", code, b)
	}
	if code, b := h.do("POST", "/v1/ai-runs", opToken, apiv1.AIRunRequest{App: "shop", Source: "gke", Target: "onprem"}); code != 400 {
		t.Fatalf("ai run to an environment of no pair: %d %s", code, b)
	}
	if len(runner.started) != 1 {
		t.Fatalf("started %+v", runner.started)
	}
}

// TestRegisteredAppWithoutPairStaysOutOfTheCatalog: an app registered
// without a database pair keeps the defaults with no environments, and the
// apps file wins over a registered app of the same name.
func TestRegisteredAppWithoutPairStaysOutOfTheCatalog(t *testing.T) {
	h := newHarness(t, withApps(gatewayURL), withGitHub(shopGitHub(), &fakeDeployer{}), withSecrets(testCipher(t)), withRegistry())
	if code, b := h.do("POST", "/v1/apps", opToken, map[string]any{"repo": shopRepo, "environments": []string{"gke", "eks"}}); code != 201 {
		t.Fatalf("create: %d %s", code, b)
	}
	code, b := h.do("GET", "/v1/apps/shop", viewerToken, nil)
	if a := decode[apiv1.App](t, b); code != 200 || len(a.Environments) != 0 || a.Deploy.DbTopology != nil {
		t.Fatalf("get: %d %s", code, b)
	}
	if code, b := h.do("GET", "/v1/traffic?app=shop", viewerToken, nil); code != 400 {
		t.Fatalf("traffic of an app without a pair: %d %s", code, b)
	}
}
