package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// Leadership is the API process that runs the background work: migration
// runs, deployments, onboardings and the reconcile loops (ADR 0065). Like
// ClusterLease it is a session-scoped advisory lock on a dedicated
// connection, so it ends with the session: a process that dies, or whose
// connection the server drops, holds it no longer.
type Leadership interface {
	Check(context.Context) error
	Release()
}

// leaderAppPrefix marks the leader's session in pg_stat_activity; the rest
// of application_name is the leader's HTTP address (LeaderAddr).
const leaderAppPrefix = "hopcharlie-leader/"

type leadership struct {
	conn *pgx.Conn
	once sync.Once
}

// TryLeadership takes the leadership called name, or answers ErrConflict
// while another session holds it. addr is the holder's HTTP address
// (host:port), which other processes read with LeaderAddr to forward the
// requests only the leader serves.
func (p *Postgres) TryLeadership(ctx context.Context, name, addr string) (Leadership, error) {
	app := leaderAppPrefix + name + " " + addr
	if name == "" || addr == "" || strings.ContainsAny(name+addr, " \x00") || len(app) > 63 {
		return nil, fmt.Errorf("invalid leadership name %q or address %q", name, addr)
	}
	conn, err := pgx.ConnectConfig(ctx, p.pool.Config().ConnConfig.Copy())
	if err != nil {
		return nil, err
	}
	l := &leadership{conn: conn}
	// The server notices a leader whose node vanished within about 25s
	// instead of the kernel's default of hours, and frees the lock.
	for _, q := range []string{"SET tcp_keepalives_idle = 10", "SET tcp_keepalives_interval = 5", "SET tcp_keepalives_count = 3"} {
		if _, err := conn.Exec(ctx, q); err != nil {
			l.Release()
			return nil, err
		}
	}
	var ok bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock(hashtextextended($1, 0))", "hopcharlie-leader/"+name).Scan(&ok); err != nil {
		l.Release()
		return nil, err
	}
	if !ok {
		l.Release()
		return nil, fmt.Errorf("%w: leadership %s is held", ErrConflict, name)
	}
	// Only after the lock: a session with this name is always the holder.
	if _, err := conn.Exec(ctx, "SELECT set_config('application_name', $1, false)", app); err != nil {
		l.Release()
		return nil, err
	}
	return l, nil
}

// LeaderAddr returns the HTTP address of the holder of leadership name, or
// "" when no session holds it.
func (p *Postgres) LeaderAddr(ctx context.Context, name string) (string, error) {
	var app string
	err := p.pool.QueryRow(ctx,
		"SELECT application_name FROM pg_stat_activity WHERE starts_with(application_name, $1) ORDER BY backend_start DESC LIMIT 1",
		leaderAppPrefix+name+" ").Scan(&app)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return strings.TrimPrefix(app, leaderAppPrefix+name+" "), nil
}

func (l *leadership) Check(ctx context.Context) error { return l.conn.Ping(ctx) }

// Release drops the name, then the lock, then the session; closing alone
// frees both too, but only once the server has ended the backend.
func (l *leadership) Release() {
	l.once.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = l.conn.Exec(ctx, "SELECT set_config('application_name', '', false), pg_advisory_unlock_all()")
		_ = l.conn.Close(ctx)
	})
}
