package store

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// ClusterLease serializes migrations and source cleanup across API processes.
// It is session scoped; a lost connection must stop work, not silently re-lock.
type ClusterLease interface {
	Check(context.Context) error
	Release()
}

type clusterLease struct {
	conn *pgx.Conn
	keys []string
	once sync.Once
}

func (p *Postgres) TryClusterLease(ctx context.Context, keys []string) (ClusterLease, error) {
	keys = slices.Clone(keys)
	slices.Sort(keys)
	keys = slices.Compact(keys)
	// A dedicated session cannot exhaust the store pool while long migrations
	// hold their locks and the event writer still needs a pooled connection.
	conn, err := pgx.ConnectConfig(ctx, p.pool.Config().ConnConfig.Copy())
	if err != nil {
		return nil, err
	}
	l := &clusterLease{conn: conn}
	for _, key := range keys {
		var ok bool
		if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock(hashtextextended($1, 0))", "hopcharlie-cluster/"+key).Scan(&ok); err != nil {
			l.Release()
			return nil, err
		}
		if !ok {
			l.Release()
			return nil, fmt.Errorf("%w: cluster lease is held", ErrConflict)
		}
		l.keys = append(l.keys, key)
	}
	return l, nil
}

func (l *clusterLease) Check(ctx context.Context) error { return l.conn.Ping(ctx) }
func (l *clusterLease) Release() {
	l.once.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for _, key := range l.keys {
			if _, err := l.conn.Exec(ctx, "SELECT pg_advisory_unlock(hashtextextended($1, 0))", "hopcharlie-cluster/"+key); err != nil {
				_ = l.conn.Close(ctx)
				return
			}
		}
		_ = l.conn.Close(ctx)
	})
}
