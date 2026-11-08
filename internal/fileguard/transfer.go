package fileguard

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/files"
)

const operationTimeout = 10 * time.Minute

type object struct {
	Path  string
	Size  int64
	IsDir bool
}

type command func(context.Context, io.Writer, ...string) error

// run ignores config files and raw process output. Remote credentials come only
// from the server environment, the same source bucketprobe measures.
func run(ctx context.Context, out io.Writer, args ...string) error {
	args = append(args, "--config", os.DevNull, "--log-level", "ERROR", "--log-file", "")
	cmd := exec.CommandContext(ctx, "rclone", args...)
	cmd.Env = remoteEnvironment(os.Environ())
	cmd.WaitDelay = 5 * time.Second // bound pipe teardown after process cancellation
	cmd.Stdout, cmd.Stderr = out, io.Discard
	if err := cmd.Run(); err != nil {
		return ErrTransfer
	}
	return nil
}

// Global rclone environment flags (filters, dry-run, links, files-from, etc.)
// must not change the inspected/copied set. Keep only remote/backend settings.
func remoteEnvironment(env []string) []string {
	out := make([]string, 0, len(env))
	for _, v := range env {
		key, _, _ := strings.Cut(v, "=")
		key = strings.ToUpper(key)
		if strings.HasPrefix(key, "RCLONE_") && !strings.HasPrefix(key, "RCLONE_CONFIG_") && !strings.HasPrefix(key, "RCLONE_S3_") && !strings.HasPrefix(key, "RCLONE_GCS_") {
			continue
		}
		out = append(out, v)
	}
	return out
}

type boundedWriter struct {
	w         io.Writer
	remaining int64
}

func (w *boundedWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.remaining {
		return 0, ErrLimit
	}
	n, err := w.w.Write(p)
	w.remaining -= int64(n)
	return n, err
}

type snapshot struct {
	root      string
	dir       string
	inventory inventory
	reviews   []Review
	binary    string
	report    Report
	memo      *classificationMemo
}

func (s *snapshot) close() error {
	if os.RemoveAll(s.root) != nil {
		return ErrCleanup
	}
	return nil
}

func take(ctx context.Context, source string, reviews []Review, binary string, execute command) (_ *snapshot, err error) {
	return takeCached(ctx, source, reviews, binary, execute, nil)
}

func takeCached(ctx context.Context, source string, reviews []Review, binary string, execute command, memo *classificationMemo) (_ *snapshot, err error) {
	root, err := os.MkdirTemp("", "hopcharlie-inspect-")
	if err != nil {
		return nil, ErrRead
	}
	dir := filepath.Join(root, "data")
	s := &snapshot{root: root, dir: dir, reviews: slices.Clone(reviews), binary: binary, memo: memo}
	defer func() {
		if err != nil {
			if e := s.close(); e != nil {
				err = e
			}
		}
	}()
	if os.Mkdir(dir, 0700) != nil {
		return nil, ErrRead
	}
	meta := filepath.Join(root, "initial")
	if os.Mkdir(meta, 0700) != nil {
		return nil, ErrRead
	}
	s.inventory, err = makeInventory(ctx, source, meta, execute)
	if err != nil {
		return nil, err
	}
	// The exact validated list is literal, never globbed. rclone's bounded
	// transfer pool reuses connections; a hard total byte limit also applies if
	// a source changes size. Missing/oversize/partial files fail inspection.
	if err = downloadInventory(ctx, source, s, execute); err != nil {
		return nil, err
	}
	// A second complete enumeration detects additions/deletions/size changes.
	// It is not an atomic source snapshot; only the inspected staging is sent.
	after := filepath.Join(root, "after")
	if os.Mkdir(after, 0700) != nil {
		return nil, ErrRead
	}
	now, err := makeInventory(ctx, source, after, execute)
	if err != nil {
		return nil, err
	}
	if !sameInventory(s.inventory, now) {
		return nil, ErrChanged
	}
	manifestName := filepath.Join(root, "inspected.tmp")
	manifest, openErr := os.OpenFile(manifestName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if openErr != nil {
		return nil, ErrRead
	}
	s.report, err = scanEachCached(ctx, dir, s.inventory.walk, reviews, binary, memo, &boundedWriter{manifest, 16 << 20})
	closeErr := manifest.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil || os.Rename(manifestName, filepath.Join(root, "inspected.json")) != nil {
		return nil, ErrRead
	}
	return s, nil
}

func downloadInventory(ctx context.Context, source string, s *snapshot, execute command) error {
	// Inventory sorting remains chunked, but a transfer need not restart its
	// process, credentials and connections at each 256-object sort boundary.
	// The literal list is streamed to disk and already capped at MaxObjects;
	// the single bounded pool retains the same hard total transfer budget.
	if ctx.Err() != nil {
		return ErrRead
	}
	paths := filepath.Join(s.root, "files.txt")
	f, err := os.OpenFile(paths, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return ErrRead
	}
	err = s.inventory.walk(func(o object) error {
		if ctx.Err() != nil {
			return ErrRead
		}
		if _, err := io.WriteString(f, o.Path+"\n"); err != nil {
			return ErrRead
		}
		return nil
	})
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return ErrRead
	}
	if s.inventory.count == 0 {
		return nil
	}
	if execute(ctx, io.Discard, "copy", source, s.dir, "--files-from-raw", paths, "--ignore-times", "--no-traverse", "--transfers", "64", "--checkers", "64", "--buffer-size", "1Mi", "--multi-thread-streams", "0", "--max-backlog", "256", "--max-transfer", fmt.Sprintf("%dB", MaxTotalBytes), "--cutoff-mode", "HARD", "--retries", "1", "--low-level-retries", "1") != nil {
		return ErrRead
	}
	return s.inventory.walk(func(o object) error {
		if ctx.Err() != nil {
			return ErrRead
		}
		info, err := os.Lstat(filepath.Join(s.dir, filepath.FromSlash(o.Path)))
		if err != nil || !info.Mode().IsRegular() || info.Size() != o.Size {
			return ErrChanged
		}
		return nil
	})
}

func readObject(dir string, o object) ([]byte, error) {
	name := filepath.Join(dir, filepath.FromSlash(o.Path))
	info, err := os.Lstat(name)
	if err != nil || !info.Mode().IsRegular() || info.Size() != o.Size {
		return nil, ErrChanged
	}
	f, err := os.Open(name)
	if err != nil {
		return nil, ErrRead
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, MaxObjectBytes+1))
	if err != nil || int64(len(b)) != o.Size || int64(len(b)) > MaxObjectBytes {
		return nil, ErrChanged
	}
	return b, nil
}

func (s *snapshot) verify(ctx context.Context) error {
	count := 0
	err := filepath.WalkDir(s.dir, func(name string, d os.DirEntry, err error) error {
		if err != nil || ctx.Err() != nil || d.Type()&os.ModeSymlink != 0 {
			return ErrChanged
		}
		if !d.IsDir() {
			count++
		}
		return nil
	})
	if err != nil || count != s.inventory.count {
		return ErrChanged
	}
	return s.verifyInspection(ctx)
}

// Inspector downloads and scans without sending anything to a destination.
// A start-time report is not reused as permission for later changed bytes.
type Inspector struct{ cache *ClassificationCache }

func NewInspector(cache *ClassificationCache) Inspector { return Inspector{cache: cache} }

func (i Inspector) Inspect(ctx context.Context, source string, reviews []Review, binary string) (Report, error) {
	return i.InspectBound(ctx, source, reviews, binary, "")
}

// InspectBound is for a server-owned Gate binding. An unbound Inspector never
// reuses classifications. No approval report or remote inventory is cached.
func (i Inspector) InspectBound(ctx context.Context, source string, reviews []Review, binary, binding string) (Report, error) {
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	s, err := takeCached(ctx, source, reviews, binary, run, newClassificationMemo(i.cache, binding, reviews, binary))
	if err != nil {
		return Report{}, err
	}
	r := s.report
	if err := s.close(); err != nil {
		return Report{}, err
	}
	return r, nil
}

// Syncer implements engine.FileSyncer. Every pass makes a new snapshot, checks
// the current metadata policy, scans all bytes and uploads only that snapshot.
// Neither the plan's executable nor flags can change this guarded path.
type Syncer struct {
	source, target string
	reviews        []Review
	binary         string
	before         func(context.Context) error
	authorize      func(context.Context, Report) error
	execute        command
	cache          *ClassificationCache
	cacheBinding   string
}

func New(source, target string, reviews []Review, binary string, before func(context.Context) error, authorize func(context.Context, Report) error) *Syncer {
	return &Syncer{source: source, target: target, reviews: slices.Clone(reviews), binary: binary, before: before, authorize: authorize, execute: run}
}

// WithClassificationCache returns a construction-time copy. Runtime calls have
// their own counters and scanner budgets; only immutable classifications share
// the Gate's bounded, synchronized cache.
func (s *Syncer) WithClassificationCache(cache *ClassificationCache, binding string) *Syncer {
	if s == nil {
		return nil
	}
	scoped := *s
	scoped.cache, scoped.cacheBinding = cache, binding
	return &scoped
}

func (s *Syncer) Available() error {
	if s == nil || s.before == nil || s.authorize == nil {
		return ErrDenied
	}
	if _, err := exec.LookPath("rclone"); err != nil {
		return ErrTransfer
	}
	return nil
}

func (s *Syncer) prepare(ctx context.Context, src, dst string) (*snapshot, error) {
	if s == nil || s.before == nil || s.authorize == nil || src != s.source || dst != s.target {
		return nil, ErrDenied
	}
	if err := s.before(ctx); err != nil {
		return nil, ErrDenied
	}
	shot, err := takeCached(ctx, src, s.reviews, s.binary, s.execute, newClassificationMemo(s.cache, s.cacheBinding, s.reviews, s.binary))
	if err != nil {
		return nil, err
	}
	if err := s.authorize(ctx, shot.report); err != nil {
		if e := shot.close(); e != nil {
			return shot, e
		}
		return shot, ErrDenied
	}
	if err := shot.verify(ctx); err != nil {
		if e := shot.close(); e != nil {
			return shot, e
		}
		return shot, err
	}
	return shot, nil
}

// Sync mirrors the inspected snapshot onto dst: objects dst has and the
// snapshot does not are deleted.
func (s *Syncer) Sync(ctx context.Context, src, dst string) (files.SyncResult, error) {
	return s.transfer(ctx, "sync", src, dst)
}

// Copy is Sync without deleting from dst (engine ADR 0018): an app without a
// database keeps what its target already holds.
func (s *Syncer) Copy(ctx context.Context, src, dst string) (files.SyncResult, error) {
	return s.transfer(ctx, "copy", src, dst)
}

// Readable lists remote's top level, for one of the guard's two buckets
// only, to show the engine can read it before anything moves.
func (s *Syncer) Readable(ctx context.Context, remote string) error {
	if s == nil || (remote != s.source && remote != s.target) {
		return ErrDenied
	}
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	if err := s.execute(ctx, io.Discard, "lsf", remote, "--max-depth", "1"); err != nil {
		return ErrTransfer
	}
	return nil
}

// transfer runs rclone op ("sync" or "copy") from a new inspected snapshot.
func (s *Syncer) transfer(ctx context.Context, op, src, dst string) (result files.SyncResult, err error) {
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	timing := newOperationTiming()
	var shot *snapshot
	defer func() {
		if e := timing.close(shot); e != nil {
			err = e
		}
		result.Duration = time.Since(timing.started)
		result.Output = timing.output(shot)
	}()
	shot, err = timing.prepare(ctx, s, src, dst)
	if err != nil {
		return result, err
	}
	// Ignore size/mtime shortcuts: every object sent comes from inspected bytes.
	if err = timing.execute(s.execute, "upload")(ctx, io.Discard, op, shot.dir, dst, "--ignore-times", "--transfers", "64", "--checkers", "64", "--buffer-size", "1Mi", "--multi-thread-streams", "0"); err != nil {
		return result, ErrTransfer
	}
	if err = timing.execute(s.execute, "targetCheck")(ctx, io.Discard, "check", shot.dir, dst, "--download", "--one-way", "--checkers", "64", "--buffer-size", "1Mi", "--multi-thread-streams", "0"); err != nil {
		return result, ErrTransfer
	}
	return result, nil
}

func (s *Syncer) Check(ctx context.Context, src, dst string) (result files.CheckResult, err error) {
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	timing := newOperationTiming()
	var shot *snapshot
	defer func() {
		if e := timing.close(shot); e != nil {
			err = e
		}
		result.Output = timing.output(shot)
	}()
	shot, err = timing.prepare(ctx, s, src, dst)
	if err != nil {
		return result, err
	}
	if err = timing.execute(s.execute, "targetCheck")(ctx, io.Discard, "check", shot.dir, dst, "--download", "--one-way", "--checkers", "64", "--buffer-size", "1Mi", "--multi-thread-streams", "0"); err != nil {
		return result, ErrTransfer
	}
	return files.CheckResult{OK: true, Matching: shot.report.Objects}, nil
}
