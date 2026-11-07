package runs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/engine"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store/storetest"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

func TestWriteVerificationDoesNotReportEmptyOrPartialAsZero(t *testing.T) {
	for _, tc := range []struct {
		name    string
		checked int
		errors  []string
		status  apiv1.WriteVerificationStatus
	}{
		{"empty", 0, nil, apiv1.WriteVerificationStatusNoRecords},
		{"failed", 2, []string{"target unavailable"}, apiv1.WriteVerificationStatusFailed},
		{"measured", 2, nil, apiv1.WriteVerificationStatusMeasured},
	} {
		t.Run(tc.name, func(t *testing.T) {
			evidence := map[string]any{"report": map[string]any{"ok": true, "written": map[string]any{"checked": tc.checked, "missing": 0, "errors": tc.errors}}}
			res := apiv1.RunResult{}
			fillResult(&res, apiv1.EngineState{Steps: map[string]apiv1.StepRecord{engine.StepVerify: {Evidence: &evidence}}}, time.Now(), time.Now())
			if res.WriteVerification == nil || res.WriteVerification.Status != tc.status {
				t.Fatalf("status %+v", res.WriteVerification)
			}
			if (res.MissingWrites != nil) != (tc.status == apiv1.WriteVerificationStatusMeasured) {
				t.Fatalf("misleading zero: %+v", res)
			}
		})
	}
}

func TestWrittenSnapshotExcludesLateReports(t *testing.T) {
	st := storetest.NewMemory()
	ctx := t.Context()
	now := time.Now()
	if err := st.AppendWrittenRecords(ctx, "demo", []store.WrittenRecord{{Table: "app.reservations", PK: "1", At: now, ReceivedAt: now}}); err != nil {
		t.Fatal(err)
	}
	snap, err := st.WrittenRecordStats(ctx, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AppendWrittenRecords(ctx, "demo", []store.WrittenRecord{{Table: "app.reservations", PK: "2", At: now, ReceivedAt: now}}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "written.jsonl")
	n, err := writeWrittenFileUntil(ctx, st, "demo", path, snap.LastSeq)
	if err != nil || n != snap.Count {
		t.Fatalf("snapshot %d %v", n, err)
	}
	b, err := os.ReadFile(path)
	if err != nil || strings.Contains(string(b), `"pk":"2"`) {
		t.Fatalf("late receipt crossed frozen cutoff: %s %v", b, err)
	}
}
