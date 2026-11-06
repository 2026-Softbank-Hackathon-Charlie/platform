package fileguard

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func archive(t *testing.T, name string, data []byte, extra []byte) []byte {
	t.Helper()
	var b bytes.Buffer
	w := zip.NewWriter(&b)
	f, err := w.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate, Extra: extra})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func testScan(t *testing.T, name string, data []byte, reviews []Review) (Report, error) {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
		t.Fatal(err)
	}
	return scan(context.Background(), dir, []object{{Path: name, Size: int64(len(data))}}, reviews)
}

func TestTextAndZIPFindings(t *testing.T) {
	body := []byte("contact demo@example.invalid; resident 000000-1000000; phone 010-0000-0000")
	for name, data := range map[string][]byte{
		"text":   body,
		"zip":    archive(t, "records.txt", body, nil),
		"nested": archive(t, "nested.zip", archive(t, "records.txt", body, nil), nil),
	} {
		t.Run(name, func(t *testing.T) {
			r, err := testScan(t, "misleading.jpg", data, nil)
			if err != nil || !slices.Contains(r.Labels, "kr-only") || r.Findings["email"] != 1 || r.Findings["kr_resident_number"] != 1 || r.Objects != 1 {
				t.Fatalf("%+v %v", r, err)
			}
			raw, _ := json.Marshal(r)
			if bytes.Contains(raw, body) || strings.Contains(string(raw), "misleading.jpg") || strings.Contains(string(raw), "demo@example.invalid") {
				t.Fatalf("unsafe evidence %s", raw)
			}
		})
	}
}

func TestUnsupportedBytesRequireExactReview(t *testing.T) {
	body := []byte{0xff, 0, 1, 2}
	h := sha256.Sum256(body)
	rv := Review{SHA256: hex.EncodeToString(h[:]), Label: "kr-only"}
	if _, err := testScan(t, "body.txt", body, nil); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("binary allowed: %v", err)
	}
	for _, printableBinary := range []string{"%PDF-1.7\nencoded document", "{\\rtf1 encoded document}", "IWADpayload"} {
		if _, err := testScan(t, "pretends.txt", []byte(printableBinary), nil); !errors.Is(err, ErrUnsupported) {
			t.Fatalf("unsupported signature treated as text: %v", err)
		}
	}
	r, err := testScan(t, "body.bin", body, []Review{rv})
	if err != nil || !slices.Contains(r.Labels, "kr-only") {
		t.Fatalf("exact review: %+v %v", r, err)
	}
	if _, err := testScan(t, "body.bin", append(body, 3), []Review{rv}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("stale review: %v", err)
	}
	// Even an exact global review must not suppress a supported text finding.
	text := []byte("demo@example.invalid")
	h = sha256.Sum256(text)
	r, err = testScan(t, "reviewed.txt", text, []Review{{SHA256: hex.EncodeToString(h[:]), Label: "global"}})
	if err != nil || !slices.Contains(r.Labels, "kr-only") {
		t.Fatalf("review weakened findings: %+v %v", r, err)
	}
}

func TestArchiveFailClosed(t *testing.T) {
	clean := archive(t, "ok.txt", []byte("hello"), nil)
	deep := []byte("hello")
	for i := 0; i <= MaxArchiveDepth; i++ {
		deep = archive(t, "nested.zip", deep, nil)
	}
	for name, b := range map[string][]byte{
		"traversal":          archive(t, "../secret.txt", []byte("hello"), nil),
		"backslash":          archive(t, "dir\\secret.txt", []byte("hello"), nil),
		"binary member":      archive(t, "body.dat", []byte{0xff, 0}, nil),
		"extra metadata":     archive(t, "ok.txt", []byte("hello"), []byte{0xfe, 0xca, 0, 0}),
		"corrupt":            []byte("PK\x03\x04bad"),
		"hidden trailer":     append(slices.Clone(clean), []byte("hidden content")...),
		"depth":              deep,
		"oversize expansion": archive(t, "large.txt", bytes.Repeat([]byte("a"), int(MaxObjectBytes+1)), nil),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := testScan(t, "x.zip", b, nil); err == nil {
				t.Fatal("archive unexpectedly allowed")
			}
		})
	}
	// Corrupt an otherwise structurally valid member so CRC verification fails.
	corrupt := slices.Clone(clean)
	corrupt[36] ^= 0xff
	if _, err := testScan(t, "corrupt.zip", corrupt, nil); err == nil {
		t.Fatal("corrupt member allowed")
	}
}

func TestNamesAndContentBindManifest(t *testing.T) {
	a, err := testScan(t, "first.txt", []byte("clean"), nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := testScan(t, "second.txt", []byte("clean"), nil)
	c, _ := testScan(t, "first.txt", []byte("other"), nil)
	if a.ManifestDigest == b.ManifestDigest || a.ManifestDigest == c.ManifestDigest || len(a.ManifestDigest) != 64 {
		t.Fatal("manifest does not bind keys and contents")
	}
	r, err := testScan(t, "demo@example.invalid.txt", []byte("clean"), nil)
	if err != nil || !slices.Contains(r.Labels, "kr-only") {
		t.Fatalf("key PII omitted: %+v %v", r, err)
	}
}

func TestPortablePaths(t *testing.T) {
	for _, s := range []string{"../x", "/x", "a/../../x", "a\\x", "x:stream", "a/./x", "a//b", "CON.txt", "a/NUL", "a.", "a ", "x\n", "x?"} {
		if safePath(s) {
			t.Errorf("accepted unsafe path %q", s)
		}
	}
	for _, s := range []string{"rooms/photo.txt", "한글/파일.json", ".config"} {
		if !safePath(s) {
			t.Errorf("rejected safe path %q", s)
		}
	}
}
