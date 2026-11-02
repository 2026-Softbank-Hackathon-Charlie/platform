package agentlog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/auth"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store/storetest"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

var (
	planner  = auth.Principal{Name: "planner", Role: apiv1.RoleAgent}
	other    = auth.Principal{Name: "other-agent", Role: apiv1.RoleAgent}
	operator = auth.Principal{Name: "seohyun", Role: apiv1.RoleOperator}
)

func ptr[T any](v T) *T { return &v }

func newSession(t *testing.T, s *Service) apiv1.AgentSession {
	t.Helper()
	a, err := s.Create(t.Context(), planner.Name, apiv1.CreateAgentSessionRequest{Kind: apiv1.AgentSessionKindPlan, Title: " plan gke -> eks ", PlanRef: ptr("pln_1")})
	if err != nil {
		t.Fatal(err)
	}
	if a.Status != apiv1.AgentSessionStatusRunning || a.Actor != "planner" || a.Title != "plan gke -> eks" || !strings.HasPrefix(a.Id, "ags_") {
		t.Fatalf("session %+v", a)
	}
	return a
}

func TestAppendRedactsAndLimits(t *testing.T) {
	s := New(storetest.NewMemory())
	a := newSession(t, s)
	ctx := t.Context()

	ev, err := s.Append(ctx, planner, a.Id, EventInput{
		Type: apiv1.AgentEventTypeToolCall, Tool: ptr("submit_plan"),
		Input:          json.RawMessage(`{"plan":{"source":{"dsn":"postgres://app:${SRC_PGPASSWORD}@db/app"},"target":{"dsn":"postgres://app:pw-literal@db2/app"},"gateway":{"token":"gw-secret"}},"bytes":12345678901234567890}`),
		DurationMillis: ptr(int64(120)), Usage: &apiv1.TokenUsage{InputTokens: 900, OutputTokens: 80},
	})
	if err != nil {
		t.Fatal(err)
	}
	in, _ := json.Marshal(ev.Input)
	if strings.Contains(string(in), "gw-secret") || strings.Contains(string(in), "pw-literal") || !strings.Contains(string(in), `"token":"[redacted]"`) ||
		!strings.Contains(string(in), "${SRC_PGPASSWORD}") || !strings.Contains(string(in), "12345678901234567890") {
		t.Fatalf("stored input %s", in)
	}
	if ev.Seq == 0 || ev.Usage == nil || ev.Usage.InputTokens != 900 || *ev.DurationMillis != 120 {
		t.Fatalf("event %+v", ev)
	}

	big := `"` + strings.Repeat("x", MaxPayloadBytes) + `"`
	if _, err := s.Append(ctx, planner, a.Id, EventInput{Type: apiv1.AgentEventTypeToolResult, Output: json.RawMessage(big)}); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("64 KiB output: %v", err)
	}
	if _, err := s.Append(ctx, planner, a.Id, EventInput{Type: "thinking"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown type: %v", err)
	}
	if _, err := s.Append(ctx, planner, a.Id, EventInput{Type: apiv1.AgentEventTypeMessage, DurationMillis: ptr(int64(-1))}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("negative duration: %v", err)
	}
	if ev, err := s.Append(ctx, planner, a.Id, EventInput{Type: apiv1.AgentEventTypeMessage, Output: json.RawMessage(`null`)}); err != nil || ev.Output != nil {
		t.Fatalf("null output: %+v %v", ev, err)
	}

	got, err := s.Get(ctx, a.Id)
	if err != nil || got.Totals.Events != 2 || got.Totals.ToolCalls != 1 || got.Totals.InputTokens != 900 || got.Totals.DurationMillis != 120 {
		t.Fatalf("totals %+v %v", got.Totals, err)
	}
}

func TestOnlyActorOrOperatorWrites(t *testing.T) {
	s := New(storetest.NewMemory())
	a := newSession(t, s)
	ctx := t.Context()
	msg := EventInput{Type: apiv1.AgentEventTypeMessage}
	if _, err := s.Append(ctx, other, a.Id, msg); !errors.Is(err, ErrForbidden) {
		t.Fatalf("other agent: %v", err)
	}
	if _, err := s.Finish(ctx, other, a.Id, apiv1.FinishAgentSessionRequest{Status: apiv1.AgentSessionStatusFailed}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("other agent finish: %v", err)
	}
	if _, err := s.Append(ctx, operator, a.Id, msg); err != nil {
		t.Fatalf("operator: %v", err)
	}
	if _, err := s.Append(ctx, planner, "ags_missing", msg); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing session: %v", err)
	}
}

func TestFinishClosesSubscribersAndRefusesEvents(t *testing.T) {
	s := New(storetest.NewMemory())
	a := newSession(t, s)
	ctx := t.Context()

	live, cancel, err := s.Subscribe(ctx, a.Id)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	ev, err := s.Append(ctx, planner, a.Id, EventInput{Type: apiv1.AgentEventTypeDecision, Output: json.RawMessage(`"submit"`)})
	if err != nil {
		t.Fatal(err)
	}
	if got := <-live; got.Seq != ev.Seq {
		t.Fatalf("live event %d, want %d", got.Seq, ev.Seq)
	}
	if _, err := s.Finish(ctx, planner, a.Id, apiv1.FinishAgentSessionRequest{Status: apiv1.AgentSessionStatusRunning}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("finish as running: %v", err)
	}
	fin, err := s.Finish(ctx, planner, a.Id, apiv1.FinishAgentSessionRequest{Status: apiv1.AgentSessionStatusSucceeded, Summary: ptr("plan pln_1 submitted")})
	if err != nil || fin.Status != apiv1.AgentSessionStatusSucceeded || fin.EndedAt == nil || *fin.Summary != "plan pln_1 submitted" || fin.Totals.Events != 1 {
		t.Fatalf("finish: %+v %v", fin, err)
	}
	if _, open := <-live; open {
		t.Fatal("subscriber not closed by finish")
	}
	if _, err := s.Append(ctx, planner, a.Id, EventInput{Type: apiv1.AgentEventTypeMessage}); !errors.Is(err, ErrFinished) {
		t.Fatalf("event after finish: %v", err)
	}
	if _, err := s.Finish(ctx, operator, a.Id, apiv1.FinishAgentSessionRequest{Status: apiv1.AgentSessionStatusFailed}); !errors.Is(err, ErrFinished) {
		t.Fatalf("second finish: %v", err)
	}
	done, _, err := s.Subscribe(ctx, a.Id)
	if err != nil {
		t.Fatal(err)
	}
	if _, open := <-done; open {
		t.Fatal("subscription to a finished session is open")
	}
	if _, _, err := s.Subscribe(ctx, "ags_missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("subscribe missing: %v", err)
	}
}

func TestSlowSubscriberIsDropped(t *testing.T) {
	s := New(storetest.NewMemory())
	a := newSession(t, s)
	live, cancel, err := s.Subscribe(t.Context(), a.Id)
	if err != nil {
		t.Fatal(err)
	}
	for range SubscriberBuffer + 1 {
		if _, err := s.Append(t.Context(), planner, a.Id, EventInput{Type: apiv1.AgentEventTypeMessage}); err != nil {
			t.Fatal(err)
		}
	}
	n := 0
	for range live {
		n++
	}
	if n != SubscriberBuffer {
		t.Fatalf("received %d events before the drop, want %d", n, SubscriberBuffer)
	}
	cancel() // after a drop, cancel is a no-op
}

func TestCreateValidation(t *testing.T) {
	s := New(storetest.NewMemory())
	for _, req := range []apiv1.CreateAgentSessionRequest{
		{Kind: "chat", Title: "x"},
		{Kind: apiv1.AgentSessionKindPlan, Title: "  "},
		{Kind: apiv1.AgentSessionKindPlan, Title: strings.Repeat("t", 201)},
	} {
		if _, err := s.Create(t.Context(), "planner", req); !errors.Is(err, ErrInvalid) {
			t.Errorf("%+v: %v", req, err)
		}
	}
}

func TestAppendCleansText(t *testing.T) {
	s := New(storetest.NewMemory())
	ctx := t.Context()
	a, err := s.Create(ctx, planner.Name, apiv1.CreateAgentSessionRequest{Kind: apiv1.AgentSessionKindPlan, Title: "plan\x00 gke\xff", Model: ptr("m\x00")})
	if err != nil || a.Title != "plan gke\uFFFD" || *a.Model != "m" {
		t.Fatalf("create: %+v %v", a, err)
	}
	if got, _ := s.Get(ctx, a.Id); got.Title != a.Title {
		t.Fatalf("stored title %q, answered %q", got.Title, a.Title)
	}
	if _, err := s.Create(ctx, planner.Name, apiv1.CreateAgentSessionRequest{Kind: apiv1.AgentSessionKindPlan, Title: "\x00"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("title of only NUL: %v", err)
	}
	ev, err := s.Append(ctx, planner, a.Id, EventInput{Type: apiv1.AgentEventTypeToolResult, Tool: ptr("sh\x00"),
		Output: json.RawMessage(`{"stdout":"ok\u0000"}`)})
	if err != nil || *ev.Tool != "sh" || string(mustJSON(t, ev.Output)) != `{"stdout":"ok"}` {
		t.Fatalf("append: %+v %v", ev, err)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestSessionFull(t *testing.T) {
	s := New(storetest.NewMemory())
	s.maxEvents = 3
	a := newSession(t, s)
	ctx := t.Context()
	for range 3 {
		if _, err := s.Append(ctx, planner, a.Id, EventInput{Type: apiv1.AgentEventTypeMessage}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Append(ctx, planner, a.Id, EventInput{Type: apiv1.AgentEventTypeMessage}); !errors.Is(err, ErrSessionFull) {
		t.Fatalf("fourth event: %v", err)
	}
	if fin, err := s.Finish(ctx, planner, a.Id, apiv1.FinishAgentSessionRequest{Status: apiv1.AgentSessionStatusFailed}); err != nil || fin.Totals.Events != 3 {
		t.Fatalf("finish a full session: %+v %v", fin, err)
	}
	if _, err := s.Append(ctx, planner, a.Id, EventInput{Type: apiv1.AgentEventTypeMessage}); !errors.Is(err, ErrFinished) {
		t.Fatalf("event after finish: %v", err)
	}
}

func TestEventsPages(t *testing.T) {
	s := New(storetest.NewMemory())
	a := newSession(t, s)
	ctx := t.Context()
	var seqs []int64
	for range 5 {
		ev, err := s.Append(ctx, planner, a.Id, EventInput{Type: apiv1.AgentEventTypeMessage})
		if err != nil {
			t.Fatal(err)
		}
		seqs = append(seqs, ev.Seq)
	}
	var got []int64
	var after int64
	for pages := 0; ; pages++ {
		page, err := s.Events(ctx, a.Id, after, 2)
		if err != nil || len(page.Events) > 2 || pages > 3 {
			t.Fatalf("page %d: %+v %v", pages, page, err)
		}
		for _, e := range page.Events {
			got = append(got, e.Seq)
		}
		if page.NextAfter == nil {
			if pages != 2 {
				t.Fatalf("%d pages of 2 for 5 events, want 3", pages+1)
			}
			break
		}
		after = *page.NextAfter
	}
	if fmt.Sprint(got) != fmt.Sprint(seqs) {
		t.Fatalf("paged %v, want %v", got, seqs)
	}
	if page, err := s.Events(ctx, a.Id, 0, 5); err != nil || len(page.Events) != 5 || page.NextAfter != nil {
		t.Fatalf("exact page: %+v %v", page, err)
	}
	if page, err := s.Events(ctx, a.Id, 0, 0); err != nil || len(page.Events) != 5 {
		t.Fatalf("default page: %+v %v", page, err)
	}
	if _, err := s.Events(ctx, "ags_missing", 0, 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing session: %v", err)
	}
}

func TestSweepClosesIdleSessions(t *testing.T) {
	s := New(storetest.NewMemory())
	now := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	ctx := t.Context()
	quiet, busy := newSession(t, s), newSession(t, s)
	live, cancel, err := s.Subscribe(ctx, quiet.Id)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()

	now = now.Add(20 * time.Minute)
	if _, err := s.Append(ctx, planner, busy.Id, EventInput{Type: apiv1.AgentEventTypeMessage}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(11 * time.Minute) // quiet: 31m since creation; busy: 11m since its event
	closed, err := s.Sweep(ctx, 30*time.Minute)
	if err != nil || fmt.Sprint(closed) != fmt.Sprint([]string{quiet.Id}) {
		t.Fatalf("sweep closed %v, %v; want %s", closed, err, quiet.Id)
	}
	if _, open := <-live; open {
		t.Fatal("the idle session's subscriber is still open")
	}
	got, _ := s.Get(ctx, quiet.Id)
	if got.Status != apiv1.AgentSessionStatusCancelled || deref(got.Summary) != "no activity for 30m; closed by the platform" || got.EndedAt == nil {
		t.Fatalf("idle session: %+v", got)
	}
	if got, _ := s.Get(ctx, busy.Id); got.Status != apiv1.AgentSessionStatusRunning {
		t.Fatalf("active session was closed: %+v", got)
	}

	now = now.Add(20 * time.Minute)
	if closed, err := s.Sweep(ctx, 30*time.Minute); err != nil || fmt.Sprint(closed) != fmt.Sprint([]string{busy.Id}) {
		t.Fatalf("second sweep closed %v, %v", closed, err)
	}
	if closed, err := s.Sweep(ctx, 30*time.Minute); err != nil || len(closed) != 0 {
		t.Fatalf("third sweep closed %v, %v", closed, err)
	}
}

func TestRunSweeperWithShortIdle(t *testing.T) {
	s := New(storetest.NewMemory())
	a := newSession(t, s)
	live, cancel, err := s.Subscribe(t.Context(), a.Id)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	ctx, stop := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		s.RunSweeper(ctx, 50*time.Millisecond, slog.New(slog.DiscardHandler))
		close(done)
	}()
	select {
	case _, open := <-live:
		if open {
			t.Fatal("unexpected live event")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the sweeper did not close the idle session")
	}
	got, _ := s.Get(t.Context(), a.Id)
	if got.Status != apiv1.AgentSessionStatusCancelled || deref(got.Summary) != "no activity for 50ms; closed by the platform" {
		t.Fatalf("session: %+v", got)
	}
	stop()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunSweeper did not return after its context ended")
	}
}

func TestShortDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{
		30 * time.Minute: "30m", time.Hour: "1h", 90 * time.Minute: "1h30m", 45 * time.Second: "45s",
		50 * time.Millisecond: "50ms", 2*time.Minute + 5*time.Second: "2m5s",
	} {
		if got := shortDuration(d); got != want {
			t.Errorf("shortDuration(%s) = %q, want %q", d, got, want)
		}
	}
}
