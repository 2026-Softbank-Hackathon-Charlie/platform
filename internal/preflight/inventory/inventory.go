// Package inventory lists every relation a whole-cluster physical copy of a
// PostgreSQL instance would carry. It reads only the system catalogs, never row
// data, and reports failures as fixed codes: the DSN, host names and server
// error messages never appear in its results.
package inventory

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/preflight"
)

// Codes explaining an incomplete inventory. An empty Code means complete.
const (
	CodeInvalidDSN            = "invalid_dsn"
	CodeConnectFailed         = "connect_failed"
	CodeQueryFailed           = "query_failed"
	CodeTimeout               = "timeout"
	CodeDatabaseNotReadable   = "database_not_connectable"
	CodeUnsupportedIdentifier = "unsupported_identifier"
	CodeLargeObjects          = "large_objects_present"
	CodeEmpty                 = "no_relations"
)

// DefaultTimeout bounds one whole collection.
const DefaultTimeout = 30 * time.Second

// Result explains how complete an inventory is.
type Result struct {
	Code string // empty when the inventory is complete
}

// Collector reads an inventory from the instance a DSN reaches.
type Collector struct {
	// Timeout bounds the whole collection (DefaultTimeout when zero).
	Timeout time.Duration
}

// The engine's physical copy carries every database, so every one is listed.
// template0 is immutable and never accepts connections.
const databasesSQL = `SELECT datname, datallowconn FROM pg_catalog.pg_database WHERE datname <> 'template0' ORDER BY datname`

// Tables, partitioned parents and materialized views hold or describe data.
// Views, sequences, indexes and foreign tables do not hold relation data that
// a catalog can classify; system and temporary schemas belong to PostgreSQL.
const relationsSQL = `SELECT n.nspname, c.relname
FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind IN ('r', 'p', 'm')
  AND n.nspname NOT IN ('pg_catalog', 'information_schema')
  AND n.nspname NOT LIKE 'pg\_toast%'
  AND n.nspname NOT LIKE 'pg\_temp\_%'
ORDER BY 1, 2`

// Large objects live in a system table and cannot be classified per relation.
const largeObjectsSQL = `SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_largeobject_metadata)`

// Collect lists the relations of every database of the instance dsn reaches.
// scope is recorded as given: the caller vouches that dsn reaches that CNPG
// cluster (the same assumption the engine makes when it copies it).
// Complete is false whenever any database could not be read entirely.
func (c Collector) Collect(ctx context.Context, dsn string, scope preflight.ClusterScope) (preflight.Inventory, Result) {
	inv := preflight.Inventory{Scope: scope, Datasets: []preflight.Dataset{}}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	base, err := pgx.ParseConfig(dsn)
	if err != nil {
		return inv, Result{Code: CodeInvalidDSN} // the error can quote the DSN
	}
	limit := timeout
	if base.ConnectTimeout == 0 || base.ConnectTimeout > limit {
		base.ConnectTimeout = limit
	}
	if base.RuntimeParams == nil {
		base.RuntimeParams = map[string]string{}
	}
	base.RuntimeParams["default_transaction_read_only"] = "on"
	base.RuntimeParams["statement_timeout"] = "10000"
	base.RuntimeParams["application_name"] = "hopcharlie-residency-inventory"

	var dbs []string
	code := withConn(ctx, base, base.Database, func(conn *pgx.Conn) string {
		rows, err := conn.Query(ctx, databasesSQL)
		if err != nil {
			return CodeQueryFailed
		}
		var blocked bool
		for rows.Next() {
			var name string
			var allow bool
			if err := rows.Scan(&name, &allow); err != nil {
				rows.Close()
				return CodeQueryFailed
			}
			if !allow {
				blocked = true // copied physically, but nobody can list it
			}
			dbs = append(dbs, name)
		}
		if rows.Err() != nil {
			return CodeQueryFailed
		}
		if blocked {
			return CodeDatabaseNotReadable
		}
		return ""
	})
	if code != "" {
		return inv, Result{Code: fail(ctx, code)}
	}

	for _, db := range dbs {
		code := withConn(ctx, base, db, func(conn *pgx.Conn) string {
			var largeObjects bool
			if err := conn.QueryRow(ctx, largeObjectsSQL).Scan(&largeObjects); err != nil {
				return CodeQueryFailed
			}
			if largeObjects {
				return CodeLargeObjects
			}
			rows, err := conn.Query(ctx, relationsSQL)
			if err != nil {
				return CodeQueryFailed
			}
			for rows.Next() {
				d := preflight.Dataset{Database: db}
				if err := rows.Scan(&d.Schema, &d.Table); err != nil {
					rows.Close()
					return CodeQueryFailed
				}
				if !preflight.ValidDataset(d) {
					rows.Close()
					return CodeUnsupportedIdentifier
				}
				inv.Datasets = append(inv.Datasets, d)
			}
			if rows.Err() != nil {
				return CodeQueryFailed
			}
			return ""
		})
		if code != "" {
			inv.Datasets = []preflight.Dataset{}
			return inv, Result{Code: fail(ctx, code)}
		}
	}
	if len(inv.Datasets) == 0 {
		return inv, Result{Code: CodeEmpty}
	}
	slices.SortFunc(inv.Datasets, compare)
	inv.Complete = true
	return inv, Result{}
}

// withConn opens one connection to database db and closes it after fn.
func withConn(ctx context.Context, base *pgx.ConnConfig, db string, fn func(*pgx.Conn) string) string {
	cfg := base.Copy()
	cfg.Database = db
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return CodeConnectFailed
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		_ = conn.Close(closeCtx)
	}()
	return fn(conn)
}

// fail reports a deadline as a timeout rather than the step it interrupted.
func fail(ctx context.Context, code string) string {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return CodeTimeout
	}
	return code
}

func compare(a, b preflight.Dataset) int {
	return cmp.Or(cmp.Compare(a.Database, b.Database), cmp.Compare(a.Schema, b.Schema), cmp.Compare(a.Table, b.Table))
}
