package runs

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/engine"
	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/plans"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

// writtenPage is how many records are read from the store at a time.
const writtenPage = 10000

// requireWritten refuses a plan whose verify step reads a written-records
// stream that has no records yet: verification would pass on no evidence.
func (r *Runner) requireWritten(ctx context.Context, p *plan.Plan) error {
	name, ok := plans.WrittenStream(p)
	if !ok {
		return nil
	}
	st, err := r.cfg.Store.WrittenRecordStats(ctx, name)
	if err != nil {
		return err
	}
	if st.Count == 0 {
		return fmt.Errorf("%w: verify.writtenRecords reads stream %s, which has no records yet; start the load generator before the migration", ErrInvalid, name)
	}
	if st.Scope == "application_commits" {
		generation, err := plans.WrittenGeneration(ctx, p.Source.DSN)
		if err != nil || generation == "" || generation != st.Generation {
			return fmt.Errorf("%w: written record source generation does not match this database", ErrInvalid)
		}
	}
	return nil
}

// useWrittenStream points a plan that reads its written records from a
// stream ("stream:<name>") at <stateDir>/<runID>.written.jsonl. A new run's
// id comes from the engine, so this runs right after the engine is built; the
// engine reads the path only when the verify step runs, through the same
// *plan.Plan.
func (r *Runner) useWrittenStream(l *live) {
	reason := "written_records_not_configured"
	if l.doc.Result.WriteVerification == nil {
		l.doc.Result.WriteVerification = &apiv1.WriteVerification{Status: apiv1.WriteVerificationStatusNotConfigured, Reason: &reason}
	}
	name, ok := plans.WrittenStream(l.plan)
	if !ok {
		if l.plan.Verify.WrittenRecords != "" {
			reason = "verify_not_reached"
			l.doc.Result.WriteVerification.Status = apiv1.WriteVerificationStatusNotReached
		}
		return
	}
	reason = "verify_not_reached"
	l.doc.Result.WriteVerification = &apiv1.WriteVerification{Status: apiv1.WriteVerificationStatusNotReached, Stream: &name, Reason: &reason}
	l.stream = name
	l.plan.Verify.WrittenRecords = filepath.Join(r.cfg.StateDir, l.id+".written.jsonl")
}

// writeWritten writes every record of the run's stream to the file the
// engine verifies, right before the verify step, so records that arrived
// during the cutover are included. When it fails the file is removed and the
// verify step reports the records as unreadable instead of passing.
func (r *Runner) writeWritten(ctx context.Context, l *live) {
	if l.stream == "" {
		return
	}
	path := l.plan.Verify.WrittenRecords
	stats, statErr := r.cfg.Store.WrittenRecordStats(ctx, l.stream)
	var n int64
	err := statErr
	if err == nil {
		n, err = writeWrittenFileUntil(ctx, r.cfg.Store, l.stream, path, stats.LastSeq)
	}
	if err == nil && (n == 0 || n != stats.Count) {
		err = fmt.Errorf("written record snapshot is empty or changed during export")
	}
	l.mu.Lock()
	if stats.Scope == "" {
		stats.Scope = "unspecified"
	}
	if stats.Coverage == "" {
		stats.Coverage = "reported_records"
	}
	scope := apiv1.WriteVerificationScope(stats.Scope)
	coverage := apiv1.WriteVerificationCoverage(stats.Coverage)
	reason := "verify_not_reached"
	l.doc.Result.WriteVerification = &apiv1.WriteVerification{Status: apiv1.WriteVerificationStatusNotReached, Stream: &l.stream, CollectedRecords: &stats.Count, CutoffSeq: &stats.LastSeq, LatestAt: stats.LatestAt, Scope: &scope, Coverage: &coverage, Reason: &reason}
	if err != nil {
		reason = "written_records_export_failed"
		l.doc.Result.WriteVerification.Status = apiv1.WriteVerificationStatusFailed
		if stats.Count == 0 && statErr == nil {
			reason = "no_records_collected"
			l.doc.Result.WriteVerification.Status = apiv1.WriteVerificationStatusNoRecords
		}
	}
	l.mu.Unlock()
	if err != nil {
		_ = os.Remove(path) // never verify against an earlier attempt's records
		r.cfg.Log.Error("write written records", "run", l.id, "stream", l.stream, "err", err)
		l.emitPlatform(engine.EventLog, engine.StepVerify,
			"WARNING: could not prepare the written records of stream "+l.stream+"; verify will report them as unreadable (see the server log)",
			map[string]any{"stream": l.stream})
		return
	}
	l.emitPlatform(engine.EventLog, engine.StepVerify, fmt.Sprintf("%d written records of stream %s are ready for verification", n, l.stream),
		map[string]any{"stream": l.stream, "records": n})
}

// writtenLine is the engine's written-records format (verify.ReadWritten).
type writtenLine struct {
	Table string `json:"table"`
	PK    string `json:"pk"`
}

// writeWrittenFile replaces path with the stream's records as JSONL, mode 0600.
func writeWrittenFile(ctx context.Context, st store.Store, stream, path string) (int64, error) {
	return writeWrittenFileUntil(ctx, st, stream, path, 0)
}

func writeWrittenFileUntil(ctx context.Context, st store.Store, stream, path string, cutoff int64) (int64, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return 0, err
	}
	f, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*") // mode 0600
	if err != nil {
		return 0, err
	}
	n, err := writeRecordsUntil(ctx, st, stream, f, cutoff)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	if err != nil {
		_ = os.Remove(f.Name())
		return 0, err
	}
	return n, nil
}

func writeRecords(ctx context.Context, st store.Store, stream string, f *os.File) (int64, error) {
	return writeRecordsUntil(ctx, st, stream, f, 0)
}

func writeRecordsUntil(ctx context.Context, st store.Store, stream string, f *os.File, cutoff int64) (int64, error) {
	w := bufio.NewWriter(f)
	enc := json.NewEncoder(w) // one object per line
	var n, after int64
	for {
		page, err := st.ListWrittenRecords(ctx, stream, after, writtenPage)
		if err != nil {
			return n, err
		}
		for _, rec := range page {
			if cutoff > 0 && rec.Seq > cutoff {
				return n, w.Flush()
			}
			if err := enc.Encode(writtenLine{Table: rec.Table, PK: rec.PK}); err != nil {
				return n, err
			}
			after = rec.Seq
			n++
		}
		if len(page) < writtenPage {
			return n, w.Flush()
		}
	}
}
