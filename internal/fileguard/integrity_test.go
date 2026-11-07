package fileguard

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestLocalRcloneAcrossInventoryChunks(t *testing.T) {
	if _, err := exec.LookPath("rclone"); err != nil {
		t.Skip("rclone is not installed")
	}
	src, dst := t.TempDir(), t.TempDir()
	for i := range inventoryChunk + 1 {
		name := fmt.Sprintf("literal-[%04d].txt", i)
		if err := os.WriteFile(filepath.Join(src, name), []byte("fixture"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	s := New(src, dst, nil, "", func(context.Context) error { return nil }, allow)
	downloads := 0
	s.execute = func(ctx context.Context, out io.Writer, args ...string) error {
		if args[0] == "copy" && args[1] == src {
			downloads++
		}
		return run(ctx, out, args...)
	}
	if _, err := s.Sync(context.Background(), src, dst); err != nil {
		t.Fatal(err)
	}
	objects, err := os.ReadDir(dst)
	if err != nil || len(objects) != inventoryChunk+1 || downloads != 1 {
		t.Fatalf("partial transfer: objects=%d downloads=%d err=%v", len(objects), downloads, err)
	}
}

// Scale the existing HARD budget down to five bytes to exercise the exact
// boundary plus a later empty object without allocating a 256 MiB fixture.
func TestLocalRcloneHardLimitBoundaryFailsClosed(t *testing.T) {
	if _, err := exec.LookPath("rclone"); err != nil {
		t.Skip("rclone is not installed")
	}
	src, dst := t.TempDir(), t.TempDir()
	for name, data := range map[string]string{"a-data.txt": "hello", "z-empty.txt": ""} {
		if err := os.WriteFile(filepath.Join(src, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	authorized := false
	s := New(src, dst, nil, "", func(context.Context) error { return nil }, func(context.Context, Report) error { authorized = true; return nil })
	s.execute = func(ctx context.Context, out io.Writer, args ...string) error {
		if args[0] == "copy" && args[1] == src {
			args = slices.Clone(args)
			i := slices.Index(args, "--max-transfer")
			if i < 0 {
				t.Fatal("download has no hard limit")
			}
			args[i+1] = "5B"
		}
		return run(ctx, out, args...)
	}
	_, err := s.Sync(context.Background(), src, dst)
	if err != nil {
		if !errors.Is(err, ErrRead) || authorized {
			t.Fatalf("partial read authorized: %v", err)
		}
		entries, e := os.ReadDir(dst)
		if e != nil || len(entries) != 0 {
			t.Fatal("partial snapshot reached destination")
		}
		t.Log("HARD boundary refused before authorization; limit is unchanged")
		return
	}
	for name, want := range map[string]string{"a-data.txt": "hello", "z-empty.txt": ""} {
		data, e := os.ReadFile(filepath.Join(dst, name))
		if e != nil || string(data) != want {
			t.Fatal("HARD boundary accepted incomplete snapshot")
		}
	}
	t.Log("HARD boundary completed every byte and empty object")
}

func TestLargeInventoryReusesOneBoundedDownloadSession(t *testing.T) {
	r := &testRemote{data: map[string][]byte{}}
	for i := range 2*inventoryChunk + 1 {
		name := fmt.Sprintf("item-%04d.txt", i)
		r.objects = append(r.objects, object{Path: name, Size: 0})
		r.data[name] = []byte{}
	}
	downloads := 0
	execute := func(ctx context.Context, out io.Writer, args ...string) error {
		if args[0] == "copy" {
			downloads++
			if downloads > 1 {
				return errors.New("unnecessary new source session")
			}
			i := slices.Index(args, "--files-from-raw")
			if i < 0 {
				t.Fatal("missing literal object list")
			}
			raw, err := os.ReadFile(args[i+1])
			if err != nil || len(strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")) != len(r.objects) {
				t.Fatal("download did not include the complete bounded inventory")
			}
		}
		return r.command(ctx, out, args...)
	}
	s, err := take(context.Background(), "src:bucket", nil, "", execute)
	if err != nil {
		t.Fatalf("large inventory did not reuse its download session: %v", err)
	}
	defer s.close()
	if downloads != 1 || s.report.Objects != len(r.objects) {
		t.Fatal("partial inventory inspected")
	}
}

func TestSnapshotIntegrityKeepsInitialArchiveInspection(t *testing.T) {
	data := archive(t, "nested.txt", []byte("contact demo@example.invalid"), nil)
	r := &testRemote{objects: []object{{Path: "archive.zip", Size: int64(len(data))}}, data: map[string][]byte{"archive.zip": data}}
	s, err := take(context.Background(), "src:bucket", nil, "", r.command)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	if s.report.Findings["email"] != 1 {
		t.Fatal("initial archive content was not inspected")
	}
	if err := s.verify(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.dir, "archive.zip"), bytes.Repeat([]byte{'x'}, len(data)), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.verify(context.Background()); !errors.Is(err, ErrChanged) {
		t.Fatalf("changed archive passed: %v", err)
	}
}

func TestSnapshotIntegrityBindsOriginalInspectionManifest(t *testing.T) {
	r := cleanRemote()
	s, err := take(context.Background(), "src:bucket", nil, "", r.command)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	manifest := filepath.Join(s.root, "inspected.json")
	raw, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifest, []byte(strings.Replace(string(raw), "global", "kr-only", 1)), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.verify(context.Background()); !errors.Is(err, ErrChanged) {
		t.Fatalf("inspection manifest changed after authorization: %v", err)
	}
}

func TestSnapshotIntegrityRejectsEveryUninspectedChange(t *testing.T) {
	for _, change := range []string{"same-size bytes", "missing", "extra", "manifest-trailing", "manifest-truncated"} {
		t.Run(change, func(t *testing.T) {
			r := cleanRemote()
			s, err := take(context.Background(), "src:bucket", nil, "", r.command)
			if err != nil {
				t.Fatal(err)
			}
			defer s.close()
			name := filepath.Join(s.dir, "body.txt")
			manifest := filepath.Join(s.root, "inspected.json")
			switch change {
			case "same-size bytes":
				err = os.WriteFile(name, []byte("other"), 0600)
			case "missing":
				err = os.Remove(name)
			case "extra":
				err = os.WriteFile(filepath.Join(s.dir, "extra.txt"), []byte("hello"), 0600)
			case "manifest-trailing":
				var f *os.File
				f, err = os.OpenFile(manifest, os.O_APPEND|os.O_WRONLY, 0600)
				if err == nil {
					_, err = f.WriteString("{}")
					f.Close()
				}
			case "manifest-truncated":
				err = os.WriteFile(manifest, []byte("{}"), 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := s.verify(context.Background()); !errors.Is(err, ErrChanged) {
				t.Fatalf("%v", err)
			}
		})
	}
}
