// Package fileguard inspects the bytes of a bounded, private staging snapshot.
// It is a conservative content gate, not a proof that arbitrary data is PII-free.
package fileguard

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/preflight"
)

const (
	RulesVersion           = "content-v1"
	MaxObjects             = 10000
	MaxObjectBytes   int64 = 16 << 20
	MaxTotalBytes    int64 = 256 << 20
	MaxExpandedBytes int64 = 64 << 20
	MaxArchiveDepth        = 3
)

// Review is a server-owned classification of exact bytes, never a filename rule.
// It permits unsupported binary formats; it never suppresses detected findings.
type Review = preflight.ContentReview

// Report deliberately contains no object keys, content, or individual hashes.
type Report = preflight.FileInspection

type entry struct {
	Path   string
	SHA256 string
	Labels []string
}

// All returned errors are fixed codes. Never wrap archive, path, or rclone errors.
var (
	ErrUnsupported = errors.New("file_content_unsupported")
	ErrLimit       = errors.New("file_inspection_limit")
	ErrRead        = errors.New("file_inspection_read_failed")
	ErrCleanup     = errors.New("file_snapshot_cleanup_failed")
	ErrPath        = errors.New("file_inspection_unsafe_path")
	ErrChanged     = errors.New("file_snapshot_changed")
	ErrDenied      = errors.New("file_residency_denied")
	ErrTransfer    = errors.New("file_transfer_failed")
	ErrOptions     = errors.New("file_guard_options_not_supported")
)

var patterns = []struct {
	code string
	re   *regexp.Regexp
}{
	{"kr_resident_number", regexp.MustCompile(`[0-9]{6}[ -]?[1-8][0-9]{6}`)},
	{"email", regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)},
	{"kr_phone", regexp.MustCompile(`(\+82[ -]?|0)1[016789][ -]?[0-9]{3,4}[ -]?[0-9]{4}`)},
	{"payment_card_candidate", regexp.MustCompile(`([0-9][ -]?){13,19}`)},
}

// ValidReview is also used by the strict operator configuration parser.
func ValidReview(r Review) bool { return preflight.ValidContentReview(r) }

// safePath rejects traversal, ambiguous Windows names, ADS and separators. The
// same portable rules apply on Linux so a policy does not depend on its host OS.
func safePath(s string) bool {
	if s == "" || len(s) > 1024 || !utf8.ValidString(s) || path.Clean(s) != s || strings.HasPrefix(s, "/") || strings.ContainsAny(s, "\\:\x00") {
		return false
	}
	for _, p := range strings.Split(s, "/") {
		if p == "" || p == "." || p == ".." || strings.TrimRight(p, " .") != p || strings.ContainsAny(p, "<>\"|?*") {
			return false
		}
		for _, r := range p {
			if unicode.IsControl(r) {
				return false
			}
		}
		base := strings.ToUpper(strings.SplitN(p, ".", 2)[0])
		if base == "CON" || base == "PRN" || base == "AUX" || base == "NUL" || (len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) && base[3] >= '0' && base[3] <= '9') {
			return false
		}
	}
	return true
}

type scanner struct {
	reviews  map[string]string
	expanded int64
	members  int
	findings map[string]int
}

func (s *scanner) inspect(ctx context.Context, data []byte, depth int) ([]string, error) {
	if ctx.Err() != nil {
		return nil, ErrRead
	}
	h := sha256.Sum256(data)
	review := s.reviews[hex.EncodeToString(h[:])]
	labels := []string{"global"}
	if review != "" {
		labels = append(labels, review)
	}
	// Recognize archives by magic, not a caller-controlled extension. Encrypted,
	// corrupt, self-extracting and other unsupported formats require exact review.
	if bytes.HasPrefix(data, []byte("PK\x03\x04")) || bytes.HasPrefix(data, []byte("PK\x05\x06")) {
		if depth >= MaxArchiveDepth {
			return nil, ErrLimit
		}
		z, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			return nil, ErrRead
		}
		if len(z.File) > MaxObjects-s.members {
			return nil, ErrLimit
		}
		if !plainZIP(data, z) {
			return nil, ErrUnsupported
		}
		if !plainText([]byte(z.Comment)) {
			return nil, ErrUnsupported
		}
		labels = append(labels, s.textLabels([]byte(z.Comment))...)
		seen := map[string]bool{}
		for _, f := range z.File {
			s.members++
			if s.members > MaxObjects || f.UncompressedSize64 > uint64(MaxObjectBytes) {
				return nil, ErrLimit
			}
			name := strings.TrimSuffix(f.Name, "/")
			if !safePath(name) || seen[name] || f.Mode()&os.ModeSymlink != 0 || (!f.FileInfo().IsDir() && !f.Mode().IsRegular()) {
				return nil, ErrPath
			}
			seen[name] = true
			if f.Flags&1 != 0 || len(f.Extra) != 0 || !plainText([]byte(f.Comment)) {
				return nil, ErrUnsupported
			}
			labels = append(labels, s.textLabels([]byte(f.Name+"\n"+f.Comment))...)
			if f.FileInfo().IsDir() {
				if f.UncompressedSize64 != 0 || f.CompressedSize64 != 0 {
					return nil, ErrUnsupported
				}
				continue
			}
			if int64(f.UncompressedSize64) > MaxExpandedBytes-s.expanded {
				return nil, ErrLimit
			}
			r, err := f.Open()
			if err != nil {
				return nil, ErrRead
			}
			b, readErr := io.ReadAll(io.LimitReader(r, MaxObjectBytes+1))
			closeErr := r.Close()
			if int64(len(b)) > MaxObjectBytes {
				return nil, ErrLimit
			}
			if readErr != nil || closeErr != nil || uint64(len(b)) != f.UncompressedSize64 {
				return nil, ErrRead
			}
			s.expanded += int64(len(b))
			if s.expanded > MaxExpandedBytes {
				return nil, ErrLimit
			}
			ls, err := s.inspect(ctx, b, depth+1)
			if err != nil {
				return nil, err
			}
			labels = append(labels, ls...)
		}
	} else if !knownBinary(data) && plainText(data) {
		labels = append(labels, s.textLabels(data)...)
	} else if review == "" {
		return nil, ErrUnsupported
	}
	slices.Sort(labels)
	return slices.Compact(labels), nil
}

func (s *scanner) textLabels(data []byte) []string {
	var labels []string
	for _, p := range patterns {
		if p.re.Match(data) {
			s.findings[p.code]++
			labels = append(labels, "kr-only")
		}
	}
	return labels
}

// Some unsupported formats can also be entirely printable UTF-8. Do not
// mistake a PDF/RTF document or a binary container signature for plain text.
func knownBinary(b []byte) bool {
	b = bytes.TrimPrefix(b, []byte{0xef, 0xbb, 0xbf})
	for _, magic := range []string{"%PDF-", "{\\rtf", "IWAD", "PWAD", "MZ", "\x7fELF", "GIF87a", "GIF89a", "RIFF"} {
		if bytes.HasPrefix(b, []byte(magic)) {
			return true
		}
	}
	return false
}

func plainText(b []byte) bool {
	if !utf8.Valid(b) {
		return false
	}
	for _, r := range string(b) {
		if unicode.IsControl(r) && r != '\n' && r != '\r' && r != '\t' {
			return false
		}
	}
	return true
}

func scan(ctx context.Context, dir string, objects []object, reviews []Review) (Report, error) {
	s := scanner{reviews: map[string]string{}, findings: map[string]int{}}
	for _, r := range reviews {
		if !ValidReview(r) || s.reviews[r.SHA256] != "" {
			return Report{}, ErrOptions
		}
		s.reviews[r.SHA256] = r.Label
	}
	r := Report{Rules: RulesVersion, Findings: s.findings, Labels: []string{}}
	manifest := []entry{}
	for _, o := range objects {
		if ctx.Err() != nil {
			return Report{}, ErrRead
		}
		// readObject uses a bounded reader and rejects links/non-regular files.
		b, err := readObject(dir, o)
		if err != nil {
			return Report{}, err
		}
		ls, err := s.inspect(ctx, b, 0)
		if err != nil {
			return Report{}, err
		}
		ls = append(ls, s.textLabels([]byte(o.Path))...)
		slices.Sort(ls)
		ls = slices.Compact(ls)
		h := sha256.Sum256(b)
		manifest = append(manifest, entry{Path: o.Path, SHA256: hex.EncodeToString(h[:]), Labels: ls})
		r.Objects++
		r.Bytes += int64(len(b))
		r.Labels = append(r.Labels, ls...)
	}
	slices.Sort(r.Labels)
	r.Labels = slices.Compact(r.Labels)
	raw, _ := json.Marshal(struct {
		Rules   string
		Entries []entry
	}{RulesVersion, manifest})
	h := sha256.Sum256(raw)
	r.ManifestDigest = hex.EncodeToString(h[:])
	return r, nil
}
