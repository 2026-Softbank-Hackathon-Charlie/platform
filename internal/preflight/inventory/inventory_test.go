package inventory

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/preflight"
)

var scope = preflight.ClusterScope{Environment: "onprem-kr", Namespace: "shop", Cluster: "shop-db"}

func TestCollectRejectsBadDSNWithoutEchoingIt(t *testing.T) {
	ctx := context.Background()
	inv, res := Collector{}.Collect(ctx, "postgres://app:TOPSECRET@[bad", scope)
	if inv.Complete || res.Code != CodeInvalidDSN || inv.Scope != scope {
		t.Fatalf("%+v %+v", inv, res)
	}
	// Nothing listens on port 1; the collector reports a code, not the error.
	inv, res = Collector{Timeout: 3 * time.Second}.Collect(ctx, "postgres://app:TOPSECRET@127.0.0.1:1/app?sslmode=disable", scope)
	if inv.Complete || (res.Code != CodeConnectFailed && res.Code != CodeTimeout) || len(inv.Datasets) != 0 {
		t.Fatalf("%+v %+v", inv, res)
	}
}

// The tests below need a PostgreSQL superuser (CI's service container). They
// share the instance, so they run one after another and each cleans up.
func testDatabaseURL(t *testing.T) string {
	u := os.Getenv("PLATFORM_TEST_DATABASE_URL")
	if u == "" {
		t.Skip("PLATFORM_TEST_DATABASE_URL is not set")
	}
	return u
}

// newDatabase creates a database with a few relations in two schemas and
// returns its name and an admin connection to it.
func newDatabase(t *testing.T, base string) (string, *pgx.Conn) {
	t.Helper()
	ctx := context.Background()
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	name := "invtest_" + hex.EncodeToString(b)
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(ctx, "ALTER DATABASE "+name+" ALLOW_CONNECTIONS true")
		if _, err := admin.Exec(ctx, "DROP DATABASE "+name+" WITH (FORCE)"); err != nil {
			t.Errorf("drop %s: %v", name, err)
		}
		_ = admin.Close(ctx)
	})
	cfg, err := pgx.ParseConfig(base)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Database = name
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(ctx) })
	for _, sql := range []string{
		"CREATE SCHEMA sales",
		"CREATE TABLE public.orders (id int PRIMARY KEY)",
		"CREATE TABLE sales.customers (id int PRIMARY KEY, name text)",
		"CREATE TABLE sales.events (id int, at date) PARTITION BY RANGE (at)",
		"CREATE TABLE sales.events_2026 PARTITION OF sales.events FOR VALUES FROM ('2026-01-01') TO ('2027-01-01')",
		"CREATE MATERIALIZED VIEW sales.totals AS SELECT count(*) AS n FROM sales.customers",
		"CREATE VIEW sales.recent AS SELECT * FROM sales.customers", // no data of its own
		"CREATE SEQUENCE sales.ids",
	} {
		if _, err := conn.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	return name, conn
}

func TestCollectEveryDatabase(t *testing.T) {
	base := testDatabaseURL(t)
	ctx := context.Background()
	name, conn := newDatabase(t, base)

	t.Run("complete", func(t *testing.T) {
		inv, res := Collector{}.Collect(ctx, base, scope)
		if res.Code != "" || !inv.Complete || inv.Scope != scope {
			t.Fatalf("%+v %+v", inv, res)
		}
		// The DSN names another database; the copy carries this one too.
		for _, table := range []string{"public.orders", "sales.customers", "sales.events", "sales.events_2026", "sales.totals"} {
			schema, rel, _ := strings.Cut(table, ".")
			if !slices.Contains(inv.Datasets, preflight.Dataset{Database: name, Schema: schema, Table: rel}) {
				t.Errorf("missing %s.%s in %v", name, table, inv.Datasets)
			}
		}
		for _, d := range inv.Datasets {
			if d.Database == name && (d.Table == "recent" || d.Table == "ids") {
				t.Errorf("listed a relation without data: %+v", d)
			}
			if d.Schema == "pg_catalog" || d.Schema == "information_schema" {
				t.Errorf("listed a system relation: %+v", d)
			}
		}
		if !slices.IsSortedFunc(inv.Datasets, compare) {
			t.Error("datasets are not sorted")
		}
	})

	t.Run("unsupported identifier", func(t *testing.T) {
		if _, err := conn.Exec(ctx, `CREATE TABLE public."has space" (id int)`); err != nil {
			t.Fatal(err)
		}
		defer conn.Exec(ctx, `DROP TABLE public."has space"`)
		inv, res := Collector{}.Collect(ctx, base, scope)
		if inv.Complete || res.Code != CodeUnsupportedIdentifier || len(inv.Datasets) != 0 {
			t.Fatalf("%+v %+v", inv, res)
		}
	})

	t.Run("large objects", func(t *testing.T) {
		var oid uint32
		if err := conn.QueryRow(ctx, "SELECT lo_create(0)").Scan(&oid); err != nil {
			t.Fatal(err)
		}
		defer conn.Exec(ctx, "SELECT lo_unlink($1)", oid)
		inv, res := Collector{}.Collect(ctx, base, scope)
		if inv.Complete || res.Code != CodeLargeObjects {
			t.Fatalf("%+v %+v", inv, res)
		}
	})

	t.Run("database nobody can connect to", func(t *testing.T) {
		admin, err := pgx.Connect(ctx, base)
		if err != nil {
			t.Fatal(err)
		}
		defer admin.Close(ctx)
		if _, err := admin.Exec(ctx, "ALTER DATABASE "+name+" ALLOW_CONNECTIONS false"); err != nil {
			t.Fatal(err)
		}
		defer admin.Exec(ctx, "ALTER DATABASE "+name+" ALLOW_CONNECTIONS true")
		inv, res := Collector{}.Collect(ctx, base, scope)
		if inv.Complete || res.Code != CodeDatabaseNotReadable {
			t.Fatalf("%+v %+v", inv, res)
		}
	})
}
