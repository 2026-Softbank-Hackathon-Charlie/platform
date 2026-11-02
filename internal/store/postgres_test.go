package store_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store/storetest"
)

// testDatabaseURL returns PLATFORM_TEST_DATABASE_URL or skips the test.
func testDatabaseURL(t *testing.T) string {
	u := os.Getenv("PLATFORM_TEST_DATABASE_URL")
	if u == "" {
		t.Skip("PLATFORM_TEST_DATABASE_URL is not set")
	}
	return u
}

// newSchema creates an empty, uniquely named schema that is dropped when the
// test ends, and returns its name and a URL whose search_path points at it.
func newSchema(t *testing.T) (string, string) {
	t.Helper()
	base := testDatabaseURL(t)
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	schema := "storetest_" + hex.EncodeToString(b)

	exec := func(ctx context.Context, sql string) {
		t.Helper()
		conn, err := pgx.Connect(ctx, base)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close(ctx)
		if _, err := conn.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	ident := pgx.Identifier{schema}.Sanitize()
	exec(t.Context(), "CREATE SCHEMA "+ident)
	// t.Context is already cancelled when cleanups run.
	t.Cleanup(func() { exec(context.Background(), "DROP SCHEMA "+ident+" CASCADE") })

	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	// Subtests run in parallel; keep each pool small so they fit in max_connections.
	q.Set("pool_max_conns", "4")
	u.RawQuery = q.Encode()
	return schema, u.String()
}

func openSchema(t *testing.T, databaseURL string) *store.Postgres {
	t.Helper()
	s, err := store.Open(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestPostgresConformance(t *testing.T) {
	testDatabaseURL(t)
	storetest.Conformance(t, func(t *testing.T) store.Store {
		_, u := newSchema(t)
		return openSchema(t, u)
	})
}

func TestOpenTwiceIsNoop(t *testing.T) {
	schema, u := newSchema(t)
	ctx := t.Context()

	s := openSchema(t, u)
	if err := s.CreatePlan(ctx, store.Plan{Ref: "pln_1", Status: store.PlanPending}); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s = openSchema(t, u)
	defer s.Close()
	if _, err := s.GetPlan(ctx, "pln_1"); err != nil {
		t.Fatalf("plan lost after reopening: %v", err)
	}

	// The pool URL carries pool_max_conns, which a plain connection would send
	// to the server, so connect with the base URL and qualify names instead.
	conn, err := pgx.Connect(ctx, testDatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	rows, err := conn.Query(ctx, `SELECT version FROM `+pgx.Identifier{schema, "schema_migrations"}.Sanitize()+` ORDER BY version`)
	if err != nil {
		t.Fatal(err)
	}
	versions, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) != 1 || versions[0] != "0001_init" {
		t.Fatalf("schema_migrations = %v, want [0001_init]", versions)
	}
	// The tables went into the schema named by search_path, not public.
	var n int
	err = conn.QueryRow(ctx, `SELECT count(*) FROM information_schema.tables WHERE table_schema = $1`, schema).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	if n != 6 {
		t.Fatalf("%d tables in schema %s, want 6", n, schema)
	}
}

func TestOpenConcurrent(t *testing.T) {
	_, u := newSchema(t)
	const n = 4
	errs := make(chan error, n)
	for range n {
		go func() {
			s, err := store.Open(t.Context(), u)
			if err == nil {
				s.Close()
			}
			errs <- err
		}()
	}
	for range n {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent Open: %v", err)
		}
	}
}
