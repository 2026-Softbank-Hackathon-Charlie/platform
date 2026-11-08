package job

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/fileguard"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/onboard"
)

// PG is SourceDB with pgx.
type PG struct{}

func dsn(host, database, user, password string) string {
	u := url.URL{Scheme: "postgres", User: url.UserPassword(user, password), Host: host, Path: "/" + database, RawQuery: "sslmode=prefer&connect_timeout=10"}
	return u.String()
}

// Check implements SourceDB.
func (PG) Check(ctx context.Context, host, database, user, password, replicationPassword string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	c, err := pgx.Connect(ctx, dsn(host, database, user, password))
	if err != nil {
		return fmt.Errorf("connect as %s: %w", user, err)
	}
	_ = c.Close(ctx)
	cfg, err := pgconn.ParseConfig(dsn(host, database, onboard.ReplicationUser, replicationPassword))
	if err != nil {
		return err
	}
	cfg.RuntimeParams["replication"] = "database"
	rc, err := pgconn.ConnectConfig(ctx, cfg)
	if err != nil {
		return fmt.Errorf("replication connection: %w", err)
	}
	defer rc.Close(ctx)
	if _, err := rc.Exec(ctx, "IDENTIFY_SYSTEM").ReadAll(); err != nil {
		return fmt.Errorf("IDENTIFY_SYSTEM: %w", err)
	}
	return nil
}

// Replication implements SourceDB.
func (PG) Replication(ctx context.Context, host, database, user, password string) (bool, time.Duration, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	c, err := pgx.Connect(ctx, dsn(host, database, user, password))
	if err != nil {
		return false, 0, err
	}
	defer c.Close(ctx)
	var state string
	var lag float64
	err = c.QueryRow(ctx, `SELECT state, COALESCE(EXTRACT(EPOCH FROM replay_lag), 0)::float8 FROM pg_stat_replication
		WHERE usename = $1 ORDER BY backend_start DESC LIMIT 1`, onboard.ReplicationUser).Scan(&state, &lag)
	if err == pgx.ErrNoRows {
		return false, 0, nil
	}
	if err != nil {
		return false, 0, err
	}
	return state == "streaming", time.Duration(lag * float64(time.Second)), nil
}

// ReadOnly implements SourceDB.
func (PG) ReadOnly(ctx context.Context, host, database, user, password string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	c, err := pgx.Connect(ctx, dsn(host, database, user, password))
	if err != nil {
		return false, err
	}
	defer c.Close(ctx)
	var v string
	if err := c.QueryRow(ctx, "SHOW default_transaction_read_only").Scan(&v); err != nil {
		return false, err
	}
	return v == "on", nil
}

// DropRole implements SourceDB.
func (PG) DropRole(ctx context.Context, dsn, role string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	c, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return err
	}
	defer c.Close(ctx)
	_, err = c.Exec(ctx, "DROP ROLE IF EXISTS "+pgx.Identifier{role}.Sanitize())
	return err
}

// Rclone is Files with fileguard.Plain: the server's remotes (the target's
// RCLONE_CONFIG_*, AWS_*, GOOGLE_* credentials) and env, the legacy remote,
// without any *_PGPASSWORD and with no config file.
type Rclone struct{}

var copied = regexp.MustCompile(`Transferred:\s+(\d+) /`)

// Copy implements Files: rclone copy, then rclone check --one-way for what
// still differs.
func (Rclone) Copy(ctx context.Context, env []string, src, dst string) (int, int, error) {
	p := fileguard.NewPlain(env)
	res, err := p.Copy(ctx, src, dst)
	if err != nil {
		return 0, 0, err
	}
	n := 0
	if m := copied.FindStringSubmatch(res.Output); m != nil {
		n, _ = strconv.Atoi(m[1])
	}
	check, err := p.Check(ctx, src, dst)
	if err != nil {
		return n, 0, err
	}
	return n, check.Differences + check.Missing, nil
}

// Resolver is DNS with the system resolver: host points at the gateway
// when it resolves to what a name only the wildcard answers resolves to.
type Resolver struct {
	Domain string // hopcharlie.dev
	R      *net.Resolver
}

// PointsAtGateway implements DNS.
func (d Resolver) PointsAtGateway(ctx context.Context, host, vmIP string) (bool, error) {
	r := d.R
	if r == nil {
		r = net.DefaultResolver
	}
	got, err := r.LookupHost(ctx, host)
	if err != nil {
		return false, err
	}
	want, err := r.LookupHost(ctx, "onb-wildcard-probe."+strings.TrimPrefix(d.Domain, "."))
	if err != nil {
		return false, err
	}
	if vmIP != "" && slices.Contains(got, vmIP) {
		return false, nil
	}
	slices.Sort(got)
	slices.Sort(want)
	return slices.Equal(got, want), nil
}
