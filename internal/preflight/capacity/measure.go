package capacity

import (
	"context"
	"os"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"sigs.k8s.io/yaml"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"
)

// Defaults for Prober.
const (
	DefaultWindow  = 5 * time.Second
	DefaultTimeout = 20 * time.Second
	appName        = "hopcharlie-capacity-preflight"
)

// Prober reads Measurements from a plan's source and target databases. It
// runs read-only catalog and status queries; errors are dropped, never
// reported, so DSNs and server messages stay out of the result.
type Prober struct {
	Window  time.Duration // between the two lag and WAL samples (DefaultWindow)
	Timeout time.Duration // the whole measurement (DefaultTimeout)
}

// Gate measures a plan and judges it (plans.CapacityGate).
type Gate struct{ Prober Prober }

// Check measures p, whose DSNs are expanded and whose target manifest is a
// local file (plans.Service.Reserve), and judges it with p's thresholds.
func (g Gate) Check(ctx context.Context, p *plan.Plan) (Report, error) {
	m := g.Prober.Measure(ctx, p)
	return Evaluate(m, Thresholds{MaxLagBytes: p.Thresholds.MaxLagBytes, CatchupTimeout: p.Thresholds.CatchupTimeout.Duration}), ctx.Err()
}

// Measure reads what it can and leaves the rest nil.
func (pr Prober) Measure(ctx context.Context, p *plan.Plan) Measurements {
	window, timeout := pr.Window, pr.Timeout
	if window <= 0 {
		window = DefaultWindow
	}
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	m := Measurements{}
	src := connect(ctx, p.Source.DSN)
	if src != nil {
		defer closeConn(ctx, src)
	}
	dst := connect(ctx, p.Target.DSN)
	if dst != nil {
		defer closeConn(ctx, dst)
	}

	if dst != nil {
		if n, ok := showInt(ctx, dst, "max_connections"); ok {
			m.TargetMaxConnections, m.TargetMaxConnectionsFrom = &n, "target"
		}
	}
	if dst != nil {
		if n, ok := showInt(ctx, dst, "max_worker_processes"); ok {
			m.TargetMaxWorkerProcesses, m.TargetMaxWorkerProcessesFrom = &n, "target"
		}
	}
	if m.TargetMaxConnections == nil {
		if n, ok := ManifestMaxConnections(p.Target.Manifest); ok {
			m.TargetMaxConnections, m.TargetMaxConnectionsFrom = &n, "manifest"
		}
	}
	if m.TargetMaxWorkerProcesses == nil {
		if n, ok := ManifestParameter(p.Target.Manifest, "max_worker_processes"); ok {
			m.TargetMaxWorkerProcesses, m.TargetMaxWorkerProcessesFrom = &n, "manifest"
		}
	}
	if src == nil {
		return m
	}

	var size int64
	if src.QueryRow(ctx, `SELECT coalesce(sum(pg_database_size(datname)), 0)::bigint
		FROM pg_catalog.pg_database WHERE datallowconn`).Scan(&size) == nil {
		m.SourceBytes = &size
	}
	var sessions int
	if src.QueryRow(ctx, `SELECT count(*) FROM pg_catalog.pg_stat_activity
		WHERE backend_type = 'client backend' AND application_name <> $1`, appName).Scan(&sessions) == nil {
		m.SourceSessions = &sessions
	}
	if n, ok := showInt(ctx, src, "max_connections"); ok {
		m.SourceMaxConnections = &n
	}
	if n, ok := showInt(ctx, src, "max_worker_processes"); ok {
		m.SourceMaxWorkerProcesses = &n
	}

	// Two samples a window apart: the source's WAL position and, when the
	// target is a streaming replica, its replay position (engine's pg.Lag).
	replica := dst != nil && inRecovery(ctx, dst)
	src0, ok0 := lsn(ctx, src, "pg_current_wal_lsn()")
	var dst0 string
	if replica {
		dst0, replica = lsn(ctx, dst, "pg_last_wal_replay_lsn()")
	}
	start := time.Now()
	select {
	case <-ctx.Done():
		return m
	case <-time.After(window):
	}
	src1, ok1 := lsn(ctx, src, "pg_current_wal_lsn()")
	var dst1 string
	if replica {
		dst1, replica = lsn(ctx, dst, "pg_last_wal_replay_lsn()")
	}
	elapsed := time.Since(start)
	m.Window = elapsed
	if ok0 && ok1 {
		if d, ok := lsnDiff(ctx, src, src1, src0); ok && d >= 0 {
			rate := float64(d) / elapsed.Seconds()
			m.WALBytesPerSecond = &rate
		}
	}
	if replica && ok0 && ok1 {
		before, okB := lsnDiff(ctx, src, src0, dst0)
		after, okA := lsnDiff(ctx, src, src1, dst1)
		if okB && okA {
			m.LagBefore, m.LagAfter = &before, &after
		}
	}
	return m
}

// SourceStats is what MeasureSource read from a source database: numbers only,
// so a planner can size the target without the database's address or password.
type SourceStats struct {
	Sessions           int
	MaxConnections     int
	MaxWorkerProcesses int
	Bytes              *int64   // nil: not measured
	WALBytesPerSecond  *float64 // nil: not measured
	Window             time.Duration
	MeasuredAt         time.Time
}

// MeasureSource measures one source database with the same queries Measure
// (and so the capacity gate) uses. ok is false when it could not connect or
// read the connection settings; the reason is not returned, so the DSN and
// server messages stay out of the result.
func (pr Prober) MeasureSource(ctx context.Context, dsn string) (SourceStats, bool) {
	at := time.Now().UTC()
	m := pr.Measure(ctx, &plan.Plan{Source: plan.Endpoint{DSN: dsn}})
	if m.SourceSessions == nil || m.SourceMaxConnections == nil || m.SourceMaxWorkerProcesses == nil {
		return SourceStats{}, false
	}
	return SourceStats{
		Sessions:           *m.SourceSessions,
		MaxConnections:     *m.SourceMaxConnections,
		MaxWorkerProcesses: *m.SourceMaxWorkerProcesses,
		Bytes:              m.SourceBytes,
		WALBytesPerSecond:  m.WALBytesPerSecond,
		Window:             m.Window,
		MeasuredAt:         at,
	}, true
}

// ManifestMaxConnections reads spec.postgresql.parameters.max_connections
// from a CNPG Cluster manifest file. It does not assume a default.
func ManifestMaxConnections(path string) (int, bool) {
	return ManifestParameter(path, "max_connections")
}

// ManifestParameter reads a positive integer spec.postgresql.parameters.<name>
// from a CNPG Cluster manifest file. It does not assume a default.
func ManifestParameter(path, name string) (int, bool) {
	if path == "" {
		return 0, false
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	var doc struct {
		Spec struct {
			PostgreSQL struct {
				Parameters map[string]string `json:"parameters"`
			} `json:"postgresql"`
		} `json:"spec"`
	}
	if yaml.Unmarshal(raw, &doc) != nil {
		return 0, false
	}
	v, ok := doc.Spec.PostgreSQL.Parameters[name]
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

func connect(ctx context.Context, dsn string) *pgx.Conn {
	if dsn == "" {
		return nil
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil
	}
	if cfg.ConnectTimeout == 0 || cfg.ConnectTimeout > 5*time.Second {
		cfg.ConnectTimeout = 5 * time.Second
	}
	if cfg.RuntimeParams == nil {
		cfg.RuntimeParams = map[string]string{}
	}
	cfg.RuntimeParams["default_transaction_read_only"] = "on"
	cfg.RuntimeParams["statement_timeout"] = "5000"
	cfg.RuntimeParams["application_name"] = appName
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil
	}
	return conn
}

func closeConn(ctx context.Context, c *pgx.Conn) {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	_ = c.Close(cctx)
}

func showInt(ctx context.Context, c *pgx.Conn, name string) (int, bool) {
	var v string
	if c.QueryRow(ctx, "SELECT current_setting($1)", name).Scan(&v) != nil {
		return 0, false
	}
	n, err := strconv.Atoi(v)
	return n, err == nil
}

func inRecovery(ctx context.Context, c *pgx.Conn) bool {
	var rec bool
	return c.QueryRow(ctx, "SELECT pg_is_in_recovery()").Scan(&rec) == nil && rec
}

// lsn reads one of two fixed position functions; fn is never caller input.
func lsn(ctx context.Context, c *pgx.Conn, fn string) (string, bool) {
	var v *string
	if c.QueryRow(ctx, "SELECT "+fn+"::text").Scan(&v) != nil || v == nil {
		return "", false
	}
	return *v, true
}

func lsnDiff(ctx context.Context, c *pgx.Conn, a, b string) (int64, bool) {
	var n int64
	if c.QueryRow(ctx, "SELECT pg_wal_lsn_diff($1::pg_lsn, $2::pg_lsn)::bigint", a, b).Scan(&n) != nil {
		return 0, false
	}
	return n, true
}
