package api

import (
	"context"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/plans"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
)

type writtenStatsReader interface {
	WrittenRecordStats(context.Context, string) (store.WrittenStats, error)
}

// automaticWrittenStream separates collector liveness from when a user last
// committed a write. Scope and both database generations remain mandatory.
func automaticWrittenStream(ctx context.Context, st writtenStatsReader, app, source, target string, now time.Time, readGeneration func(context.Context, string) (string, error)) (string, string, error) {
	stream := "app-" + app
	if !plans.StreamName.MatchString(stream) {
		return "", "invalid_stream", nil
	}
	stats, err := st.WrittenRecordStats(ctx, stream)
	if err != nil {
		return "", "stats_unavailable", err
	}
	if stats.Count <= 0 {
		return "", "no_records", nil
	}
	if stats.Scope != "application_commits" || stats.Coverage != "reservation_inserts" || stats.Generation == "" {
		return "", "collector_identity_unknown", nil
	}
	if stats.LastReceivedAt == nil {
		return "", "collector_activity_unknown", nil
	}
	if !stats.LastReceivedAt.After(now.Add(-10*time.Minute)) || stats.LastReceivedAt.After(now) {
		return "", "collector_activity_stale", nil
	}
	sourceGeneration, err := readGeneration(ctx, source)
	if err != nil {
		return "", "source_identity_unavailable", nil
	}
	if sourceGeneration != stats.Generation {
		return "", "source_generation_mismatch", nil
	}
	targetGeneration, err := readGeneration(ctx, target)
	if err != nil {
		return "", "target_identity_unavailable", nil
	}
	if targetGeneration != stats.Generation {
		return "", "target_generation_mismatch", nil
	}
	return stream, "", nil
}
