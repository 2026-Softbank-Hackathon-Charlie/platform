package gateway

import (
	"net/url"
	"path/filepath"
	"testing"
)

func TestOwnerLossPersistsAllAppHolds(t *testing.T) {
	u, _ := url.Parse("http://127.0.0.1:1")
	path := filepath.Join(t.TempDir(), "state.json")
	cfg := Config{Backends: []BackendConfig{{Name: "gke", URL: u}}, StateFile: path}
	g, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	rt, err := g.newRouter("shop", cfg.Backends, g.log, g.prom.apps.view(map[string]string{"app": "shop"}))
	if err != nil {
		t.Fatal(err)
	}
	rt.writeBackend = "gke"
	rt.readWeights = map[string]int{"gke": 100}
	g.apps["shop"] = rt
	if err := g.HoldAfterOwnerLoss(); err != nil {
		t.Fatal(err)
	}
	restored, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, rt := range restored.routers() {
		if !rt.held || rt.gen == nil {
			t.Fatalf("%s lost its recovery hold", rt.name)
		}
	}
}
