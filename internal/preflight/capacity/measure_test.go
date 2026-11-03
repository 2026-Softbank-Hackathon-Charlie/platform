package capacity

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"
)

func testDatabaseURL(t *testing.T) string {
	u := os.Getenv("PLATFORM_TEST_DATABASE_URL")
	if u == "" {
		t.Skip("PLATFORM_TEST_DATABASE_URL is not set")
	}
	return u
}

const unreachable = "postgres://app:TOPSECRET@127.0.0.1:1/app?sslmode=disable&connect_timeout=1"

func TestMeasureLiveSource(t *testing.T) {
	base := testDatabaseURL(t)
	ctx := context.Background()

	// An application session the probe must count, writing WAL during the window.
	app, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close(ctx)
	if _, err := app.Exec(ctx, "CREATE TEMP TABLE capacity_writes (id serial, v text)"); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			_, _ = app.Exec(ctx, "INSERT INTO capacity_writes (v) SELECT repeat('x', 1000) FROM generate_series(1, 200)")
		}
	}()
	defer func() { close(stop); <-done }()

	manifest := filepath.Join(t.TempDir(), "target.yaml")
	if err := os.WriteFile(manifest, []byte("spec:\n  postgresql:\n    parameters:\n      max_connections: \"250\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("new target from manifest", func(t *testing.T) {
		p := &plan.Plan{Source: plan.Endpoint{DSN: base}, Target: plan.Endpoint{DSN: unreachable, Manifest: manifest}}
		m := Prober{Window: time.Second}.Measure(ctx, p)
		if m.SourceBytes == nil || *m.SourceBytes <= 0 {
			t.Fatalf("source bytes %v", m.SourceBytes)
		}
		if m.WALBytesPerSecond == nil || *m.WALBytesPerSecond <= 0 {
			t.Fatalf("WAL rate %v while writing", m.WALBytesPerSecond)
		}
		if m.SourceSessions == nil || *m.SourceSessions < 1 || m.SourceMaxConnections == nil || *m.SourceMaxConnections <= 0 {
			t.Fatalf("sessions %v max %v", m.SourceSessions, m.SourceMaxConnections)
		}
		if m.TargetMaxConnections == nil || *m.TargetMaxConnections != 250 || m.TargetMaxConnectionsFrom != "manifest" {
			t.Fatalf("target max %v from %q", m.TargetMaxConnections, m.TargetMaxConnectionsFrom)
		}
		if m.LagBefore != nil || m.LagAfter != nil {
			t.Fatal("lag measured without a replica")
		}
		if m.Window < time.Second {
			t.Fatalf("window %s", m.Window)
		}
	})

	t.Run("running target that is not a replica", func(t *testing.T) {
		p := &plan.Plan{Source: plan.Endpoint{DSN: base}, Target: plan.Endpoint{DSN: base, Manifest: manifest}}
		m := Prober{Window: 200 * time.Millisecond}.Measure(ctx, p)
		if m.TargetMaxConnections == nil || *m.TargetMaxConnections != *m.SourceMaxConnections || m.TargetMaxConnectionsFrom != "target" {
			t.Fatalf("target max %v from %q", m.TargetMaxConnections, m.TargetMaxConnectionsFrom)
		}
		if m.LagBefore != nil {
			t.Fatal("a primary target has no replay lag")
		}
		r := Gate{Prober: Prober{Window: 200 * time.Millisecond}}
		p.Thresholds = plan.Thresholds{MaxLagBytes: 16 << 20, CatchupTimeout: plan.Duration{Duration: 10 * time.Minute}}
		rep, err := r.Check(ctx, p)
		if err != nil || check(rep, CheckCatchup).Status != Unknown {
			t.Fatalf("%+v %v", rep, err)
		}
	})
}

func TestMeasureUnreachableSourceMeasuresNothing(t *testing.T) {
	m := Prober{Window: 10 * time.Millisecond}.Measure(context.Background(), &plan.Plan{
		Source: plan.Endpoint{DSN: unreachable}, Target: plan.Endpoint{DSN: unreachable},
	})
	if m.SourceBytes != nil || m.SourceSessions != nil || m.WALBytesPerSecond != nil || m.TargetMaxConnections != nil {
		t.Fatalf("%+v", m)
	}
	if r := Evaluate(m, defaults); !r.Allowed {
		t.Fatalf("an unreachable source must not block: %+v", r)
	}
}
