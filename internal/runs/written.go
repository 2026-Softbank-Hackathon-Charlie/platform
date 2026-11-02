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
	return nil
}

// useWrittenStream points a plan that reads its written records from a
// stream ("stream:<name>") at <stateDir>/<runID>.written.jsonl. A new run's
// id comes from the engine, so this runs right after the engine is built; the
// engine reads the path only when the verify step runs, through the same
// *plan.Plan.
func (r *Runner) useWrittenStream(l *live) {
	name, ok := plans.WrittenStream(l.plan)
	if !ok {
		return
	}
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
	n, err := writeWrittenFile(ctx, r.cfg.Store, l.stream, path)
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
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return 0, err
	}
	f, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*") // mode 0600
	if err != nil {
		return 0, err
	}
	n, err := writeRecords(ctx, st, stream, f)
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
	w := bufio.NewWriter(f)
	enc := json.NewEncoder(w) // one object per line
	var n, after int64
	for {
		page, err := st.ListWrittenRecords(ctx, stream, after, writtenPage)
		if err != nil {
			return n, err
		}
		for _, rec := range page {
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
