package store_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store/storetest"
)

func TestPostgresDeployConformance(t *testing.T) {
	testDatabaseURL(t)
	storetest.ConformanceDeploy(t, func(t *testing.T) store.Store {
		_, u := newSchema(t)
		return openSchema(t, u)
	})
}

// Deployments stored before 0006_app_branches deployed their app's default
// instance: they get the app's branch and its name as the instance. One
// whose app is gone gets its app's name and no branch.
func TestMigrationAppBranchesFillsExistingDeployments(t *testing.T) {
	schema, u := newSchema(t)
	ctx := t.Context()
	conn, err := pgx.Connect(ctx, testDatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, "SET search_path TO "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	versions, _ := migrationFiles(t)
	if _, err := conn.Exec(ctx, `CREATE TABLE schema_migrations (version text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		t.Fatal(err)
	}
	for _, v := range versions {
		if v >= "0006" {
			break
		}
		sql, err := os.ReadFile(filepath.Join("migrations", v+".sql"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("%s: %v", v, err)
		}
		if _, err := conn.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, v); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := conn.Exec(ctx, `INSERT INTO apps (name, branch) VALUES ('moimroom', 'release');
		INSERT INTO deployments (id, app) VALUES ('dep_1', 'moimroom'), ('dep_2', 'gone')`); err != nil {
		t.Fatal(err)
	}

	s := openSchema(t, u)
	defer s.Close()
	for id, want := range map[string]struct{ branch, instance string }{
		"dep_1": {"release", "moimroom"},
		"dep_2": {"", "gone"},
	} {
		d, err := s.GetDeployment(ctx, id)
		if err != nil || d.Branch != want.branch || d.Instance != want.instance {
			t.Errorf("%s: branch %q instance %q (%v), want branch %q instance %q", id, d.Branch, d.Instance, err, want.branch, want.instance)
		}
	}
}
