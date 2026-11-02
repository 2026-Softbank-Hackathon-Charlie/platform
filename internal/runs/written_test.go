package runs

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/engine"
	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store/storetest"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

// Start and Resume both refuse a plan that verifies an empty written-records
// stream, before any engine is built.
func TestRunNeedsWrittenRecords(t *testing.T) {
	ctx := context.Background()
	p, err := plan.Parse([]byte(`{"id":"p1","source":{"env":"gke","namespace":"n","cluster":"a","dsn":"postgres://x@a/db"},` +
		`"target":{"env":"eks","namespace":"n","cluster":"b","dsn":"postgres://x@b/db"},"verify":{"writtenRecords":"stream:demo"}}`))
	if err != nil {
		t.Fatal(err)
	}
	st := storetest.NewMemory()
	factory := func(*plan.Plan, engine.Options) (Engine, error) { return nil, errors.New("no engine may be built") }
	r := New(ctx, Config{StateDir: t.TempDir(), Store: st, Factory: factory})

	_, err = r.Start(ctx, StartOptions{PlanRef: "pln_1", Plan: p, Actor: "op"})
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "stream demo, which has no records yet") {
		t.Fatalf("start: %v", err)
	}

	now := time.Now().UTC()
	doc, _ := json.Marshal(apiv1.Run{RunId: "run_1", PlanRef: "pln_1", Status: apiv1.RunStatusFailed, StartedAt: now, UpdatedAt: now,
		Engine: apiv1.EngineState{RunId: "run_1", Outcome: apiv1.EngineOutcomeFailed}})
	if err := st.CreateRun(ctx, store.Run{RunID: "run_1", PlanRef: "pln_1", Status: string(apiv1.RunStatusFailed), Doc: doc, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Resume(ctx, "run_1", "op", p); !errors.Is(err, ErrInvalid) {
		t.Fatalf("resume: %v", err)
	}

	if err := st.AppendWrittenRecords(ctx, "demo", []store.WrittenRecord{{Table: "app.t", PK: "1", At: now, ReceivedAt: now}}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Start(ctx, StartOptions{PlanRef: "pln_1", Plan: p, Actor: "op"}); err == nil || errors.Is(err, ErrInvalid) {
		t.Fatalf("start with records got past the check to the engine: %v", err)
	}
}
