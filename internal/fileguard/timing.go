package fileguard

import (
	"context"
	"encoding/json"
	"io"
	"sync"
	"time"
)

// Timings belong to one Sync/Copy/Check call, never the shared Syncer or report
// used for policy approval. Accumulate durations before rounding to milliseconds.
type operationTiming struct {
	started time.Time
	phases  map[string]time.Duration
	pending sync.WaitGroup
}

func newOperationTiming() *operationTiming {
	t := &operationTiming{started: time.Now(), phases: make(map[string]time.Duration)}
	for _, phase := range []string{"prepare", "sourceInventory", "sourceDownload", "prepareOther", "upload", "targetCheck", "cleanup", "total"} {
		t.phases[phase] = 0
	}
	return t
}

func (t *operationTiming) execute(next command, phase string) command {
	return func(ctx context.Context, out io.Writer, args ...string) error {
		t.pending.Add(1)
		defer t.pending.Done()
		started := time.Now()
		defer func() { t.phases[phase] += time.Since(started) }()
		return next(ctx, out, args...)
	}
}

func (t *operationTiming) prepare(ctx context.Context, s *Syncer, src, dst string) (*snapshot, error) {
	started := time.Now()
	defer func() {
		// streamListing cancels its producer on an invalid listing, but may
		// return before the producer exits. Finish cancelled commands before
		// reading their timing or returning/cleaning the operation snapshot.
		t.pending.Wait()
		t.phases["prepare"] = time.Since(started)
		// Includes local inventory work, scans, policy callbacks and staging I/O;
		// sourceInventory/sourceDownload are subsets of prepare, not extra time.
		t.phases["prepareOther"] = t.phases["prepare"] - t.phases["sourceInventory"] - t.phases["sourceDownload"]
	}()
	if s == nil {
		return nil, ErrDenied
	}
	observed := *s
	observed.execute = func(ctx context.Context, out io.Writer, args ...string) error {
		phase := "sourceDownload"
		if args[0] == "lsjson" {
			phase = "sourceInventory"
		}
		return t.execute(s.execute, phase)(ctx, out, args...)
	}
	return observed.prepare(ctx, src, dst)
}

func (t *operationTiming) close(s *snapshot) error {
	if s == nil {
		return nil
	}
	started := time.Now()
	defer func() { t.phases["cleanup"] += time.Since(started) }()
	return s.close()
}

func (t *operationTiming) output(s *snapshot) string {
	t.phases["total"] = time.Since(t.started)
	evidence := struct {
		*Report
		TimingMillis map[string]int64 `json:"timingMillis"`
	}{TimingMillis: make(map[string]int64, len(t.phases))}
	if s != nil {
		evidence.Report = &s.report
	}
	for phase, elapsed := range t.phases {
		evidence.TimingMillis[phase] = elapsed.Milliseconds()
	}
	raw, _ := json.Marshal(evidence)
	return string(raw)
}
