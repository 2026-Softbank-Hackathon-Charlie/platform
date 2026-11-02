package storetest

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
	"slices"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
)

// memoryPlanTools is the memory store's part for store.PlanTools. Its fields
// are guarded by memory.mu, like the rest of the store, so an attachment write
// and a plan transition are atomic with respect to each other.
type memoryPlanTools struct {
	written     []store.WrittenRecord // in Seq order
	writtenSeq  int64
	attachments map[[2]string]store.PlanAttachment // by ref and name; nil until the first put
}

func (m *memory) AppendWrittenRecords(_ context.Context, stream string, recs []store.WrittenRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range recs {
		m.tools.writtenSeq++
		r.Seq, r.Stream = m.tools.writtenSeq, stream
		r.At, r.ReceivedAt = pgTime(r.At), pgTime(r.ReceivedAt)
		m.tools.written = append(m.tools.written, r)
	}
	return nil
}

func (m *memory) ListWrittenRecords(_ context.Context, stream string, afterSeq int64, limit int) ([]store.WrittenRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []store.WrittenRecord{}
	for _, r := range m.tools.written {
		if limit > 0 && len(out) == limit {
			break
		}
		if r.Stream == stream && r.Seq > afterSeq {
			out = append(out, r)
		}
	}
	return out, nil
}

func (m *memory) WrittenRecordStats(_ context.Context, stream string) (store.WrittenStats, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var st store.WrittenStats
	for _, r := range m.tools.written {
		if r.Stream != stream {
			continue
		}
		st.Count++
		if st.LatestAt == nil || r.At.After(*st.LatestAt) {
			at := r.At
			st.LatestAt = &at
		}
	}
	return st, nil
}

func (m *memory) PutPlanAttachment(_ context.Context, a store.PlanAttachment) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.plans[a.Ref]
	if !ok {
		return fmt.Errorf("%w: plan %s", store.ErrNotFound, a.Ref)
	}
	if p.Status != store.PlanPending {
		return fmt.Errorf("%w: plan is %s", store.ErrConflict, p.Status)
	}
	if m.tools.attachments == nil {
		m.tools.attachments = map[[2]string]store.PlanAttachment{}
	}
	a.Content = bytes.Clone(a.Content)
	if a.Content == nil {
		a.Content = []byte{}
	}
	a.CreatedAt = pgTime(a.CreatedAt)
	m.tools.attachments[[2]string{a.Ref, a.Name}] = a
	return nil
}

func (m *memory) GetPlanAttachment(_ context.Context, ref, name string) (store.PlanAttachment, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.tools.attachments[[2]string{ref, name}]
	if !ok {
		return store.PlanAttachment{}, fmt.Errorf("%w: attachment %s of plan %s", store.ErrNotFound, name, ref)
	}
	a.Content = bytes.Clone(a.Content)
	return a, nil
}

func (m *memory) ListPlanAttachments(_ context.Context, refs []string) ([]store.PlanAttachment, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []store.PlanAttachment{}
	for k, a := range m.tools.attachments {
		if slices.Contains(refs, k[0]) {
			a.Content = nil
			out = append(out, a)
		}
	}
	// Same order as Postgres: ref, name.
	slices.SortFunc(out, func(a, b store.PlanAttachment) int {
		return cmp.Or(cmp.Compare(a.Ref, b.Ref), cmp.Compare(a.Name, b.Name))
	})
	return out, nil
}
