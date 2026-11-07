package fileguard

import (
	"archive/zip"
	"bytes"
	"context"
	"testing"
)

// Synthetic CPU comparison only, not a claim about remote transfer latency.
// Both paths read all staged bytes. The old path additionally expands and
// scans the already inspected archive a second time.
func BenchmarkSnapshotIntegrity(b *testing.B) {
	var archive bytes.Buffer
	w := zip.NewWriter(&archive)
	f, err := w.Create("fixture.txt")
	if err != nil {
		b.Fatal(err)
	}
	if _, err := f.Write(bytes.Repeat([]byte("ordinary fixture text\n"), 50000)); err != nil {
		b.Fatal(err)
	}
	if err := w.Close(); err != nil {
		b.Fatal(err)
	}
	r := &testRemote{objects: []object{{Path: "fixture.zip", Size: int64(archive.Len())}}, data: map[string][]byte{"fixture.zip": archive.Bytes()}}
	s, err := take(context.Background(), "src:bucket", nil, "", r.command)
	if err != nil {
		b.Fatal(err)
	}
	defer s.close()
	b.Run("full-dlp-rescan", func(b *testing.B) {
		for b.Loop() {
			if _, err := scanEach(context.Background(), s.dir, s.inventory.walk, s.reviews, s.binary); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("sha256-integrity", func(b *testing.B) {
		for b.Loop() {
			if err := s.verify(context.Background()); err != nil {
				b.Fatal(err)
			}
		}
	})
}
