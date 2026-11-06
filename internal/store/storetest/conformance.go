package storetest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
)

// Conformance runs the store.Store contract against fresh stores from
// newStore. Subtests run in parallel, so newStore must be safe to call
// concurrently and return an empty store each time; Conformance closes it
// when the subtest ends, and newStore may register further cleanup on t.
func Conformance(t *testing.T, newStore func(t *testing.T) store.Store) {
	cases := []struct {
		name string
		run  func(t *testing.T, s store.Store)
	}{
		{"PlanCreateGet", testPlanCreateGet},
		{"PlanCreateDuplicate", testPlanCreateDuplicate},
		{"PlanGetMissing", testPlanGetMissing},
		{"PlanRawByteExact", testPlanRawByteExact},
		{"PlanList", testPlanList},
		{"PlanTransition", testPlanTransition},
		{"PlanTransitionFromList", testPlanTransitionFromList},
		{"PlanTransitionMissing", testPlanTransitionMissing},
		{"PlanTransitionConcurrent", testPlanTransitionConcurrent},
		{"PlanTransitionIfRevision", testPlanTransitionIfRevision},
		{"PlanRollbackOfUnique", testPlanRollbackOfUnique},
		{"RunCreateGetUpdate", testRunCreateGetUpdate},
		{"RunCreateDuplicate", testRunCreateDuplicate},
		{"RunMissing", testRunMissing},
		{"RunList", testRunList},
		{"Events", testEvents},
		{"EventsConcurrent", testEventsConcurrent},
		{"Audit", testAudit},
		{"Idempotency", testIdempotency},
		{"IdempotencyConcurrent", testIdempotencyConcurrent},
		{"JSONNilAndValues", testJSONNilAndValues},
		{"Copies", testCopies},
	}
	cases = append(cases, planToolsCases...)
	cases = append(cases, scanCases...)
	cases = append(cases, exceptionCases...)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			s := newStore(t)
			t.Cleanup(s.Close)
			c.run(t, s)
		})
	}
}

// at returns a time n minutes after a fixed base, in a non-UTC zone and with
// nanoseconds, so tests see the UTC conversion and microsecond truncation.
func at(n int) time.Time {
	kst := time.FixedZone("KST", 9*60*60)
	return time.Date(2026, 11, 2, 9, 0, 0, 123456789, kst).Add(time.Duration(n) * time.Minute)
}

func ptr[T any](v T) *T { return &v }

func newPlan(ref string, submittedAt time.Time) store.Plan {
	return store.Plan{
		Ref:         ref,
		PlanID:      "shop-gke-to-eks",
		Status:      store.PlanPending,
		Summary:     []byte(`{"planId": "shop-gke-to-eks", "source": {"env": "gke", "cluster": "db-gke"}, "files": true}`),
		Raw:         []byte("{\n  \"id\": \"shop-gke-to-eks\",\n  \"dsn\": \"postgres://app:${SRC_PGPASSWORD}@db/app\"\n}\n"),
		Revision:    1,
		SubmittedBy: "alice",
		SubmittedAt: submittedAt,
	}
}

func newRun(id, status string, createdAt time.Time) store.Run {
	return store.Run{
		RunID:     id,
		PlanRef:   "pln_1",
		SourceEnv: "gke",
		TargetEnv: "eks",
		Status:    status,
		Doc:       []byte(fmt.Sprintf(`{"runId": %q, "outcome": %q}`, id, status)),
		CreatedAt: createdAt,
		UpdatedAt: createdAt,
	}
}

// --- assertions ---

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func wantErr(t *testing.T, err, target error) {
	t.Helper()
	if !errors.Is(err, target) {
		t.Fatalf("err = %v, want %v", err, target)
	}
}

// canonJSON re-encodes JSON so jsonb normalization (key order, spacing) does
// not count as a difference. nil stays nil: it stands for SQL NULL.
func canonJSON(t *testing.T, b []byte) []byte {
	t.Helper()
	if b == nil {
		return nil
	}
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("invalid JSON %q: %v", b, err)
	}
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// pg is what a stored time reads back as.
func pg(t time.Time) time.Time { return t.UTC().Truncate(time.Microsecond) }

func pgPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	return ptr(pg(*t))
}

// same compares field values. Times must be equal and in UTC.
func same(g, w any) bool {
	switch gt := g.(type) {
	case time.Time:
		wt := w.(time.Time)
		return gt.Equal(wt) && gt.Location() == wt.Location()
	case *time.Time:
		wt := w.(*time.Time)
		if gt == nil || wt == nil {
			return gt == nil && wt == nil
		}
		return same(*gt, *wt)
	case []byte:
		wb := w.([]byte)
		return bytes.Equal(gt, wb) && (gt == nil) == (wb == nil)
	}
	return reflect.DeepEqual(g, w)
}

func show(v any) string {
	switch x := v.(type) {
	case []byte:
		if x == nil {
			return "nil"
		}
		return fmt.Sprintf("%q", x)
	case *time.Time:
		if x == nil {
			return "nil"
		}
		return x.String()
	case time.Time:
		return x.String()
	}
	return fmt.Sprintf("%#v", v)
}

// eqFields reports every field of two structs of the same type that differs.
func eqFields(t *testing.T, what string, got, want any) {
	t.Helper()
	gv, wv := reflect.ValueOf(got), reflect.ValueOf(want)
	for i := range gv.NumField() {
		g, w := gv.Field(i).Interface(), wv.Field(i).Interface()
		if !same(g, w) {
			t.Errorf("%s.%s = %s, want %s", what, gv.Type().Field(i).Name, show(g), show(w))
		}
	}
}

func eqPlan(t *testing.T, got, want store.Plan) {
	t.Helper()
	got.Summary, want.Summary = canonJSON(t, got.Summary), canonJSON(t, want.Summary)
	want.SubmittedAt, want.ReviewedAt = pg(want.SubmittedAt), pgPtr(want.ReviewedAt)
	eqFields(t, "plan "+want.Ref, got, want)
}

func eqRun(t *testing.T, got, want store.Run) {
	t.Helper()
	got.Doc, want.Doc = canonJSON(t, got.Doc), canonJSON(t, want.Doc)
	want.CreatedAt, want.UpdatedAt = pg(want.CreatedAt), pg(want.UpdatedAt)
	eqFields(t, "run "+want.RunID, got, want)
}

func eqEvent(t *testing.T, got, want store.Event) {
	t.Helper()
	got.Data, want.Data = canonJSON(t, got.Data), canonJSON(t, want.Data)
	want.Time = pg(want.Time)
	eqFields(t, fmt.Sprintf("event %d", want.Seq), got, want)
}

func eqAudit(t *testing.T, got, want store.Audit) {
	t.Helper()
	got.Detail, want.Detail = canonJSON(t, got.Detail), canonJSON(t, want.Detail)
	want.Time = pg(want.Time)
	eqFields(t, fmt.Sprintf("audit %d", want.ID), got, want)
}

func refs(ps []store.Plan) []string {
	out := []string{}
	for _, p := range ps {
		out = append(out, p.Ref)
	}
	return out
}

func runIDs(rs []store.Run) []string {
	out := []string{}
	for _, r := range rs {
		out = append(out, r.RunID)
	}
	return out
}

func seqs(es []store.Event) []int64 {
	out := []int64{}
	for _, e := range es {
		out = append(out, e.Seq)
	}
	return out
}

func auditIDs(as []store.Audit) []int64 {
	out := []int64{}
	for _, a := range as {
		out = append(out, a.ID)
	}
	return out
}

func eqList[T comparable](t *testing.T, what string, got, want []T) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s = %v, want %v", what, got, want)
	}
}

// parallel runs f(0..n-1) in goroutines released at the same moment.
func parallel(n int, f func(i int)) {
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range n {
		wg.Go(func() {
			<-start
			f(i)
		})
	}
	close(start)
	wg.Wait()
}

// --- plans ---

func testPlanCreateGet(t *testing.T, s store.Store) {
	ctx := t.Context()
	p := newPlan("pln_1", at(0))
	must(t, s.CreatePlan(ctx, p))
	got, err := s.GetPlan(ctx, "pln_1")
	must(t, err)
	eqPlan(t, got, p)

	// Every optional field set at creation round-trips too.
	full := newPlan("pln_2", at(1))
	full.Status = store.PlanStarted
	full.ReviewedBy, full.ReviewedAt, full.ReviewComment, full.RunID = "bob", ptr(at(2)), "lgtm", "run_1"
	full.SubmittedRole = "agent"
	full.Revision, full.RollbackOf = 7, "run_0"
	must(t, s.CreatePlan(ctx, full))
	got, err = s.GetPlan(ctx, "pln_2")
	must(t, err)
	eqPlan(t, got, full)
}

func testPlanCreateDuplicate(t *testing.T, s store.Store) {
	ctx := t.Context()
	p := newPlan("pln_1", at(0))
	must(t, s.CreatePlan(ctx, p))
	dup := newPlan("pln_1", at(5))
	dup.SubmittedBy, dup.Status = "mallory", store.PlanApproved
	wantErr(t, s.CreatePlan(ctx, dup), store.ErrConflict)
	got, err := s.GetPlan(ctx, "pln_1")
	must(t, err)
	eqPlan(t, got, p)
}

func testPlanGetMissing(t *testing.T, s store.Store) {
	_, err := s.GetPlan(t.Context(), "pln_missing")
	wantErr(t, err, store.ErrNotFound)
}

func testPlanRawByteExact(t *testing.T, s store.Store) {
	ctx := t.Context()
	raws := map[string][]byte{
		// jsonb would reorder keys, drop the duplicate and reformat; Raw must not.
		"json":   []byte("{ \"z\":1,\"a\" :  [1,2.50,3e2], \"a\": \"dup\", \"s\": \"\\u00e9${DST_PGPASSWORD}\" }\r\n\t"),
		"binary": {0x00, 0xff, 0xfe, '{', 0x80, '}'},
		"empty":  {},
		"nil":    nil,
	}
	for name, raw := range raws {
		p := newPlan("pln_"+name, at(0))
		p.Raw = raw
		must(t, s.CreatePlan(ctx, p))
		got, err := s.GetPlan(ctx, p.Ref)
		must(t, err)
		if !same(got.Raw, raw) {
			t.Errorf("%s: Raw = %s, want %s", name, show(got.Raw), show(raw))
		}
	}
}

func testPlanList(t *testing.T, s store.Store) {
	ctx := t.Context()
	list, err := s.ListPlans(ctx)
	must(t, err)
	if len(list) != 0 {
		t.Fatalf("empty store lists %d plans", len(list))
	}
	plans := []store.Plan{newPlan("pln_a", at(1)), newPlan("pln_b", at(3)), newPlan("pln_c", at(2))}
	for _, p := range plans {
		must(t, s.CreatePlan(ctx, p))
	}
	list, err = s.ListPlans(ctx)
	must(t, err)
	eqList(t, "ListPlans", refs(list), []string{"pln_b", "pln_c", "pln_a"})
	eqPlan(t, list[0], plans[1])
}

func testPlanTransition(t *testing.T, s store.Store) {
	ctx := t.Context()
	want := newPlan("pln_1", at(0))
	must(t, s.CreatePlan(ctx, want))

	check := func(got store.Plan) {
		t.Helper()
		eqPlan(t, got, want)
		stored, err := s.GetPlan(ctx, want.Ref)
		must(t, err)
		eqPlan(t, stored, want)
	}

	got, err := s.TransitionPlan(ctx, "pln_1", []string{store.PlanPending}, store.PlanApproved,
		store.PlanUpdate{ReviewedBy: "bob", ReviewedAt: ptr(at(5)), ReviewComment: "lgtm"})
	must(t, err)
	want.Status, want.ReviewedBy, want.ReviewedAt, want.ReviewComment = store.PlanApproved, "bob", ptr(at(5)), "lgtm"
	check(got)

	// An empty update changes only the status.
	got, err = s.TransitionPlan(ctx, "pln_1", []string{store.PlanApproved}, store.PlanStarting, store.PlanUpdate{})
	must(t, err)
	want.Status = store.PlanStarting
	check(got)

	// Setting one field leaves the others alone.
	got, err = s.TransitionPlan(ctx, "pln_1", []string{store.PlanStarting}, store.PlanStarted, store.PlanUpdate{RunID: "run_1"})
	must(t, err)
	want.Status, want.RunID = store.PlanStarted, "run_1"
	check(got)

	// Non-empty fields overwrite earlier values.
	got, err = s.TransitionPlan(ctx, "pln_1", []string{store.PlanStarted}, store.PlanStarted,
		store.PlanUpdate{ReviewedBy: "carol", ReviewedAt: ptr(at(9)), ReviewComment: "again", RunID: "run_2"})
	must(t, err)
	want.ReviewedBy, want.ReviewedAt, want.ReviewComment, want.RunID = "carol", ptr(at(9)), "again", "run_2"
	check(got)
}

func testPlanTransitionFromList(t *testing.T, s store.Store) {
	ctx := t.Context()
	p := newPlan("pln_1", at(0))
	must(t, s.CreatePlan(ctx, p))

	// Any status in from qualifies.
	_, err := s.TransitionPlan(ctx, "pln_1", []string{store.PlanApproved, store.PlanPending}, store.PlanApproved, store.PlanUpdate{})
	must(t, err)
	// from may include to: re-approving is allowed when the caller asks for it.
	_, err = s.TransitionPlan(ctx, "pln_1", []string{store.PlanApproved}, store.PlanApproved, store.PlanUpdate{})
	must(t, err)

	for name, from := range map[string][]string{
		"other statuses": {store.PlanPending, store.PlanStarting},
		"empty":          {},
		"nil":            nil,
	} {
		_, err := s.TransitionPlan(ctx, "pln_1", from, store.PlanStarted, store.PlanUpdate{ReviewedBy: "mallory", RunID: "run_x"})
		if !errors.Is(err, store.ErrConflict) || errors.Is(err, store.ErrNotFound) {
			t.Fatalf("%s: err = %v, want ErrConflict", name, err)
		}
		if !strings.Contains(err.Error(), "plan is "+store.PlanApproved) {
			t.Errorf("%s: err %q does not name the current status", name, err)
		}
	}
	got, err := s.GetPlan(ctx, "pln_1")
	must(t, err)
	p.Status = store.PlanApproved
	eqPlan(t, got, p) // failed transitions applied nothing
}

func testPlanTransitionMissing(t *testing.T, s store.Store) {
	ctx := t.Context()
	must(t, s.CreatePlan(ctx, newPlan("pln_other", at(0))))
	for _, from := range [][]string{{store.PlanPending}, nil} {
		_, err := s.TransitionPlan(ctx, "pln_missing", from, store.PlanApproved, store.PlanUpdate{})
		if !errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrConflict) {
			t.Fatalf("from %v: err = %v, want ErrNotFound", from, err)
		}
	}
	_, err := s.GetPlan(ctx, "pln_missing")
	wantErr(t, err, store.ErrNotFound)
}

func testPlanTransitionConcurrent(t *testing.T, s store.Store) {
	ctx := t.Context()
	must(t, s.CreatePlan(ctx, newPlan("pln_1", at(0))))

	const n = 16
	errs := make([]error, n)
	parallel(n, func(i int) {
		_, errs[i] = s.TransitionPlan(ctx, "pln_1", []string{store.PlanPending}, store.PlanStarting,
			store.PlanUpdate{RunID: fmt.Sprintf("run_%d", i)})
	})
	winner := -1
	for i, err := range errs {
		switch {
		case err == nil:
			if winner >= 0 {
				t.Fatalf("goroutines %d and %d both moved the plan", winner, i)
			}
			winner = i
		case errors.Is(err, store.ErrConflict):
			if !strings.Contains(err.Error(), "plan is "+store.PlanStarting) {
				t.Errorf("loser %d: err %q does not name the current status", i, err)
			}
		default:
			t.Fatalf("goroutine %d: %v", i, err)
		}
	}
	if winner < 0 {
		t.Fatal("no goroutine moved the plan")
	}
	got, err := s.GetPlan(ctx, "pln_1")
	must(t, err)
	if got.Status != store.PlanStarting || got.RunID != fmt.Sprintf("run_%d", winner) {
		t.Fatalf("plan = %s/%s, want %s/run_%d", got.Status, got.RunID, store.PlanStarting, winner)
	}
}

func testPlanTransitionIfRevision(t *testing.T, s store.Store) {
	ctx := t.Context()
	p := newPlan("pln_1", at(0))
	p.Revision = 3
	must(t, s.CreatePlan(ctx, p))

	_, err := s.TransitionPlan(ctx, "pln_1", []string{store.PlanPending}, store.PlanApproved, store.PlanUpdate{ReviewedBy: "bob", IfRevision: 2})
	if !errors.Is(err, store.ErrConflict) || !strings.Contains(err.Error(), "revision is 3") {
		t.Fatalf("stale revision: err = %v, want ErrConflict naming revision 3", err)
	}
	// A status mismatch is reported as such, whatever the revision.
	_, err = s.TransitionPlan(ctx, "pln_1", []string{store.PlanApproved}, store.PlanStarting, store.PlanUpdate{IfRevision: 2})
	if !errors.Is(err, store.ErrConflict) || !strings.Contains(err.Error(), "plan is "+store.PlanPending) {
		t.Fatalf("wrong status: err = %v", err)
	}
	got, err := s.GetPlan(ctx, "pln_1")
	must(t, err)
	eqPlan(t, got, p) // nothing applied

	got, err = s.TransitionPlan(ctx, "pln_1", []string{store.PlanPending}, store.PlanApproved, store.PlanUpdate{ReviewedBy: "bob", IfRevision: 3})
	must(t, err)
	p.Status, p.ReviewedBy = store.PlanApproved, "bob"
	eqPlan(t, got, p)
}

func testPlanRollbackOfUnique(t *testing.T, s store.Store) {
	ctx := t.Context()
	a := newPlan("pln_a", at(0))
	a.Status, a.RollbackOf = store.PlanApproved, "run_1"
	must(t, s.CreatePlan(ctx, a))

	b := newPlan("pln_b", at(1))
	b.Status, b.RollbackOf = store.PlanApproved, "run_1"
	err := s.CreatePlan(ctx, b)
	if !errors.Is(err, store.ErrConflict) || !strings.Contains(err.Error(), "run_1") {
		t.Fatalf("second rollback plan of run_1: err = %v, want ErrConflict", err)
	}
	_, err = s.GetPlan(ctx, "pln_b")
	wantErr(t, err, store.ErrNotFound)

	// Other runs, plans that roll back nothing and rejected plans do not collide.
	c := newPlan("pln_c", at(2))
	c.RollbackOf = "run_2"
	must(t, s.CreatePlan(ctx, c))
	must(t, s.CreatePlan(ctx, newPlan("pln_d", at(3))))
	must(t, s.CreatePlan(ctx, newPlan("pln_e", at(4))))
	r := newPlan("pln_r", at(5))
	r.Status, r.RollbackOf = store.PlanRejected, "run_1"
	must(t, s.CreatePlan(ctx, r))
	got, err := s.GetPlan(ctx, "pln_a")
	must(t, err)
	eqPlan(t, got, a)
}

// --- runs ---

func testRunCreateGetUpdate(t *testing.T, s store.Store) {
	ctx := t.Context()
	r := newRun("run_1", "running", at(0))
	must(t, s.CreateRun(ctx, r))
	got, err := s.GetRun(ctx, "run_1")
	must(t, err)
	eqRun(t, got, r)

	u := store.Run{
		RunID:     "run_1",
		PlanRef:   "pln_2",
		SourceEnv: "aks",
		TargetEnv: "gke",
		Status:    "succeeded",
		Doc:       []byte(`{"runId": "run_1", "outcome": "succeeded", "steps": [{"name": "copy"}]}`),
		CreatedAt: at(99), // not replaced
		UpdatedAt: at(5),
	}
	must(t, s.UpdateRun(ctx, u))
	got, err = s.GetRun(ctx, "run_1")
	must(t, err)
	want := u
	want.CreatedAt = r.CreatedAt
	eqRun(t, got, want)

	u.Doc, u.UpdatedAt = nil, at(6)
	must(t, s.UpdateRun(ctx, u))
	got, err = s.GetRun(ctx, "run_1")
	must(t, err)
	want.Doc, want.UpdatedAt = nil, at(6)
	eqRun(t, got, want)
}

func testRunCreateDuplicate(t *testing.T, s store.Store) {
	ctx := t.Context()
	r := newRun("run_1", "running", at(0))
	must(t, s.CreateRun(ctx, r))
	wantErr(t, s.CreateRun(ctx, newRun("run_1", "failed", at(3))), store.ErrConflict)
	got, err := s.GetRun(ctx, "run_1")
	must(t, err)
	eqRun(t, got, r)
}

func testRunMissing(t *testing.T, s store.Store) {
	ctx := t.Context()
	_, err := s.GetRun(ctx, "run_missing")
	wantErr(t, err, store.ErrNotFound)
	wantErr(t, s.UpdateRun(ctx, newRun("run_missing", "running", at(0))), store.ErrNotFound)
	_, err = s.GetRun(ctx, "run_missing")
	wantErr(t, err, store.ErrNotFound) // UpdateRun does not insert
}

func testRunList(t *testing.T, s store.Store) {
	ctx := t.Context()
	list, err := s.ListRuns(ctx)
	must(t, err)
	if len(list) != 0 {
		t.Fatalf("empty store lists %d runs", len(list))
	}
	runs := []store.Run{
		newRun("run_1", "running", at(1)),
		newRun("run_2", "succeeded", at(3)),
		newRun("run_3", "running", at(2)),
		newRun("run_4", "failed", at(4)),
	}
	for _, r := range runs {
		must(t, s.CreateRun(ctx, r))
	}
	// Order is by CreatedAt, so a later update does not move a run.
	runs[0].UpdatedAt = at(10)
	must(t, s.UpdateRun(ctx, runs[0]))

	list, err = s.ListRuns(ctx)
	must(t, err)
	eqList(t, "ListRuns", runIDs(list), []string{"run_4", "run_2", "run_3", "run_1"})
	eqRun(t, list[3], runs[0])

	for _, c := range []struct {
		statuses []string
		want     []string
	}{
		{[]string{"running"}, []string{"run_3", "run_1"}},
		{[]string{"running", "failed"}, []string{"run_4", "run_3", "run_1"}},
		{[]string{"unknown"}, []string{}},
		{nil, []string{}},
	} {
		list, err := s.ListRunsByStatus(ctx, c.statuses...)
		must(t, err)
		eqList(t, fmt.Sprintf("ListRunsByStatus(%v)", c.statuses), runIDs(list), c.want)
	}
	list, err = s.ListRunsByStatus(ctx, "succeeded")
	must(t, err)
	eqRun(t, list[0], runs[1])
}

// --- events ---

func testEvents(t *testing.T, s store.Store) {
	ctx := t.Context()
	var a, all []store.Event
	var last int64
	for i := range 6 {
		e := store.Event{
			RunID:   []string{"run_a", "run_b"}[i%2],
			Time:    at(i),
			Kind:    "step",
			Step:    fmt.Sprintf("step-%d", i),
			Message: fmt.Sprintf("message %d", i),
		}
		if i%3 != 0 {
			e.Data = []byte(fmt.Sprintf(`{"i": %d, "nested": {"ok": true}}`, i))
		}
		got, err := s.AppendEvent(ctx, e)
		must(t, err)
		if got.Seq <= last {
			t.Fatalf("Seq %d after %d: not strictly increasing", got.Seq, last)
		}
		last = got.Seq
		e.Seq = got.Seq
		eqEvent(t, got, e)
		all = append(all, e)
		if e.RunID == "run_a" {
			a = append(a, e)
		}
	}

	list, err := s.ListEvents(ctx, "run_a", 0, 0)
	must(t, err)
	eqList(t, "ListEvents(run_a)", seqs(list), seqs(a))
	for i := range list {
		eqEvent(t, list[i], a[i])
	}

	for _, c := range []struct {
		after int64
		limit int
		want  []store.Event
	}{
		{a[0].Seq, 0, a[1:]},
		{all[1].Seq, 0, a[1:]}, // afterSeq from another run's event
		{0, 2, a[:2]},
		{a[0].Seq, 1, a[1:2]},
		{0, -1, a},
		{0, 100, a},
		{a[2].Seq, 0, nil},
	} {
		list, err := s.ListEvents(ctx, "run_a", c.after, c.limit)
		must(t, err)
		eqList(t, fmt.Sprintf("ListEvents(run_a, %d, %d)", c.after, c.limit), seqs(list), seqs(c.want))
	}

	list, err = s.ListEvents(ctx, "run_none", 0, 0)
	must(t, err)
	if len(list) != 0 {
		t.Fatalf("unknown run lists %d events", len(list))
	}
}

func testEventsConcurrent(t *testing.T, s store.Store) {
	ctx := t.Context()
	const n, each = 16, 5
	errs := make([]error, n)
	got := make([][]int64, n)
	parallel(n, func(i int) {
		run := fmt.Sprintf("run_%d", i%2)
		for j := range each {
			e, err := s.AppendEvent(ctx, store.Event{RunID: run, Time: at(j), Kind: "log", Message: fmt.Sprint(i, j)})
			if err != nil {
				errs[i] = err
				return
			}
			got[i] = append(got[i], e.Seq)
		}
	})
	seen := map[int64]bool{}
	for i := range n {
		must(t, errs[i])
		for j, seq := range got[i] {
			if seen[seq] {
				t.Fatalf("Seq %d assigned twice", seq)
			}
			seen[seq] = true
			if j > 0 && seq <= got[i][j-1] {
				t.Fatalf("goroutine %d: Seq %d after %d", i, seq, got[i][j-1])
			}
		}
	}
	total := 0
	for _, run := range []string{"run_0", "run_1"} {
		list, err := s.ListEvents(ctx, run, 0, 0)
		must(t, err)
		for j := 1; j < len(list); j++ {
			if list[j].Seq <= list[j-1].Seq {
				t.Fatalf("%s: listed Seq %d after %d", run, list[j].Seq, list[j-1].Seq)
			}
		}
		total += len(list)
	}
	if total != n*each {
		t.Fatalf("listed %d events, want %d", total, n*each)
	}
}

// --- audit ---

func testAudit(t *testing.T, s store.Store) {
	ctx := t.Context()
	list, err := s.ListAudit(ctx, 0, 10)
	must(t, err)
	if len(list) != 0 {
		t.Fatalf("empty store lists %d audit entries", len(list))
	}

	var entries []store.Audit // oldest first
	for i := range 5 {
		a := store.Audit{
			Time:      at(i),
			Actor:     "alice",
			Role:      "operator",
			Action:    "POST /v1/plans/{ref}/approve",
			Target:    fmt.Sprintf("/v1/plans/pln_%d/approve", i),
			Status:    200 + i,
			RequestID: fmt.Sprintf("req_%d", i),
		}
		if i%2 == 0 {
			a.Detail = []byte(fmt.Sprintf(`{"i": %d}`, i))
		}
		id, err := s.AppendAudit(ctx, a)
		must(t, err)
		if len(entries) > 0 && id <= entries[len(entries)-1].ID {
			t.Fatalf("ID %d after %d: not increasing", id, entries[len(entries)-1].ID)
		}
		a.ID = id
		entries = append(entries, a)
	}
	ids := auditIDs(entries) // oldest first
	newest := func(xs ...int) []int64 {
		out := []int64{}
		for _, i := range xs {
			out = append(out, ids[i])
		}
		return out
	}

	list, err = s.ListAudit(ctx, 0, 10)
	must(t, err)
	eqList(t, "ListAudit(0, 10)", auditIDs(list), newest(4, 3, 2, 1, 0))
	for i, a := range list {
		eqAudit(t, a, entries[4-i])
	}

	for _, c := range []struct {
		before int64
		limit  int
		want   []int64
	}{
		{0, 2, newest(4, 3)},
		{-1, 2, newest(4, 3)},
		{ids[3], 2, newest(2, 1)},
		{ids[1], 2, newest(0)},
		{ids[0], 2, newest()},
		{ids[4] + 1000, 1, newest(4)},
		{ids[2], 0, newest(1, 0)}, // limit <= 0: no limit, as in ListEvents
	} {
		list, err := s.ListAudit(ctx, c.before, c.limit)
		must(t, err)
		eqList(t, fmt.Sprintf("ListAudit(%d, %d)", c.before, c.limit), auditIDs(list), c.want)
	}

	// Paging with the last ID of each page walks every entry once.
	var paged []int64
	before := int64(0)
	for {
		page, err := s.ListAudit(ctx, before, 2)
		must(t, err)
		if len(page) == 0 {
			break
		}
		paged = append(paged, auditIDs(page)...)
		before = page[len(page)-1].ID
	}
	eqList(t, "paged audit", paged, newest(4, 3, 2, 1, 0))
}

// --- idempotency ---

func testIdempotency(t *testing.T, s store.Store) {
	ctx := t.Context()
	_, _, err := s.GetIdempotencyKey(ctx, "k1")
	wantErr(t, err, store.ErrNotFound)

	pr, rid, err := s.PutIdempotencyKey(ctx, "k1", "pln_1", "run_1")
	must(t, err)
	if pr != "pln_1" || rid != "run_1" {
		t.Fatalf("Put returned %s/%s, want pln_1/run_1", pr, rid)
	}
	for _, retry := range [][2]string{{"pln_2", "run_2"}, {"pln_1", "run_1"}, {"pln_1", ""}} {
		pr, rid, err := s.PutIdempotencyKey(ctx, "k1", retry[0], retry[1])
		wantErr(t, err, store.ErrConflict)
		if pr != "pln_1" || rid != "run_1" {
			t.Fatalf("Put(k1, %v) returned %s/%s, want the stored pln_1/run_1", retry, pr, rid)
		}
	}
	pr, rid, err = s.GetIdempotencyKey(ctx, "k1")
	must(t, err)
	if pr != "pln_1" || rid != "run_1" {
		t.Fatalf("Get(k1) = %s/%s, want pln_1/run_1", pr, rid)
	}

	// An empty run id is a value like any other.
	_, _, err = s.PutIdempotencyKey(ctx, "k2", "pln_1", "")
	must(t, err)
	pr, rid, err = s.GetIdempotencyKey(ctx, "k2")
	must(t, err)
	if pr != "pln_1" || rid != "" {
		t.Fatalf("Get(k2) = %s/%q, want pln_1/\"\"", pr, rid)
	}
}

func testIdempotencyConcurrent(t *testing.T, s store.Store) {
	ctx := t.Context()
	const n = 16
	type result struct {
		planRef, runID string
		err            error
	}
	res := make([]result, n)
	parallel(n, func(i int) {
		pr, rid, err := s.PutIdempotencyKey(ctx, "k", fmt.Sprintf("pln_%d", i), fmt.Sprintf("run_%d", i))
		res[i] = result{pr, rid, err}
	})
	winner := -1
	for i, r := range res {
		if r.err == nil {
			if winner >= 0 {
				t.Fatalf("goroutines %d and %d both stored the key", winner, i)
			}
			winner = i
		} else if !errors.Is(r.err, store.ErrConflict) {
			t.Fatalf("goroutine %d: %v", i, r.err)
		}
	}
	if winner < 0 {
		t.Fatal("no goroutine stored the key")
	}
	for i, r := range res {
		if r.planRef != fmt.Sprintf("pln_%d", winner) || r.runID != fmt.Sprintf("run_%d", winner) {
			t.Fatalf("goroutine %d got %s/%s, want the winner's pln_%d/run_%d", i, r.planRef, r.runID, winner, winner)
		}
	}
}

// --- shared ---

func testJSONNilAndValues(t *testing.T, s store.Store) {
	ctx := t.Context()
	values := map[string][]byte{
		"nil":    nil,            // SQL NULL
		"null":   []byte("null"), // a JSON null is a value, not SQL NULL
		"empty":  []byte("{}"),   // empty object
		"nested": []byte(`{"b": [1, "x", {"c": null}], "a": {"d": false}}`),
		"array":  []byte(` [1, 2] `), // whitespace is not preserved, the value is
	}
	for name, v := range values {
		p := newPlan("pln_"+name, at(0))
		p.Summary = v
		must(t, s.CreatePlan(ctx, p))
		gotPlan, err := s.GetPlan(ctx, p.Ref)
		must(t, err)
		eqPlan(t, gotPlan, p)

		r := newRun("run_"+name, "running", at(0))
		r.Doc = v
		must(t, s.CreateRun(ctx, r))
		gotRun, err := s.GetRun(ctx, r.RunID)
		must(t, err)
		eqRun(t, gotRun, r)

		e := store.Event{RunID: "run_" + name, Time: at(0), Kind: "log", Data: v}
		gotEvent, err := s.AppendEvent(ctx, e)
		must(t, err)
		e.Seq = gotEvent.Seq
		eqEvent(t, gotEvent, e)
		listed, err := s.ListEvents(ctx, e.RunID, 0, 0)
		must(t, err)
		if len(listed) != 1 {
			t.Fatalf("%s: listed %d events, want 1", name, len(listed))
		}
		eqEvent(t, listed[0], e)

		a := store.Audit{Time: at(0), Actor: "alice", Target: name, Status: 201, Detail: v}
		a.ID, err = s.AppendAudit(ctx, a)
		must(t, err)
		audit, err := s.ListAudit(ctx, a.ID+1, 1)
		must(t, err)
		if len(audit) != 1 {
			t.Fatalf("%s: listed %d audit entries, want 1", name, len(audit))
		}
		eqAudit(t, audit[0], a)
	}

	// Invalid JSON is rejected, as a jsonb column would, and nothing is stored.
	bad := newPlan("pln_bad", at(0))
	bad.Summary = []byte(`{"unterminated":`)
	if err := s.CreatePlan(ctx, bad); err == nil {
		t.Fatal("CreatePlan accepted an invalid summary")
	}
	_, err := s.GetPlan(ctx, "pln_bad")
	wantErr(t, err, store.ErrNotFound)
}

func testCopies(t *testing.T, s store.Store) {
	ctx := t.Context()

	// Mutating what was passed in does not change the stored plan.
	in := newPlan("pln_1", at(0))
	in.ReviewedAt = ptr(at(1))
	orig := in
	orig.Summary, orig.Raw, orig.ReviewedAt = bytes.Clone(in.Summary), bytes.Clone(in.Raw), ptr(*in.ReviewedAt)
	must(t, s.CreatePlan(ctx, in))
	in.Raw[0], in.Summary[0] = 'X', ' '
	*in.ReviewedAt = at(50)

	// Mutating what was returned does not either.
	got, err := s.GetPlan(ctx, "pln_1")
	must(t, err)
	got.Raw[0], got.Summary[0] = 'Y', ' '
	*got.ReviewedAt = at(60)
	list, err := s.ListPlans(ctx)
	must(t, err)
	list[0].Raw[1] = 'Z'
	*list[0].ReviewedAt = at(70)

	reviewedAt := at(2)
	moved, err := s.TransitionPlan(ctx, "pln_1", []string{store.PlanPending}, store.PlanApproved, store.PlanUpdate{ReviewedAt: &reviewedAt})
	must(t, err)
	reviewedAt = at(80)
	moved.Raw[2] = 'W'
	*moved.ReviewedAt = at(90)

	got, err = s.GetPlan(ctx, "pln_1")
	must(t, err)
	orig.Status, orig.ReviewedAt = store.PlanApproved, ptr(at(2))
	eqPlan(t, got, orig)

	// Same for runs, events and audit entries.
	r := newRun("run_1", "running", at(0))
	origDoc := bytes.Clone(r.Doc)
	must(t, s.CreateRun(ctx, r))
	r.Doc[0] = ' '
	gotRun, err := s.GetRun(ctx, "run_1")
	must(t, err)
	gotRun.Doc[0] = ' '
	gotRun, err = s.GetRun(ctx, "run_1")
	must(t, err)
	if !bytes.Equal(canonJSON(t, gotRun.Doc), canonJSON(t, origDoc)) {
		t.Errorf("run doc changed through an alias: %s", gotRun.Doc)
	}

	e := store.Event{RunID: "run_1", Time: at(0), Kind: "log", Data: []byte(`{"a": 1}`)}
	appended, err := s.AppendEvent(ctx, e)
	must(t, err)
	e.Data[0], appended.Data[0] = ' ', ' '
	events, err := s.ListEvents(ctx, "run_1", 0, 0)
	must(t, err)
	events[0].Data[0] = ' '
	events, err = s.ListEvents(ctx, "run_1", 0, 0)
	must(t, err)
	if !bytes.Equal(canonJSON(t, events[0].Data), []byte(`{"a":1}`)) {
		t.Errorf("event data changed through an alias: %s", events[0].Data)
	}

	a := store.Audit{Time: at(0), Actor: "alice", Detail: []byte(`{"a": 1}`)}
	_, err = s.AppendAudit(ctx, a)
	must(t, err)
	a.Detail[0] = ' '
	audit, err := s.ListAudit(ctx, 0, 1)
	must(t, err)
	audit[0].Detail[0] = ' '
	audit, err = s.ListAudit(ctx, 0, 1)
	must(t, err)
	if !bytes.Equal(canonJSON(t, audit[0].Detail), []byte(`{"a":1}`)) {
		t.Errorf("audit detail changed through an alias: %s", audit[0].Detail)
	}
}
