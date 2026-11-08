package api

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
)

type writtenStatsFixture struct{ stats store.WrittenStats }

func (f writtenStatsFixture) WrittenRecordStats(context.Context, string) (store.WrittenStats, error) {
	return f.stats, nil
}

func TestQuietCollectorBindsOnlyWithFreshAuthenticatedActivityAndBothDatabaseIdentities(t *testing.T) {
	now := time.Now().UTC()
	old := now.Add(-time.Hour)
	fresh := now.Add(-time.Minute)
	base := store.WrittenStats{Count: 132, Scope: "application_commits", Coverage: "reservation_inserts", Generation: "db-1", LatestAt: &old, LastReceivedAt: &fresh}
	for _, tc := range []struct {
		name           string
		change         func(*store.WrittenStats)
		source, target string
		want           string
	}{
		{"quiet but live", func(*store.WrittenStats) {}, "db-1", "db-1", "app-moimroom-mig"},
		{"no collector evidence", func(s *store.WrittenStats) { s.LastReceivedAt = nil }, "db-1", "db-1", ""},
		{"collector stopped", func(s *store.WrittenStats) { s.LastReceivedAt = &old }, "db-1", "db-1", ""},
		{"no records", func(s *store.WrittenStats) { s.Count = 0 }, "db-1", "db-1", ""},
		{"acknowledgements only", func(s *store.WrittenStats) { s.Scope = "client_acknowledged" }, "db-1", "db-1", ""},
		{"source reset", func(*store.WrittenStats) {}, "db-2", "db-1", ""},
		{"target reset", func(*store.WrittenStats) {}, "db-1", "db-2", ""},
		{"target unavailable", func(*store.WrittenStats) {}, "db-1", "error", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := base
			tc.change(&st)
			stream, _, err := automaticWrittenStream(t.Context(), writtenStatsFixture{st}, "moimroom-mig", "source", "target", now, func(_ context.Context, dsn string) (string, error) {
				generation := tc.source
				if dsn == "target" {
					generation = tc.target
				}
				if generation == "error" {
					return "", errors.New("identity unavailable")
				}
				return generation, nil
			})
			if err != nil || stream != tc.want {
				t.Fatalf("stream=%q error=%v want=%q", stream, err, tc.want)
			}
		})
	}
}
