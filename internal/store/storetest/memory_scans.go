package storetest

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
)

// scansMemory is the memory store's part for store.ScanStore, guarded by memory.mu.
type scansMemory struct {
	byDigest map[string]store.ImageScan // nil until the first put
}

func (m *memory) PutImageScan(_ context.Context, s store.ImageScan) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for d, have := range m.scans.byDigest {
		if have.ID == s.ID && d != s.Digest {
			return fmt.Errorf("%w: scan %s belongs to another digest", store.ErrConflict, s.ID)
		}
	}
	s.FinishedAt, s.CreatedAt = pgTime(s.FinishedAt), pgTime(s.CreatedAt)
	if have, ok := m.scans.byDigest[s.Digest]; ok && have.FinishedAt.After(s.FinishedAt) {
		return fmt.Errorf("%w: digest %s has a scan that finished later", store.ErrConflict, s.Digest)
	}
	if m.scans.byDigest == nil {
		m.scans.byDigest = map[string]store.ImageScan{}
	}
	s.Doc, s.Vulnerabilities, s.Report = bytes.Clone(s.Doc), bytes.Clone(s.Vulnerabilities), bytes.Clone(s.Report)
	m.scans.byDigest[s.Digest] = s
	return nil
}

func (m *memory) ListImageScans(_ context.Context, since time.Time) ([]store.ImageScan, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []store.ImageScan{}
	for _, s := range m.scans.byDigest {
		if s.FinishedAt.Before(since) {
			continue
		}
		s.Doc, s.Vulnerabilities, s.Report = bytes.Clone(s.Doc), bytes.Clone(s.Vulnerabilities), nil
		out = append(out, s)
	}
	slices.SortFunc(out, func(a, b store.ImageScan) int { return strings.Compare(a.Digest, b.Digest) })
	return out, nil
}
