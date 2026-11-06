package api

import (
	"slices"
	"sync"
	"testing"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/runs"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
)

// fakeDBRoutes records the pairs added and the apps removed.
type fakeDBRoutes struct {
	mu      sync.Mutex
	added   []string
	removed []string
}

func (f *fakeDBRoutes) Added(d store.AppDatabase) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.added = append(f.added, d.App+":"+d.PrimaryEnv+","+d.PeerEnv)
}

func (f *fakeDBRoutes) Removed(app string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = append(f.removed, app)
}

func withDBRoutes(r *fakeDBRoutes) option {
	return func(_ *harness, _ *runs.Config, d *Deps) { d.DBRoutes = r }
}

func TestDBRoutesFollowRegistrationAndDeletion(t *testing.T) {
	routes, rm := &fakeDBRoutes{}, &fakeRemover{}
	h := newHarness(t, withApps(""), withGitHub(shopGitHub(), &fakeDeployer{}), withSecrets(testCipher(t)), withAppDBTopology(),
		withRemover(rm), withDBRoutes(routes))
	h.registerShop()
	if !slices.Equal(routes.added, []string{"shop:gke,eks"}) {
		t.Fatalf("added %v", routes.added)
	}
	if code, b := h.do("DELETE", "/v1/apps/shop", opToken, nil); code != 202 {
		t.Fatalf("delete: %d %s", code, b)
	}
	if !slices.Equal(routes.removed, []string{"shop"}) {
		t.Fatalf("removed %v", routes.removed)
	}
}

func TestDBRoutesNotAddedWithoutAPair(t *testing.T) {
	routes := &fakeDBRoutes{}
	h := newHarness(t, withApps(""), withGitHub(shopGitHub(), &fakeDeployer{}), withSecrets(testCipher(t)), withDBRoutes(routes))
	h.registerShop()
	if len(routes.added) != 0 {
		t.Fatalf("added %v", routes.added)
	}
}
