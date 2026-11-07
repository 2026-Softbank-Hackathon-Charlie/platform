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
	"strings"
	"testing"
)

// Synthetic command transport only; snapshot creation and content scanning run
// for real. No test transport is linked into production wiring.
type testRemote struct {
	objects  []object
	data     map[string][]byte
	uploads  int
	checks   int
	stage    string
	outbound []byte
	fail     string
}

func (r *testRemote) command(_ context.Context, out io.Writer, args ...string) error {
	if args[0] == r.fail {
		return errors.New("TOPSECRET remote error with an object key")
	}
	switch args[0] {
	case "lsjson":
		objects := r.objects
		if objects == nil {
			objects = []object{}
		}
		return json.NewEncoder(out).Encode(objects)
	case "cat":
		b, ok := r.data[strings.TrimPrefix(args[1], "src:bucket/")]
		if !ok {
			return errors.New("TOPSECRET missing")
		}
		_, err := out.Write(b)
		return err
	case "copy":
		if !slices.Contains(args, "--files-from-raw") || !slices.Contains(args, "--max-transfer") || !slices.Contains(args, "--cutoff-mode") {
			return errors.New("unbounded download")
		}
		for _, o := range r.objects {
			name := filepath.Join(args[2], filepath.FromSlash(o.Path))
			if err := os.MkdirAll(filepath.Dir(name), 0700); err != nil {
				return err
			}
			if err := os.WriteFile(name, r.data[o.Path], 0600); err != nil {
				return err
			}
		}
		return nil
	case "sync":
		r.uploads++
		r.stage = args[1]
		if args[1] == "src:bucket" || !slices.Contains(args, "--ignore-times") {
			return errors.New("not a staged transfer")
		}
		var err error
		r.outbound, err = os.ReadFile(filepath.Join(args[1], "body.txt"))
		return err
	case "check":
		r.checks++
		if !slices.Contains(args, "--download") || !slices.Contains(args, "--one-way") {
			return errors.New("weak verification")
		}
		return nil
	}
	return errors.New("unexpected command")
}

func cleanRemote() *testRemote {
	return &testRemote{objects: []object{{Path: "body.txt", Size: 5}}, data: map[string][]byte{"body.txt": []byte("hello")}}
}

func makeSyncer(r *testRemote, authorize func(context.Context, Report) error) *Syncer {
	s := New("src:bucket", "dst:bucket", nil, "", func(context.Context) error { return nil }, authorize)
	s.execute = r.command
	return s
}

func allow(context.Context, Report) error { return nil }

func TestOnlyInspectedSnapshotIsUploaded(t *testing.T) {
	r := cleanRemote()
	s := makeSyncer(r, func(_ context.Context, report Report) error {
		if report.Objects != 1 || report.ManifestDigest == "" {
			t.Fatal("missing real inspection")
		}
		// Same-size remote mutation after inspection must NOT reach destination.
		r.data["body.txt"] = []byte("other")
		return nil
	})
	res, err := s.Sync(context.Background(), "src:bucket", "dst:bucket")
	if err != nil || string(r.outbound) != "hello" || r.uploads != 1 || r.checks != 1 {
		t.Fatalf("%+v %v outbound=%q", res, err, r.outbound)
	}
	if _, err := os.Stat(r.stage); !os.IsNotExist(err) {
		t.Fatalf("staging not removed: %v", err)
	}
	if strings.Contains(res.Output, "body.txt") || strings.Contains(res.Output, "hello") {
		t.Fatal("unsafe output")
	}
}

func TestEveryPassRescansAndDenialPreventsUpload(t *testing.T) {
	r := cleanRemote()
	s := makeSyncer(r, func(_ context.Context, report Report) error {
		if slices.Contains(report.Labels, "kr-only") {
			return ErrDenied
		}
		return nil
	})
	if _, err := s.Sync(context.Background(), "src:bucket", "dst:bucket"); err != nil {
		t.Fatal(err)
	}
	r.data["body.txt"] = []byte("demo@example.invalid")
	r.objects[0].Size = int64(len(r.data["body.txt"]))
	if _, err := s.Sync(context.Background(), "src:bucket", "dst:bucket"); !errors.Is(err, ErrDenied) || r.uploads != 1 {
		t.Fatalf("changed source bypassed guard: %v uploads=%d", err, r.uploads)
	}
	if _, err := s.Check(context.Background(), "src:bucket", "dst:bucket"); !errors.Is(err, ErrDenied) {
		t.Fatalf("check bypassed guard: %v", err)
	}
}

func TestSnapshotMutationPreventsUpload(t *testing.T) {
	r := cleanRemote()
	var stage string
	execute := r.command
	s := makeSyncer(r, func(context.Context, Report) error {
		if err := os.WriteFile(stage, []byte("other"), 0600); err != nil {
			t.Fatal(err)
		}
		return nil
	})
	s.execute = func(ctx context.Context, w io.Writer, args ...string) error {
		if args[0] == "copy" {
			stage = filepath.Join(args[2], "body.txt")
		}
		return execute(ctx, w, args...)
	}
	if _, err := s.Sync(context.Background(), "src:bucket", "dst:bucket"); !errors.Is(err, ErrChanged) || r.uploads != 0 {
		t.Fatalf("mutated snapshot allowed: %v", err)
	}
}

func TestTransferFailuresAndLimitsAreClosedAndRedacted(t *testing.T) {
	for _, name := range []string{"lsjson", "copy", "sync", "check"} {
		t.Run(name, func(t *testing.T) {
			r := cleanRemote()
			r.fail = name
			_, err := makeSyncer(r, allow).Sync(context.Background(), "src:bucket", "dst:bucket")
			if err == nil || strings.Contains(err.Error(), "TOPSECRET") {
				t.Fatalf("unsafe failure: %v", err)
			}
			if (name == "lsjson" || name == "copy") && r.uploads != 0 {
				t.Fatal("uploaded after incomplete read")
			}
		})
	}
	for name, objects := range map[string][]object{
		"traversal": {{Path: "../outside", Size: 1}},
		"duplicate": {{Path: "a", Size: 1}, {Path: "A", Size: 1}},
		"negative":  {{Path: "a", Size: -1}},
		"size":      {{Path: "a", Size: MaxObjectBytes + 1}},
		"count":     make([]object, MaxObjects+1),
	} {
		t.Run(name, func(t *testing.T) {
			r := cleanRemote()
			r.objects = objects
			if _, err := makeSyncer(r, allow).Sync(context.Background(), "src:bucket", "dst:bucket"); err == nil || r.uploads != 0 {
				t.Fatalf("bad listing allowed: %v", err)
			}
		})
	}
	r := cleanRemote()
	r.objects[0].Size = 4
	if _, err := makeSyncer(r, allow).Sync(context.Background(), "src:bucket", "dst:bucket"); err == nil || r.uploads != 0 {
		t.Fatalf("size change allowed: %v", err)
	}
	r = cleanRemote()
	if _, err := makeSyncer(r, allow).Sync(context.Background(), "src:other", "dst:bucket"); !errors.Is(err, ErrDenied) {
		t.Fatalf("route changed: %v", err)
	}
}

func TestGlobalRcloneFlagsCannotChangeInspectedSet(t *testing.T) {
	env := []string{"PATH=bin", "RCLONE_FILTER=- *", "RCLONE_DRY_RUN=true", "RCLONE_LINKS=true", "RCLONE_CONFIG=other.conf", "RCLONE_CONFIG_SRC_TYPE=s3", "RCLONE_S3_REGION=region", "RCLONE_GCS_LOCATION=region"}
	got := remoteEnvironment(env)
	want := []string{env[0], env[5], env[6], env[7]}
	if !slices.Equal(got, want) {
		t.Fatalf("unsafe environment %v", got)
	}
}

// Real local integration: actual rclone processes and filesystem, no cloud.
// Absence of the external binary is an explicit skip, never a simulated pass.
func TestLocalRcloneIntegration(t *testing.T) {
	if _, err := exec.LookPath("rclone"); err != nil {
		t.Skip("rclone is not installed")
	}
	src, dst := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "data.txt"), []byte("hello"), 0600); err != nil {
		t.Fatal(err)
	}
	s := New(src, dst, nil, "", func(context.Context) error { return nil }, func(_ context.Context, r Report) error {
		if slices.Contains(r.Labels, "kr-only") {
			return ErrDenied
		}
		return nil
	})
	if _, err := s.Sync(context.Background(), src, dst); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dst, "data.txt"))
	if err != nil || string(b) != "hello" {
		t.Fatalf("local transfer: %q %v", b, err)
	}
	if _, err := s.Check(context.Background(), src, dst); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "data.txt"), []byte("demo@example.invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Sync(context.Background(), src, dst); !errors.Is(err, ErrDenied) {
		t.Fatalf("PII allowed: %v", err)
	}
	b, err = os.ReadFile(filepath.Join(dst, "data.txt"))
	if err != nil || string(b) != "hello" {
		t.Fatal("denied bytes reached destination")
	}
}
