package fileguard

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestTransferPoolsKeepFullInspectionAndTargetProof(t *testing.T) {
	for _, operation := range []string{"sync", "copy", "check"} {
		t.Run(operation, func(t *testing.T) {
			r := cleanRemote()
			s := makeSyncer(r, allow)
			var commands [][]string
			s.execute = func(ctx context.Context, out io.Writer, args ...string) error {
				commands = append(commands, slices.Clone(args))
				if args[0] == "copy" && args[2] == "dst:bucket" {
					return nil
				}
				return r.command(ctx, out, args...)
			}
			var output string
			var err error
			switch operation {
			case "sync":
				res, e := s.Sync(context.Background(), "src:bucket", "dst:bucket")
				output, err = res.Output, e
			case "copy":
				res, e := s.Copy(context.Background(), "src:bucket", "dst:bucket")
				output, err = res.Output, e
			case "check":
				res, e := s.Check(context.Background(), "src:bucket", "dst:bucket")
				output, err = res.Output, e
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, args := range commands {
				if args[0] == "lsjson" {
					continue
				}
				pool := "16"
				if slices.Contains(args, "--files-from-raw") {
					pool = "64"
				}
				for flag, want := range map[string]string{"--checkers": pool, "--buffer-size": "1Mi", "--multi-thread-streams": "0"} {
					i := slices.Index(args, flag)
					if i < 0 || i+1 >= len(args) || args[i+1] != want {
						t.Fatalf("%s: missing bounded %s=%s", args[0], flag, want)
					}
				}
				if args[0] != "check" {
					i := slices.Index(args, "--transfers")
					if i < 0 || i+1 >= len(args) || args[i+1] != pool || !slices.Contains(args, "--ignore-times") {
						t.Fatal("transfer must send the full inspected snapshot with a bounded pool")
					}
				}
			}
			var evidence struct {
				Report
				TimingMillis map[string]int64 `json:"timingMillis"`
			}
			if err := json.Unmarshal([]byte(output), &evidence); err != nil || evidence.Objects != 1 || evidence.ManifestDigest == "" {
				t.Fatalf("existing report lost: %v", err)
			}
			for _, phase := range []string{"prepare", "sourceInventory", "sourceDownload", "prepareOther", "upload", "targetCheck", "cleanup", "total"} {
				if duration, ok := evidence.TimingMillis[phase]; !ok || duration < 0 {
					t.Fatalf("missing/nonnegative phase %s: %v", phase, evidence.TimingMillis)
				}
			}
			if r.checks != 1 {
				t.Fatal("target byte comparison was skipped")
			}
		})
	}
}

func TestFailedTargetProofRetainsTimingAndCleansSnapshot(t *testing.T) {
	r := cleanRemote()
	r.fail = "check"
	res, err := makeSyncer(r, allow).Sync(context.Background(), "src:bucket", "dst:bucket")
	if !errors.Is(err, ErrTransfer) {
		t.Fatalf("verification failure allowed: %v", err)
	}
	var evidence struct {
		TimingMillis map[string]int64 `json:"timingMillis"`
	}
	if json.Unmarshal([]byte(res.Output), &evidence) != nil || len(evidence.TimingMillis) == 0 || res.Duration <= 0 {
		t.Fatal("failed transfer lost its elapsed evidence")
	}
	if _, err := os.Stat(r.stage); !os.IsNotExist(err) {
		t.Fatal("failed transfer retained its private staging")
	}
}

func TestEarlyListingFailureWaitsForCommandCancellation(t *testing.T) {
	s := makeSyncer(cleanRemote(), allow)
	cancelled, release := make(chan struct{}), make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	s.execute = func(ctx context.Context, out io.Writer, args ...string) error {
		if args[0] != "lsjson" {
			return errors.New("unexpected transfer after invalid listing")
		}
		_, _ = io.WriteString(out, `[{"Path":"../unsafe","Size":0}]`)
		<-ctx.Done()
		close(cancelled)
		<-release
		return ctx.Err()
	}
	done := make(chan error, 1)
	go func() {
		_, err := s.Sync(context.Background(), "src:bucket", "dst:bucket")
		done <- err
	}()
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("invalid listing did not cancel its producer")
	}
	select {
	case <-done:
		t.Fatal("timing and cleanup completed while command was still running")
	case <-time.After(10 * time.Millisecond):
	}
	close(release)
	released = true
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("invalid listing allowed")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled command finished but operation did not return")
	}
}

// Real rclone processes prove the configured pools support literal nested keys,
// empty objects and same-size target corruption without any external service.
func TestLocalRcloneFullSnapshotRepair(t *testing.T) {
	if _, err := exec.LookPath("rclone"); err != nil {
		t.Skip("rclone is not installed")
	}
	src, dst := t.TempDir(), t.TempDir()
	for _, root := range []string{src, dst} {
		if err := os.MkdirAll(filepath.Join(root, "nested"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	for name, body := range map[string]string{"nested/[literal].txt": "hello", "empty.txt": ""} {
		if err := os.WriteFile(filepath.Join(src, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	s := New(src, dst, nil, "", func(context.Context) error { return nil }, allow)
	if _, err := s.Sync(context.Background(), src, dst); err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(dst, "nested/[literal].txt")
	info, err := os.Stat(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte("other"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(name, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Check(context.Background(), src, dst); !errors.Is(err, ErrTransfer) {
		t.Fatalf("same-size corruption passed download proof: %v", err)
	}
	if _, err := s.Sync(context.Background(), src, dst); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(name); err != nil || string(data) != "hello" {
		t.Fatal("full staged upload did not repair equal-size target corruption")
	}
	if _, err := s.Check(context.Background(), src, dst); err != nil {
		t.Fatal(err)
	}
}
