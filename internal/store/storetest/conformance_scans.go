package storetest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
)

// scanCases are the store.ScanStore part of Conformance.
var scanCases = []struct {
	name string
	run  func(t *testing.T, s store.Store)
}{
	{"ImageScanLatestPerDigest", testImageScanLatestPerDigest},
	{"ImageScanIDBelongsToOneDigest", testImageScanIDBelongsToOneDigest},
}

func imageScan(digest, id string, n int) store.ImageScan {
	return store.ImageScan{Digest: digest, ID: id, Status: "succeeded", FinishedAt: at(n), CreatedAt: at(n + 1),
		Doc: []byte(`{"id": "` + id + `"}`), Vulnerabilities: []byte(`[]`), Report: []byte{0x1f, 0x8b}}
}

func testImageScanLatestPerDigest(t *testing.T, s store.Store) {
	ctx := context.Background()
	for _, sc := range []store.ImageScan{imageScan("sha256:b", "scan_b1", 1), imageScan("sha256:a", "scan_a1", 2), imageScan("sha256:a", "scan_a2", 5)} {
		if err := s.PutImageScan(ctx, sc); err != nil {
			t.Fatal(err)
		}
	}
	// A retry that finished before the stored scan does not replace it.
	if err := s.PutImageScan(ctx, imageScan("sha256:a", "scan_a0", 3)); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("older scan: %v, want ErrConflict", err)
	}
	got, err := s.ListImageScans(ctx, at(0))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Digest != "sha256:a" || got[0].ID != "scan_a2" || got[1].ID != "scan_b1" {
		t.Fatalf("got %+v", got)
	}
	a := got[0]
	if !a.FinishedAt.Equal(at(5).Truncate(time.Microsecond)) || a.FinishedAt.Location().String() != "UTC" || a.Report != nil || string(a.Vulnerabilities) != "[]" || a.Status != "succeeded" {
		t.Fatalf("row %+v", a)
	}
	// since leaves out scans that finished earlier.
	if got, _ := s.ListImageScans(ctx, at(2)); len(got) != 1 || got[0].ID != "scan_a2" {
		t.Fatalf("since: %+v", got)
	}
}

func testImageScanIDBelongsToOneDigest(t *testing.T, s store.Store) {
	ctx := context.Background()
	if err := s.PutImageScan(ctx, imageScan("sha256:a", "scan_1", 1)); err != nil {
		t.Fatal(err)
	}
	if err := s.PutImageScan(ctx, imageScan("sha256:b", "scan_1", 2)); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("reused id: %v, want ErrConflict", err)
	}
}
