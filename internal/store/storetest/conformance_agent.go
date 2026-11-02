package storetest

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
)

// ConformanceAgent runs the store.AgentStore contract against fresh stores
// from newStore, with the same rules as Conformance.
func ConformanceAgent(t *testing.T, newStore func(t *testing.T) store.Store) {
	cases := []struct {
		name string
		run  func(t *testing.T, s store.Store)
	}{
		{"AgentSessionCreateGet", testAgentSessionCreateGet},
		{"AgentSessionList", testAgentSessionList},
		{"AgentSessionFinish", testAgentSessionFinish},
		{"AgentEvents", testAgentEvents},
		{"AgentEventsAfterFinish", testAgentEventsAfterFinish},
		{"AgentAppendFinishRace", testAgentAppendFinishRace},
		{"AgentCopies", testAgentCopies},
		{"AgentText", testAgentText},
		{"AgentIdle", testAgentIdle},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			s := newStore(t)
			t.Cleanup(s.Close)
			c.run(t, s)
		})
	}
}

func newAgentSession(id string, createdAt int) store.AgentSession {
	return store.AgentSession{
		ID:        id,
		Kind:      "plan",
		Title:     "plan gke -> eks",
		Actor:     "planner",
		PlanRef:   "pln_1",
		Model:     "claude-test",
		Status:    store.AgentRunning,
		CreatedAt: at(createdAt),
	}
}

func eqAgentSession(t *testing.T, got, want store.AgentSession) {
	t.Helper()
	want.CreatedAt, want.EndedAt = pg(want.CreatedAt), pgPtr(want.EndedAt)
	eqFields(t, "agent session "+want.ID, got, want)
}

func eqAgentEvent(t *testing.T, got, want store.AgentEvent) {
	t.Helper()
	got.Input, want.Input = canonJSON(t, got.Input), canonJSON(t, want.Input)
	got.Output, want.Output = canonJSON(t, got.Output), canonJSON(t, want.Output)
	want.Time = pg(want.Time)
	eqFields(t, fmt.Sprintf("agent event %d", want.Seq), got, want)
}

func agentSeqs(es []store.AgentEvent) []int64 {
	out := []int64{}
	for _, e := range es {
		out = append(out, e.Seq)
	}
	return out
}

func sessionIDs(as []store.AgentSession) []string {
	out := []string{}
	for _, a := range as {
		out = append(out, a.ID)
	}
	return out
}

func testAgentSessionCreateGet(t *testing.T, s store.Store) {
	ctx := t.Context()
	a := newAgentSession("ags_1", 0)
	must(t, s.CreateAgentSession(ctx, a))
	got, err := s.GetAgentSession(ctx, "ags_1")
	must(t, err)
	eqAgentSession(t, got, a)

	wantErr(t, s.CreateAgentSession(ctx, newAgentSession("ags_1", 1)), store.ErrConflict)
	_, err = s.GetAgentSession(ctx, "ags_missing")
	wantErr(t, err, store.ErrNotFound)
}

func testAgentSessionList(t *testing.T, s store.Store) {
	ctx := t.Context()
	list, err := s.ListAgentSessions(ctx, store.AgentSessionFilter{})
	must(t, err)
	if list == nil || len(list) != 0 {
		t.Fatalf("empty store lists %v, want an empty non-nil slice", list)
	}
	for i, c := range []struct{ id, plan, run string }{
		{"ags_a", "pln_1", ""},
		{"ags_b", "pln_2", "run_2"},
		{"ags_c", "pln_1", "run_1"},
		{"ags_d", "", "run_1"},
	} {
		a := newAgentSession(c.id, i)
		a.PlanRef, a.RunID = c.plan, c.run
		must(t, s.CreateAgentSession(ctx, a))
	}
	// Same created_at: the id breaks the tie, descending.
	tie := newAgentSession("ags_e", 3)
	must(t, s.CreateAgentSession(ctx, tie))

	for _, c := range []struct {
		f    store.AgentSessionFilter
		want []string
	}{
		{store.AgentSessionFilter{}, []string{"ags_e", "ags_d", "ags_c", "ags_b", "ags_a"}},
		{store.AgentSessionFilter{PlanRef: "pln_1"}, []string{"ags_e", "ags_c", "ags_a"}},
		{store.AgentSessionFilter{RunID: "run_1"}, []string{"ags_d", "ags_c"}},
		{store.AgentSessionFilter{PlanRef: "pln_1", RunID: "run_1"}, []string{"ags_c"}},
		{store.AgentSessionFilter{PlanRef: "pln_none"}, []string{}},
		{store.AgentSessionFilter{Limit: 2}, []string{"ags_e", "ags_d"}},
		{store.AgentSessionFilter{Limit: -1}, []string{"ags_e", "ags_d", "ags_c", "ags_b", "ags_a"}},
	} {
		list, err := s.ListAgentSessions(ctx, c.f)
		must(t, err)
		eqList(t, fmt.Sprintf("ListAgentSessions(%+v)", c.f), sessionIDs(list), c.want)
	}
}

func testAgentSessionFinish(t *testing.T, s store.Store) {
	ctx := t.Context()
	a := newAgentSession("ags_1", 0)
	must(t, s.CreateAgentSession(ctx, a))
	got, err := s.FinishAgentSession(ctx, "ags_1", store.AgentSucceeded, "plan submitted as pln_1", at(5))
	must(t, err)
	a.Status, a.Summary, a.EndedAt = store.AgentSucceeded, "plan submitted as pln_1", ptr(at(5))
	eqAgentSession(t, got, a)
	got, err = s.GetAgentSession(ctx, "ags_1")
	must(t, err)
	eqAgentSession(t, got, a)

	_, err = s.FinishAgentSession(ctx, "ags_1", store.AgentFailed, "again", at(6))
	wantErr(t, err, store.ErrConflict)
	if !strings.Contains(err.Error(), store.AgentSucceeded) {
		t.Errorf("conflict %q does not name the current status", err)
	}
	got, err = s.GetAgentSession(ctx, "ags_1")
	must(t, err)
	eqAgentSession(t, got, a) // unchanged by the refused finish

	_, err = s.FinishAgentSession(ctx, "ags_missing", store.AgentFailed, "", at(6))
	wantErr(t, err, store.ErrNotFound)
}

func testAgentEvents(t *testing.T, s store.Store) {
	ctx := t.Context()
	must(t, s.CreateAgentSession(ctx, newAgentSession("ags_a", 0)))
	must(t, s.CreateAgentSession(ctx, newAgentSession("ags_b", 1)))

	var a, all []store.AgentEvent
	var last int64
	for i := range 6 {
		e := store.AgentEvent{
			SessionID: []string{"ags_a", "ags_b"}[i%2],
			Time:      at(i),
			Type:      []string{"tool_call", "tool_result", "message"}[i%3],
			Tool:      "list_envs",
			Status:    "ok",
		}
		switch i % 3 {
		case 0:
			e.Input = []byte(`{"env": "gke", "n": 12345678901234567}`)
		case 1:
			e.Output = []byte(`["a", {"b": null}]`)
			e.DurationMillis = ptr(int64(100 * i))
			e.InputTokens, e.OutputTokens = int64(10*i), int64(i)
		case 2:
			e.Output = []byte(`"plain text"`)
			e.DurationMillis = ptr(int64(0)) // zero is kept, not turned into nil
		}
		got, err := s.AppendAgentEvent(ctx, e)
		must(t, err)
		if got.Seq <= last {
			t.Fatalf("Seq %d after %d: not strictly increasing", got.Seq, last)
		}
		last = got.Seq
		e.Seq = got.Seq
		eqAgentEvent(t, got, e)
		all = append(all, e)
		if e.SessionID == "ags_a" {
			a = append(a, e)
		}
	}

	list, err := s.ListAgentEvents(ctx, "ags_a", 0, 0)
	must(t, err)
	eqList(t, "ListAgentEvents(ags_a)", agentSeqs(list), agentSeqs(a))
	for i := range list {
		eqAgentEvent(t, list[i], a[i])
	}
	for _, c := range []struct {
		after int64
		limit int
		want  []store.AgentEvent
	}{
		{a[0].Seq, 0, a[1:]},
		{all[1].Seq, 0, a[1:]},
		{0, 2, a[:2]},
		{0, -1, a},
		{a[2].Seq, 0, nil},
	} {
		list, err := s.ListAgentEvents(ctx, "ags_a", c.after, c.limit)
		must(t, err)
		eqList(t, fmt.Sprintf("ListAgentEvents(ags_a, %d, %d)", c.after, c.limit), agentSeqs(list), agentSeqs(c.want))
	}

	// Totals: a holds events 0 (tool_call), 2 (message), 4 (tool_result).
	got, err := s.GetAgentSession(ctx, "ags_a")
	must(t, err)
	if want := (store.AgentTotals{Events: 3, ToolCalls: 1, InputTokens: 40, OutputTokens: 4, DurationMillis: 400}); got.Totals != want {
		t.Fatalf("totals of ags_a = %+v, want %+v", got.Totals, want)
	}
	listed, err := s.ListAgentSessions(ctx, store.AgentSessionFilter{})
	must(t, err)
	if want := (store.AgentTotals{Events: 3, ToolCalls: 1, InputTokens: 10, OutputTokens: 1, DurationMillis: 100}); listed[0].ID != "ags_b" || listed[0].Totals != want {
		t.Fatalf("listed %s totals = %+v, want ags_b %+v", listed[0].ID, listed[0].Totals, want)
	}

	_, err = s.AppendAgentEvent(ctx, store.AgentEvent{SessionID: "ags_missing", Time: at(0), Type: "message"})
	wantErr(t, err, store.ErrNotFound)
	list, err = s.ListAgentEvents(ctx, "ags_missing", 0, 0)
	must(t, err)
	if len(list) != 0 {
		t.Fatalf("unknown session lists %d events", len(list))
	}
}

func testAgentEventsAfterFinish(t *testing.T, s store.Store) {
	ctx := t.Context()
	must(t, s.CreateAgentSession(ctx, newAgentSession("ags_1", 0)))
	_, err := s.AppendAgentEvent(ctx, store.AgentEvent{SessionID: "ags_1", Time: at(1), Type: "message"})
	must(t, err)
	_, err = s.FinishAgentSession(ctx, "ags_1", store.AgentCancelled, "", at(2))
	must(t, err)
	_, err = s.AppendAgentEvent(ctx, store.AgentEvent{SessionID: "ags_1", Time: at(3), Type: "message"})
	wantErr(t, err, store.ErrConflict)
	list, err := s.ListAgentEvents(ctx, "ags_1", 0, 0)
	must(t, err)
	if len(list) != 1 {
		t.Fatalf("%d events after a refused append, want 1", len(list))
	}
}

// testAgentAppendFinishRace appends from several goroutines while another
// finishes the session: every append either lands before the finish or is
// refused, and the stored events are exactly the accepted ones in Seq order.
func testAgentAppendFinishRace(t *testing.T, s store.Store) {
	ctx := t.Context()
	must(t, s.CreateAgentSession(ctx, newAgentSession("ags_1", 0)))
	const n, each = 8, 5
	var mu sync.Mutex
	var accepted []int64
	errs := make([]error, n+1)
	parallel(n+1, func(i int) {
		if i == n {
			_, errs[i] = s.FinishAgentSession(ctx, "ags_1", store.AgentSucceeded, "", at(1))
			return
		}
		for range each {
			e, err := s.AppendAgentEvent(ctx, store.AgentEvent{SessionID: "ags_1", Time: at(0), Type: "tool_call"})
			switch {
			case err == nil:
				mu.Lock()
				accepted = append(accepted, e.Seq)
				mu.Unlock()
			case errors.Is(err, store.ErrConflict):
				return // finished
			default:
				errs[i] = err
				return
			}
		}
	})
	for _, err := range errs {
		must(t, err)
	}
	list, err := s.ListAgentEvents(ctx, "ags_1", 0, 0)
	must(t, err)
	got := agentSeqs(list)
	if len(got) != len(accepted) {
		t.Fatalf("%d events stored, %d accepted", len(got), len(accepted))
	}
	for i := 1; i < len(got); i++ {
		if got[i] <= got[i-1] {
			t.Fatalf("listed Seq %d after %d", got[i], got[i-1])
		}
	}
	a, err := s.GetAgentSession(ctx, "ags_1")
	must(t, err)
	if a.Status != store.AgentSucceeded || a.Totals.Events != int64(len(got)) {
		t.Fatalf("session %s with %d events, want succeeded with %d", a.Status, a.Totals.Events, len(got))
	}
}

func testAgentCopies(t *testing.T, s store.Store) {
	ctx := t.Context()
	must(t, s.CreateAgentSession(ctx, newAgentSession("ags_1", 0)))
	in := store.AgentEvent{SessionID: "ags_1", Time: at(0), Type: "tool_call", Input: []byte(`{"a":1}`), DurationMillis: ptr(int64(5))}
	out, err := s.AppendAgentEvent(ctx, in)
	must(t, err)
	in.Input[2], *in.DurationMillis = 'X', 99
	out.Input[0], *out.DurationMillis = '[', 98
	list, err := s.ListAgentEvents(ctx, "ags_1", 0, 0)
	must(t, err)
	if string(canonJSON(t, list[0].Input)) != `{"a":1}` || *list[0].DurationMillis != 5 {
		t.Fatalf("stored event changed through a caller's slice or pointer: %s %d", list[0].Input, *list[0].DurationMillis)
	}
}

// testAgentText stores strings with NUL and invalid UTF-8, which PostgreSQL
// rejects in text and jsonb: every store keeps them cleaned the same way, and
// a lookup by an id that is not clean finds nothing.
func testAgentText(t *testing.T, s store.Store) {
	ctx := t.Context()
	a := newAgentSession("ags_1", 0)
	a.Title, a.Kind, a.Model, a.PlanRef = "plan\x00 gke\xff", "plan\x00", "claude\x00\xfe\xfftest", "pln\x001"
	must(t, s.CreateAgentSession(ctx, a))
	want := newAgentSession("ags_1", 0)
	want.Title, want.Kind, want.Model, want.PlanRef = "plan gke\uFFFD", "plan", "claude\uFFFDtest", "pln1"
	got, err := s.GetAgentSession(ctx, "ags_1")
	must(t, err)
	eqAgentSession(t, got, want)

	e := store.AgentEvent{SessionID: "ags_1", Time: at(1), Type: "tool\x00_call", Tool: "list\x00_envs\xff", Status: "o\x00k",
		Input:  []byte(`{"q":"a\u0000b","k\u0000":"\ud800","raw":"x` + "\xff" + `y","n":12345678901234567890}`),
		Output: []byte(`["\u0000"]`)}
	stored, err := s.AppendAgentEvent(ctx, e)
	must(t, err)
	wantEv := store.AgentEvent{Seq: stored.Seq, SessionID: "ags_1", Time: at(1), Type: "tool_call", Tool: "list_envs\uFFFD", Status: "ok",
		Input: []byte(`{"q":"ab","k":"\ufffd","raw":"x\ufffdy","n":12345678901234567890}`), Output: []byte(`[""]`)}
	eqAgentEvent(t, stored, wantEv)
	list, err := s.ListAgentEvents(ctx, "ags_1", 0, 0)
	must(t, err)
	if len(list) != 1 {
		t.Fatalf("%d events, want 1", len(list))
	}
	eqAgentEvent(t, list[0], wantEv)

	fin, err := s.FinishAgentSession(ctx, "ags_1", store.AgentFailed, "gave up\x00 \xff", at(2))
	must(t, err)
	if fin.Summary != "gave up \uFFFD" || fin.Status != store.AgentFailed {
		t.Fatalf("finished %q %q", fin.Status, fin.Summary)
	}

	_, err = s.GetAgentSession(ctx, "ags\x00_1")
	wantErr(t, err, store.ErrNotFound)
	_, err = s.FinishAgentSession(ctx, "ags_1\xff", store.AgentFailed, "", at(3))
	wantErr(t, err, store.ErrNotFound)
	_, err = s.AppendAgentEvent(ctx, store.AgentEvent{SessionID: "ags_1\x00", Time: at(3), Type: "message"})
	wantErr(t, err, store.ErrNotFound)
	list, err = s.ListAgentEvents(ctx, "ags_1\x00", 0, 0)
	must(t, err)
	sessions, err := s.ListAgentSessions(ctx, store.AgentSessionFilter{PlanRef: "pln\x001"})
	must(t, err)
	if len(list) != 0 || sessions == nil || len(sessions) != 0 {
		t.Fatalf("lookups by unclean ids: %d events, sessions %v", len(list), sessions)
	}
}

// testAgentIdle lists running sessions whose last event, or creation when
// they have none, is older than a time.
func testAgentIdle(t *testing.T, s store.Store) {
	ctx := t.Context()
	for _, id := range []string{"ags_quiet", "ags_busy", "ags_done"} {
		must(t, s.CreateAgentSession(ctx, newAgentSession(id, 0)))
	}
	must(t, s.CreateAgentSession(ctx, newAgentSession("ags_new", 10)))
	_, err := s.AppendAgentEvent(ctx, store.AgentEvent{SessionID: "ags_busy", Time: at(10), Type: "message"})
	must(t, err)
	_, err = s.AppendAgentEvent(ctx, store.AgentEvent{SessionID: "ags_busy", Time: at(2), Type: "message"})
	must(t, err)
	_, err = s.FinishAgentSession(ctx, "ags_done", store.AgentSucceeded, "", at(1))
	must(t, err)

	for _, c := range []struct {
		before int
		want   []string
	}{
		{0, []string{}},
		{5, []string{"ags_quiet"}},
		{10, []string{"ags_quiet"}}, // an event or a creation at the time itself is activity
		{11, []string{"ags_busy", "ags_quiet", "ags_new"}},
	} {
		got, err := s.IdleAgentSessions(ctx, at(c.before))
		must(t, err)
		eqList(t, fmt.Sprintf("IdleAgentSessions(at(%d))", c.before), sessionIDs(got), c.want)
	}
	got, err := s.IdleAgentSessions(ctx, at(11))
	must(t, err)
	if got[0].Totals.Events != 2 {
		t.Fatalf("idle session totals %+v", got[0].Totals)
	}
}
