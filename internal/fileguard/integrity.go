package fileguard

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
)

// verifyInspection checks every staged byte against the original DLP manifest.
// The manifest digest is the approval's trust anchor, so its labels cannot be
// replaced to authorize different bytes. Re-running regexes and decompressing
// archives cannot add information once these exact bytes are proven unchanged.
// This is private, per-operation evidence; every new pass still downloads and
// inspects the source again. Target verification still downloads every object.
func (s *snapshot) verifyInspection(ctx context.Context) error {
	f, err := os.Open(filepath.Join(s.root, "inspected.json"))
	if err != nil {
		return ErrChanged
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 16<<20 {
		return ErrChanged
	}
	h := sha256.New()
	d := json.NewDecoder(io.TeeReader(io.LimitReader(f, 16<<20+1), h))
	token := func(want any) bool { got, err := d.Token(); return err == nil && got == want }
	var rules string
	if !token(json.Delim('{')) || !token("Rules") || d.Decode(&rules) != nil || rules != RulesVersion || !token("Entries") || !token(json.Delim('[')) {
		return ErrChanged
	}
	if err := s.inventory.walk(func(o object) error {
		if ctx.Err() != nil || !d.More() {
			return ErrChanged
		}
		var inspected entry
		if d.Decode(&inspected) != nil || inspected.Path != o.Path {
			return ErrChanged
		}
		b, err := readObject(s.dir, o)
		if err != nil {
			return ErrChanged
		}
		digest := sha256.Sum256(b)
		if hex.EncodeToString(digest[:]) != inspected.SHA256 {
			return ErrChanged
		}
		return nil
	}); err != nil {
		return ErrChanged
	}
	if d.More() || !token(json.Delim(']')) || !token(json.Delim('}')) {
		return ErrChanged
	}
	var extra any
	if d.Decode(&extra) != io.EOF || ctx.Err() != nil || hex.EncodeToString(h.Sum(nil)) != s.report.ManifestDigest {
		return ErrChanged
	}
	return nil
}
