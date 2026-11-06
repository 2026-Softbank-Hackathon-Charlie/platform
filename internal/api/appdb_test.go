package api

import (
	"context"
	"errors"
	"testing"

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
	want := apiv1.AppDBTopology{Primary: "gke", Replica: "eks"}
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
	if got := decode[apiv1.App](t, b).Deploy.DbTopology; code != 200 || got == nil || *got != want {
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
		"one environment":    {[]string{"gke"}, []option{withSecrets(testCipher(t)), withAppDBTopology()}},
		"three environments": {[]string{"gke", "eks", "onprem"}, []option{withSecrets(testCipher(t)), withAppDBTopology()}},
		"setting off":        {[]string{"gke", "eks"}, []option{withSecrets(testCipher(t))}},
		"no secrets key":     {[]string{"gke", "eks"}, []option{withAppDBTopology()}},
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
