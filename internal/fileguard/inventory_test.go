package fileguard

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDiskInventorySortAndCrossChunkCollision(t *testing.T) {
	objects := make([]object, 600)
	for i := range objects {
		objects[i] = object{Path: fmt.Sprintf("object-%04d", 599-i), Size: 1}
	}
	execute := func(_ context.Context, w io.Writer, _ ...string) error { return json.NewEncoder(w).Encode(objects) }
	result, err := makeInventory(context.Background(), "src", t.TempDir(), execute)
	if err != nil {
		t.Fatal(err)
	}
	previous := ""
	count := 0
	if err := result.walk(func(o object) error {
		if o.Path <= previous {
			t.Fatal("inventory not sorted")
		}
		previous = o.Path
		count++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if count != 600 || result.bytes != 600 {
		t.Fatal("partial inventory accepted")
	}
	objects[0].Path = "duplicate"
	objects[599].Path = "DUPLICATE"
	if _, err := makeInventory(context.Background(), "src", t.TempDir(), execute); err != ErrPath {
		t.Fatalf("cross-chunk collision accepted: %v", err)
	}
	objects[0].Path = "Parent/first"
	objects[599].Path = "parent/second"
	if _, err := makeInventory(context.Background(), "src", t.TempDir(), execute); err != ErrPath {
		t.Fatalf("directory case alias accepted: %v", err)
	}
}

func TestPartialOrFailedEnumerationIsNotComplete(t *testing.T) {
	for _, body := range []string{`null`, `[{"Path":"file","Size":1}`, `[] {}`, `[{"Path":"../escape","Size":1}]`} {
		execute := func(_ context.Context, w io.Writer, _ ...string) error { _, err := io.WriteString(w, body); return err }
		if _, err := makeInventory(context.Background(), "src", t.TempDir(), execute); err == nil {
			t.Fatal("invalid listing accepted")
		}
	}
	execute := func(_ context.Context, w io.Writer, _ ...string) error {
		_, _ = io.WriteString(w, "[]")
		return errors.New("SECRET failed after complete JSON")
	}
	if _, err := makeInventory(context.Background(), "src", t.TempDir(), execute); err != ErrRead {
		t.Fatalf("late process failure accepted: %v", err)
	}
}

func TestEnumerationChangesAndDownloadFailurePreventUpload(t *testing.T) {
	for _, fault := range []string{"added", "removed", "size", "disk-full", "cancelled"} {
		t.Run(fault, func(t *testing.T) {
			remote := cleanRemote()
			syncer := makeSyncer(remote, allow)
			calls := 0
			syncer.execute = func(ctx context.Context, w io.Writer, args ...string) error {
				if args[0] == "copy" && (fault == "disk-full" || fault == "cancelled") {
					if err := os.WriteFile(filepath.Join(args[2], "partial"), []byte("partial"), 0600); err != nil {
						return err
					}
					return errors.New("SECRET ENOSPC or cancelled source key")
				}
				if args[0] == "lsjson" {
					calls++
					if calls == 2 {
						switch fault {
						case "added":
							remote.objects = append(remote.objects, object{Path: "late", Size: 1})
						case "removed":
							remote.objects = []object{}
						case "size":
							remote.objects[0].Size++
						}
					}
				}
				return remote.command(ctx, w, args...)
			}
			_, err := syncer.Sync(context.Background(), "src:bucket", "dst:bucket")
			if err == nil || strings.Contains(err.Error(), "SECRET") || remote.uploads != 0 {
				t.Fatalf("unsafe failure %v uploads=%d", err, remote.uploads)
			}
		})
	}
}

func TestStreamingManifestKeepsLegacyDigest(t *testing.T) {
	remote := cleanRemote()
	shot, err := take(context.Background(), "src:bucket", nil, "", remote.command)
	if err != nil {
		t.Fatal(err)
	}
	defer shot.close()
	legacy := `{"Rules":"content-v1","Entries":[{"Path":"body.txt","SHA256":"2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824","Labels":["global"]}]}`
	digest := sha256.Sum256([]byte(legacy))
	if shot.report.ManifestDigest != hex.EncodeToString(digest[:]) {
		t.Fatal("manifest digest contract changed")
	}
}

func TestInventoryMutationCannotChangeUploadedSet(t *testing.T) {
	remote := cleanRemote()
	shot, err := take(context.Background(), "src:bucket", nil, "", remote.command)
	if err != nil {
		t.Fatal(err)
	}
	defer shot.close()
	if err := os.WriteFile(shot.inventory.path, []byte("{\"Path\":\"../outside\",\"Size\":5}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := shot.verify(context.Background()); err != ErrChanged {
		t.Fatalf("modified inventory trusted: %v", err)
	}
}

func TestStreamingObjectCapAndCancelledCleanup(t *testing.T) {
	execute := func(_ context.Context, w io.Writer, _ ...string) error {
		if _, err := io.WriteString(w, "["); err != nil {
			return err
		}
		for i := 0; i <= MaxObjects; i++ {
			if i > 0 {
				if _, err := io.WriteString(w, ","); err != nil {
					return err
				}
			}
			if err := json.NewEncoder(w).Encode(object{Path: fmt.Sprintf("key-%05d", i), Size: 1}); err != nil {
				return err
			}
		}
		_, err := io.WriteString(w, "]")
		return err
	}
	if _, err := makeInventory(context.Background(), "src", t.TempDir(), execute); err != ErrLimit {
		t.Fatalf("object count cap was not enforced: %v", err)
	}
	root := t.TempDir()
	t.Setenv("TMPDIR", root)
	t.Setenv("TMP", root)
	t.Setenv("TEMP", root)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	remote := cleanRemote()
	if _, err := take(ctx, "src:bucket", nil, "", remote.command); err == nil {
		t.Fatal("cancelled inspection accepted")
	}
	files, err := os.ReadDir(root)
	if err != nil || len(files) != 0 {
		t.Fatal("cancelled snapshot not cleaned")
	}
}
