package fileguard

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Both cases read every byte and rebuild the manifest. Only immutable DLP/ZIP
// content classification is reused; no network, target check or policy gate is
// removed. This synthetic CPU benchmark is not a production latency claim.
func BenchmarkRepeatedMigrationClassification(b *testing.B) {
	var archive bytes.Buffer
	w := zip.NewWriter(&archive)
	f, err := w.Create("records.txt")
	if err != nil {
		b.Fatal(err)
	}
	if _, err := io.Copy(f, bytes.NewReader(bytes.Repeat([]byte("ordinary fixture text\n"), 50000))); err != nil {
		b.Fatal(err)
	}
	if err := w.Close(); err != nil {
		b.Fatal(err)
	}
	dir := b.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "records.zip"), archive.Bytes(), 0600); err != nil {
		b.Fatal(err)
	}
	walk := func(visit func(object) error) error {
		return visit(object{Path: "records.zip", Size: int64(archive.Len())})
	}
	cache := NewClassificationCache(16)
	scope := strings.Repeat("a", 64)
	if _, err := scanEachCached(context.Background(), dir, walk, nil, "", newClassificationMemo(cache, scope, nil, "")); err != nil {
		b.Fatal(err)
	}
	b.Run("fresh-DLP", func(b *testing.B) {
		for b.Loop() {
			if _, err := scanEach(context.Background(), dir, walk, nil, ""); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("exact-SHA256-reuse", func(b *testing.B) {
		for b.Loop() {
			if _, err := scanEachCached(context.Background(), dir, walk, nil, "", newClassificationMemo(cache, scope, nil, "")); err != nil {
				b.Fatal(err)
			}
		}
	})
}
