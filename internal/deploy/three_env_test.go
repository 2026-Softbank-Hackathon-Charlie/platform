package deploy

import (
	"strings"
	"testing"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/appspec"
)

func TestRenderThreeDatabaseMembers(t *testing.T) {
	spec, errs := appspec.Check([]byte(pgSpec))
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	db := sealedDatabase{row: store.AppDatabase{App: "shop", PrimaryEnv: "gke", PeerEnv: "eks"}}
	members := []string{"gke", "eks", "onprem"}
	for _, env := range members {
		topology, err := db.topology(env, members)
		if err != nil {
			t.Fatal(err)
		}
		f := File{App: "shop", Instance: "shop", Env: env, Commit: "c", Deployment: "d", Spec: *spec, Images: map[string]string{"web": "r/web@sha256:aa"}, DBTopology: topology, Sealed: &Sealed{PGApp: map[string]string{"password": "sealed"}, PGCA: map[string]string{"ca.crt": "sealed", "ca.key": "sealed"}}}
		b, err := Render(f)
		if err != nil || !strings.Contains(string(b), "    members:\n      - \"gke\"\n      - \"eks\"\n      - \"onprem\"\n") {
			t.Fatalf("%s topology: %s %v", env, b, err)
		}
		f.DBTopology.Members = []string{"gke", "eks", "eks"}
		if _, err := Render(f); err == nil {
			t.Fatal("duplicate database members accepted")
		}
	}
}
