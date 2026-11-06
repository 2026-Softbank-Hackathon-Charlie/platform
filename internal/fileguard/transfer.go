package fileguard

import (
	"context"
	"encoding/json"
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
	report    Report
}

func (s *snapshot) close() error {
	if os.RemoveAll(s.root) != nil {
		return ErrCleanup
	}
	return nil
}

func take(ctx context.Context, source string, reviews []Review, execute command) (_ *snapshot, err error) {
	root, err := os.MkdirTemp("", "hopcharlie-inspect-")
	if err != nil {
		return nil, ErrRead
	}
	dir := filepath.Join(root, "data")
	s := &snapshot{root: root, dir: dir, reviews: slices.Clone(reviews)}
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
	s.report, err = scanEach(ctx, dir, s.inventory.walk, reviews, &boundedWriter{manifest, 16 << 20})
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
	window := make([]object, 0, inventoryChunk)
	var staged int64
	flush := func() error {
		if len(window) == 0 {
			return nil
		}
		if ctx.Err() != nil {
			return ErrRead
		}
		paths := filepath.Join(s.root, "files.txt")
		f, err := os.OpenFile(paths, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
		if err != nil {
			return ErrRead
		}
		for _, o := range window {
			if _, err := io.WriteString(f, o.Path+"\n"); err != nil {
				f.Close()
				return ErrRead
			}
		}
		if f.Close() != nil {
			return ErrRead
		}
		remaining := MaxTotalBytes - staged
		if remaining == 0 {
			remaining = 1
		} // zero-length objects still need materializing
		if execute(ctx, io.Discard, "copy", source, s.dir, "--files-from-raw", paths, "--ignore-times", "--no-traverse", "--transfers", "4", "--checkers", "4", "--buffer-size", "1Mi", "--multi-thread-streams", "0", "--max-backlog", "256", "--max-transfer", fmt.Sprintf("%dB", remaining), "--cutoff-mode", "HARD", "--retries", "1", "--low-level-retries", "1") != nil {
			return ErrRead
		}
		for _, o := range window {
			info, err := os.Lstat(filepath.Join(s.dir, filepath.FromSlash(o.Path)))
			if err != nil || !info.Mode().IsRegular() || info.Size() != o.Size {
				return ErrChanged
			}
			staged += o.Size
		}
		window = window[:0]
		return nil
	}
	if err := s.inventory.walk(func(o object) error {
		window = append(window, o)
		if len(window) == inventoryChunk {
			return flush()
		}
		return nil
	}); err != nil {
		return err
	}
	return flush()
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
		if err != nil || d.Type()&os.ModeSymlink != 0 {
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
	r, err := scanEach(ctx, s.dir, s.inventory.walk, s.reviews)
	if err != nil || r.ManifestDigest != s.report.ManifestDigest {
		return ErrChanged
	}
	return nil
}

// Inspector downloads and scans without sending anything to a destination.
// A start-time report is not reused as permission for later changed bytes.
type Inspector struct{}

func (Inspector) Inspect(ctx context.Context, source string, reviews []Review) (Report, error) {
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	s, err := take(ctx, source, reviews, run)
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
	before         func(context.Context) error
	authorize      func(context.Context, Report) error
	execute        command
}

func New(source, target string, reviews []Review, before func(context.Context) error, authorize func(context.Context, Report) error) *Syncer {
	return &Syncer{source: source, target: target, reviews: slices.Clone(reviews), before: before, authorize: authorize, execute: run}
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
	shot, err := take(ctx, src, s.reviews, s.execute)
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

func (s *Syncer) Sync(ctx context.Context, src, dst string) (result files.SyncResult, err error) {
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	shot, err := s.prepare(ctx, src, dst)
	if shot != nil {
		raw, _ := json.Marshal(shot.report)
		result.Output = string(raw)
	}
	if err != nil {
		return result, err
	}
	defer func() {
		if e := shot.close(); e != nil {
			err = e
		}
	}()
	// Ignore size/mtime shortcuts: every object sent comes from inspected bytes.
	if err = s.execute(ctx, io.Discard, "sync", shot.dir, dst, "--ignore-times"); err != nil {
		return result, ErrTransfer
	}
	if err = s.execute(ctx, io.Discard, "check", shot.dir, dst, "--download", "--one-way"); err != nil {
		return result, ErrTransfer
	}
	raw, _ := json.Marshal(shot.report)
	return files.SyncResult{Duration: time.Since(start), Output: string(raw)}, nil
}

func (s *Syncer) Check(ctx context.Context, src, dst string) (result files.CheckResult, err error) {
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	shot, err := s.prepare(ctx, src, dst)
	if shot != nil {
		raw, _ := json.Marshal(shot.report)
		result.Output = string(raw)
	}
	if err != nil {
		return result, err
	}
	defer func() {
		if e := shot.close(); e != nil {
			err = e
		}
	}()
	if err = s.execute(ctx, io.Discard, "check", shot.dir, dst, "--download", "--one-way"); err != nil {
		return result, ErrTransfer
	}
	return files.CheckResult{OK: true, Matching: shot.report.Objects, Output: result.Output}, nil
}
