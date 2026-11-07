package storetest

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
)

// planToolsCases are the store.PlanTools part of Conformance.
var planToolsCases = []struct {
	name string
	run  func(t *testing.T, s store.Store)
}{
	{"WrittenAppendList", testWrittenAppendList},
	{"WrittenStats", testWrittenStats},
	{"WrittenReceiptRetry", testWrittenReceiptRetry},
	{"AttachmentPutGet", testAttachmentPutGet},
	{"AttachmentOnlyWhilePending", testAttachmentOnlyWhilePending},
	{"AttachmentCopies", testAttachmentCopies},
	{"AttachmentRaisesRevision", testAttachmentRaisesRevision},
}

func testWrittenReceiptRetry(t *testing.T, s store.Store) {
	ctx := t.Context()
	r := written("app.reservations", "91", 2)
	r.EventID, r.Scope, r.Coverage, r.Generation = "reservation-91", "application_commits", "reservation_inserts", "db-1"
	must(t, s.AppendWrittenRecords(ctx, "app-demo", []store.WrittenRecord{r, r}))
	r.ReceivedAt = at(8)
	must(t, s.AppendWrittenRecords(ctx, "app-demo", []store.WrittenRecord{r}))
	st, err := s.WrittenRecordStats(ctx, "app-demo")
	must(t, err)
	if st.Count != 1 || st.LastSeq <= 0 || st.Generation != "db-1" || st.Scope != "application_commits" || st.Coverage != "reservation_inserts" {
		t.Fatalf("retry stats: %+v", st)
	}
	for _, change := range []func(*store.WrittenRecord){func(r *store.WrittenRecord) { r.PK = "92" }, func(r *store.WrittenRecord) { r.Generation = "db-2" }, func(r *store.WrittenRecord) { r.Scope = "client_acknowledged" }, func(r *store.WrittenRecord) { r.Coverage = "reported_records" }} {
		bad := r
		change(&bad)
		fresh := r
		fresh.PK = "100"
		fresh.EventID = "reservation-100"
		if err := s.AppendWrittenRecords(ctx, "app-demo", []store.WrittenRecord{fresh, bad}); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("conflict: %v", err)
		}
		st, err = s.WrittenRecordStats(ctx, "app-demo")
		must(t, err)
		if st.Count != 1 {
			t.Fatalf("failed batch leaked a row: %+v", st)
		}
	}
}

// written is a record that happened n minutes after the base and arrived a minute later.
func written(table, pk string, n int) store.WrittenRecord {
	return store.WrittenRecord{Table: table, PK: pk, At: at(n), ReceivedAt: at(n + 1)}
}

func testWrittenAppendList(t *testing.T, s store.Store) {
	ctx := t.Context()
	list, err := s.ListWrittenRecords(ctx, "reservations", 0, 0)
	must(t, err)
	if len(list) != 0 {
		t.Fatalf("empty stream lists %d records", len(list))
	}

	must(t, s.AppendWrittenRecords(ctx, "reservations", []store.WrittenRecord{
		written("app.reservations", "3", 5), written("app.reservations", "1", 1), written("app.reservation_slots", "(3,2026-11-07,14)", 2),
	}))
	must(t, s.AppendWrittenRecords(ctx, "other", []store.WrittenRecord{written("t", "x", 0)}))
	must(t, s.AppendWrittenRecords(ctx, "reservations", nil)) // an empty batch is a no-op
	// Stream and Seq in the input are ignored.
	in := written("app.reservations", "2", 3)
	in.Stream, in.Seq = "other", 999
	must(t, s.AppendWrittenRecords(ctx, "reservations", []store.WrittenRecord{in}))

	list, err = s.ListWrittenRecords(ctx, "reservations", 0, 0)
	must(t, err)
	var got []string
	for i, r := range list {
		got = append(got, r.Table+"/"+r.PK)
		if r.Stream != "reservations" {
			t.Errorf("record %d stream = %q", i, r.Stream)
		}
		if i > 0 && r.Seq <= list[i-1].Seq {
			t.Errorf("seq not increasing: %d then %d", list[i-1].Seq, r.Seq)
		}
	}
	// Insertion order, not primary key or time order.
	eqList(t, "records", got, []string{"app.reservations/3", "app.reservations/1", "app.reservation_slots/(3,2026-11-07,14)", "app.reservations/2"})
	first := list[0]
	if !same(first.At, pg(at(5))) || !same(first.ReceivedAt, pg(at(6))) {
		t.Errorf("times = %s / %s, want %s / %s", first.At, first.ReceivedAt, pg(at(5)), pg(at(6)))
	}

	page, err := s.ListWrittenRecords(ctx, "reservations", list[0].Seq, 2)
	must(t, err)
	if len(page) != 2 || page[0].Seq != list[1].Seq || page[1].Seq != list[2].Seq {
		t.Fatalf("page after %d = %+v", list[0].Seq, page)
	}
	other, err := s.ListWrittenRecords(ctx, "other", 0, 0)
	must(t, err)
	if len(other) != 1 || other[0].PK != "x" {
		t.Fatalf("other stream = %+v", other)
	}
}

func testWrittenStats(t *testing.T, s store.Store) {
	ctx := t.Context()
	st, err := s.WrittenRecordStats(ctx, "reservations")
	must(t, err)
	if st.Count != 0 || st.LatestAt != nil {
		t.Fatalf("empty stream stats = %+v", st)
	}
	must(t, s.AppendWrittenRecords(ctx, "reservations", []store.WrittenRecord{written("t", "1", 7), written("t", "2", 3)}))
	must(t, s.AppendWrittenRecords(ctx, "reservations", []store.WrittenRecord{written("t", "3", 4)}))
	must(t, s.AppendWrittenRecords(ctx, "other", []store.WrittenRecord{written("t", "9", 50)}))
	st, err = s.WrittenRecordStats(ctx, "reservations")
	must(t, err)
	// The newest At, not the last appended.
	if st.Count != 3 || st.LatestAt == nil || !same(*st.LatestAt, pg(at(7))) {
		t.Fatalf("stats = %d, %v; want 3, %s", st.Count, st.LatestAt, pg(at(7)))
	}
}

func attachment(ref, name, content string) store.PlanAttachment {
	return store.PlanAttachment{Ref: ref, Name: name, Content: []byte(content), SHA256: "sha-" + content, Size: int64(len(content)),
		CreatedBy: "planner", CreatedAt: at(1)}
}

func eqAttachment(t *testing.T, got, want store.PlanAttachment) {
	t.Helper()
	want.CreatedAt = pg(want.CreatedAt)
	eqFields(t, fmt.Sprintf("attachment %s/%s", want.Ref, want.Name), got, want)
}

func testAttachmentPutGet(t *testing.T, s store.Store) {
	ctx := t.Context()
	must(t, s.CreatePlan(ctx, newPlan("pln_b", at(0))))
	must(t, s.CreatePlan(ctx, newPlan("pln_a", at(0))))

	_, err := s.GetPlanAttachment(ctx, "pln_a", "target.yaml")
	wantErr(t, err, store.ErrNotFound)

	a := attachment("pln_a", "target.yaml", "kind: Cluster\n")
	a.Content = append(a.Content, 0x00, 0xff) // bytes, not text
	must(t, s.PutPlanAttachment(ctx, a))
	got, err := s.GetPlanAttachment(ctx, "pln_a", "target.yaml")
	must(t, err)
	eqAttachment(t, got, a)

	// Putting the same name again replaces it.
	b := attachment("pln_a", "target.yaml", "kind: Cluster\nspec: {}\n")
	b.CreatedBy, b.CreatedAt = "alice", at(2)
	must(t, s.PutPlanAttachment(ctx, b))
	got, err = s.GetPlanAttachment(ctx, "pln_a", "target.yaml")
	must(t, err)
	eqAttachment(t, got, b)

	// An empty attachment reads back as empty, not nil.
	must(t, s.PutPlanAttachment(ctx, attachment("pln_b", "empty", "")))
	got, err = s.GetPlanAttachment(ctx, "pln_b", "empty")
	must(t, err)
	if got.Content == nil || len(got.Content) != 0 {
		t.Errorf("empty content = %s", show(got.Content))
	}
	must(t, s.PutPlanAttachment(ctx, attachment("pln_a", "Values.yaml", "x")))

	list, err := s.ListPlanAttachments(ctx, []string{"pln_b", "pln_a", "pln_missing"})
	must(t, err)
	var names []string
	for _, l := range list {
		names = append(names, l.Ref+"/"+l.Name)
		if l.Content != nil {
			t.Errorf("listing %s/%s carries content", l.Ref, l.Name)
		}
	}
	// Byte order: upper case before lower case.
	eqList(t, "attachments", names, []string{"pln_a/Values.yaml", "pln_a/target.yaml", "pln_b/empty"})
	want := b
	want.Content = nil
	eqAttachment(t, list[1], want)

	list, err = s.ListPlanAttachments(ctx, []string{"pln_missing"})
	must(t, err)
	if len(list) != 0 {
		t.Fatalf("attachments of an unknown plan: %+v", list)
	}
	_, err = s.GetPlanAttachment(ctx, "pln_b", "target.yaml")
	wantErr(t, err, store.ErrNotFound)
}

func testAttachmentOnlyWhilePending(t *testing.T, s store.Store) {
	ctx := t.Context()
	wantErr(t, s.PutPlanAttachment(ctx, attachment("pln_missing", "target.yaml", "x")), store.ErrNotFound)

	must(t, s.CreatePlan(ctx, newPlan("pln_1", at(0))))
	must(t, s.PutPlanAttachment(ctx, attachment("pln_1", "target.yaml", "approved content")))
	_, err := s.TransitionPlan(ctx, "pln_1", []string{store.PlanPending}, store.PlanApproved, store.PlanUpdate{ReviewedBy: "bob"})
	must(t, err)

	err = s.PutPlanAttachment(ctx, attachment("pln_1", "target.yaml", "changed after approval"))
	if !errors.Is(err, store.ErrConflict) || errors.Is(err, store.ErrNotFound) {
		t.Fatalf("put after approval: err = %v, want ErrConflict", err)
	}
	if !strings.Contains(err.Error(), "plan is "+store.PlanApproved) {
		t.Errorf("err %q does not name the current status", err)
	}
	wantErr(t, s.PutPlanAttachment(ctx, attachment("pln_1", "new.yaml", "x")), store.ErrConflict)

	got, err := s.GetPlanAttachment(ctx, "pln_1", "target.yaml")
	must(t, err)
	if string(got.Content) != "approved content" {
		t.Fatalf("content = %q after a refused put", got.Content)
	}
	_, err = s.GetPlanAttachment(ctx, "pln_1", "new.yaml")
	wantErr(t, err, store.ErrNotFound)
}

func testAttachmentCopies(t *testing.T, s store.Store) {
	ctx := t.Context()
	must(t, s.CreatePlan(ctx, newPlan("pln_1", at(0))))
	in := attachment("pln_1", "target.yaml", "abc")
	must(t, s.PutPlanAttachment(ctx, in))
	in.Content[0] = 'X'
	got, err := s.GetPlanAttachment(ctx, "pln_1", "target.yaml")
	must(t, err)
	got.Content[1] = 'Y'
	got, err = s.GetPlanAttachment(ctx, "pln_1", "target.yaml")
	must(t, err)
	if !bytes.Equal(got.Content, []byte("abc")) {
		t.Errorf("attachment changed through an alias: %q", got.Content)
	}
}

func testAttachmentRaisesRevision(t *testing.T, s store.Store) {
	ctx := t.Context()
	must(t, s.CreatePlan(ctx, newPlan("pln_1", at(0))))
	revision := func() int64 {
		t.Helper()
		p, err := s.GetPlan(ctx, "pln_1")
		must(t, err)
		return p.Revision
	}
	// Every put counts, a replacement with the same content too.
	for i, name := range []string{"target.yaml", "target.yaml", "values.yaml"} {
		must(t, s.PutPlanAttachment(ctx, attachment("pln_1", name, "same")))
		if got := revision(); got != int64(i+2) {
			t.Fatalf("after put %d: revision %d, want %d", i+1, got, i+2)
		}
	}
	// An approval that names an older revision does not apply.
	_, err := s.TransitionPlan(ctx, "pln_1", []string{store.PlanPending}, store.PlanApproved, store.PlanUpdate{IfRevision: 3})
	wantErr(t, err, store.ErrConflict)
	_, err = s.TransitionPlan(ctx, "pln_1", []string{store.PlanPending}, store.PlanApproved, store.PlanUpdate{IfRevision: 4})
	must(t, err)
	// A refused put leaves the revision alone.
	wantErr(t, s.PutPlanAttachment(ctx, attachment("pln_1", "target.yaml", "late")), store.ErrConflict)
	if got := revision(); got != 4 {
		t.Fatalf("after a refused put: revision %d, want 4", got)
	}
}
