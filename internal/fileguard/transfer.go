package fileguard

import (
	"bytes"
	"context"
	"encoding/json"
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
	dir     string
	objects []object
	reviews []Review
	report  Report
}

func (s *snapshot) close() error {
	if os.RemoveAll(s.dir) != nil {
		return ErrCleanup
	}
	return nil
}

func take(ctx context.Context, source string, reviews []Review, execute command) (_ *snapshot, err error) {
	var listing bytes.Buffer
	if err := execute(ctx, &boundedWriter{&listing, 8 << 20}, "lsjson", source, "--recursive", "--files-only", "--no-modtime"); err != nil {
		return nil, ErrRead
	}
	var objects []object
	if json.Unmarshal(listing.Bytes(), &objects) != nil || objects == nil {
		return nil, ErrRead
	}
	if len(objects) > MaxObjects {
		return nil, ErrLimit
	}
	slices.SortFunc(objects, func(a, b object) int { return strings.Compare(a.Path, b.Path) })
	var total int64
	seen := map[string]bool{}
	for _, o := range objects {
		// Case-folded collisions are denied on every OS, not silently overwritten
		// on Windows. Directories are created by us, never copied as links.
		key := strings.ToLower(o.Path)
		if o.IsDir || !safePath(o.Path) || seen[key] {
			return nil, ErrPath
		}
		seen[key] = true
		if o.Size < 0 || o.Size > MaxObjectBytes || o.Size > MaxTotalBytes-total {
			return nil, ErrLimit
		}
		total += o.Size
	}
	dir, err := os.MkdirTemp("", "hopcharlie-inspect-")
	if err != nil {
		return nil, ErrRead
	}
	s := &snapshot{dir: dir, objects: objects, reviews: slices.Clone(reviews)}
	defer func() {
		if err != nil {
			if e := s.close(); e != nil {
				err = e
			}
		}
	}()
	// MkdirTemp is mode 0700 on Unix. On Windows the service account must own
	// a private TEMP directory/ACL; chmod alone cannot establish Windows ACLs.
	for _, o := range objects {
		name := filepath.Join(dir, filepath.FromSlash(o.Path))
		if os.MkdirAll(filepath.Dir(name), 0700) != nil {
			return nil, ErrPath
		}
		f, openErr := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if openErr != nil {
			return nil, ErrPath
		}
		w := &boundedWriter{f, o.Size}
		copyErr := execute(ctx, w, "cat", strings.TrimRight(source, "/")+"/"+o.Path)
		closeErr := f.Close()
		if copyErr != nil || closeErr != nil || w.remaining != 0 {
			return nil, ErrRead
		}
	}
	s.report, err = scan(ctx, dir, objects, reviews)
	if err != nil {
		return nil, err
	}
	return s, nil
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
	if err != nil || count != len(s.objects) {
		return ErrChanged
	}
	r, err := scan(ctx, s.dir, s.objects, s.reviews)
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
