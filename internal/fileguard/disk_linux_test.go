package fileguard

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Run only with TMPDIR on a dedicated 1 MiB tmpfs. Fixtures stay on /tmp,
// so this injects a real staging ENOSPC without filling the host filesystem.
func TestRealStagingDiskFull(t *testing.T) {
	if os.Getenv("FILEGUARD_ENOSPC") != "1" {
		t.Skip("requires isolated small tmpfs")
	}
	source, err := os.MkdirTemp("/tmp", "hop87-source-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(source)
	destination, err := os.MkdirTemp("/tmp", "hop87-target-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(destination)
	if err := os.WriteFile(filepath.Join(source, "data.txt"), bytes.Repeat([]byte("x"), 2<<20), 0600); err != nil {
		t.Fatal(err)
	}
	authorized := false
	sawCapacityError := false
	syncer := New(source, destination, nil, func(context.Context) error { return nil }, func(context.Context, Report) error { authorized = true; return nil })
	syncer.execute = func(ctx context.Context, out io.Writer, args ...string) error {
		if args[0] != "copy" {
			return run(ctx, out, args...)
		}
		args = append(args, "--config", os.DevNull, "--log-level", "ERROR", "--log-file", "")
		cmd := exec.CommandContext(ctx, "rclone", args...)
		cmd.Env = remoteEnvironment(os.Environ())
		cmd.Stdout = out
		var diagnostic bytes.Buffer
		cmd.Stderr = &diagnostic
		err := cmd.Run()
		message := strings.ToLower(diagnostic.String())
		sawCapacityError = strings.Contains(message, "no space left on device") || strings.Contains(message, "file too big for remaining disk space")
		if err != nil {
			return ErrTransfer
		}
		return nil
	}
	if _, err := syncer.Sync(context.Background(), source, destination); err == nil || authorized {
		t.Fatal("staging ENOSPC did not stop before authorization")
	}
	if !sawCapacityError {
		t.Fatal("failure was not an actual staging capacity rejection")
	}
	files, err := os.ReadDir(destination)
	if err != nil || len(files) != 0 {
		t.Fatal("data uploaded after staging ENOSPC")
	}
	leftovers, err := filepath.Glob(filepath.Join(os.TempDir(), "hopcharlie-inspect-*"))
	if err != nil || len(leftovers) != 0 {
		t.Fatal("failed snapshot was not cleaned")
	}
}
