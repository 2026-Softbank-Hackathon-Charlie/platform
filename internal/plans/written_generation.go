package plans

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
)

// WrittenGeneration reads the application collector's WAL-persisted identity.
// It never includes the DSN or provider error in its returned error.
func WrittenGeneration(ctx context.Context, dsn string) (string, error) {
	missing := false
	dsn = os.Expand(dsn, func(key string) string {
		v, ok := os.LookupEnv(key)
		if !ok || v == "" {
			missing = true
		}
		return v
	})
	if missing {
		return "", errors.New("source collection identity unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	c, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return "", errors.New("source collection identity unavailable")
	}
	defer c.Close(ctx)
	var generation string
	if err := c.QueryRow(ctx, `SELECT generation::text FROM app.write_collection_identity WHERE id=true`).Scan(&generation); err != nil {
		return "", errors.New("source collection identity unavailable")
	}
	return generation, nil
}
