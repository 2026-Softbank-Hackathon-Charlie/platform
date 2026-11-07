package dbroute

import (
	"context"
	"testing"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
)

func TestThreeEnvironmentRoutes(t *testing.T) {
	p := &pairs{list: []store.AppDatabase{pair("shop")}, envs: map[string][]string{"shop": {"gke", "eks", "onprem"}}}
	m, cs := newManager(t, p)
	m.cfg.Routes["onprem"] = Route{Namespace: ns, Prefix: "db-onprem-"}
	if err := m.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, env := range []string{"gke", "eks", "onprem"} {
		if _, ok := get(t, cs, "db-"+env+"-shop"); !ok {
			t.Fatalf("missing %s management database route", env)
		}
	}
}
