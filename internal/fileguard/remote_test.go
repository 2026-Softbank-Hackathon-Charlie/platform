package fileguard

import (
	"context"
	"io"
	"slices"
	"testing"
)

// TestSyncerRemote: a syncer with a remote of its own (an onboarding's
// legacy bucket, ADR 0042) runs rclone with its variables, and the checks
// before each pass see them.
func TestSyncerRemote(t *testing.T) {
	r := Remote{Env: []string{"RCLONE_CONFIG_LEGACY_TYPE=s3"}, Buckets: []string{"legacy:b"}}
	var seen []Remote
	before := func(ctx context.Context) error { seen = append(seen, RemoteOf(ctx)); return nil }
	s := New("legacy:b", "eks:b", nil, "", before, func(context.Context, Report) error { return nil }).WithRemote(r)
	s.execute = func(ctx context.Context, _ io.Writer, args ...string) error {
		seen = append(seen, RemoteOf(ctx))
		return ErrTransfer // stop after the first command
	}
	_, _ = s.Sync(context.Background(), "legacy:b", "eks:b")
	if len(seen) < 2 {
		t.Fatalf("saw %d calls", len(seen))
	}
	for _, got := range seen {
		if !slices.Equal(got.Env, r.Env) || !slices.Equal(got.Buckets, r.Buckets) {
			t.Fatalf("a call saw remote %+v", got)
		}
	}
	if got := remoteEnvironment(append([]string{"RCLONE_DRY_RUN=true"}, RemoteOf(WithRemote(context.Background(), r)).Env...)); !slices.Equal(got, r.Env) {
		t.Fatalf("environment %v", got)
	}
}
