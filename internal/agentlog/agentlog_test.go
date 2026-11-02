package agentlog

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

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

func TestRedact(t *testing.T) {
	in := map[string]any{
		"query":       "select 1",
		"DB_PASSWORD": "hunter2", "apiKey": "k", "api-key": "k", "Authorization": "Bearer x",
		"nested": []any{map[string]any{"client_secret": map[string]any{"value": "s"}, "ok": 1}},
		"auth":   map[string]any{"token": "t", "user": "u"},
	}
	got, _ := json.Marshal(Redact(in))
	want := `{"Authorization":"[redacted]","DB_PASSWORD":"[redacted]","api-key":"[redacted]","apiKey":"[redacted]",` +
		`"auth":{"token":"[redacted]","user":"u"},"nested":[{"client_secret":"[redacted]","ok":1}],"query":"select 1"}`
	if string(got) != want {
		t.Fatalf("redacted:\n%s\nwant:\n%s", got, want)
	}
	if in["DB_PASSWORD"] != "hunter2" {
		t.Fatal("Redact modified its input")
	}
}

func TestAppendRedactsAndLimits(t *testing.T) {
	s := New(storetest.NewMemory())
	a := newSession(t, s)
	ctx := t.Context()

	ev, err := s.Append(ctx, planner, a.Id, EventInput{
		Type: apiv1.AgentEventTypeToolCall, Tool: ptr("submit_plan"),
		Input:          json.RawMessage(`{"plan":{"source":{"dsn":"postgres://app:${SRC_PGPASSWORD}@db/app"},"gateway":{"token":"gw-secret"}},"bytes":12345678901234567890}`),
		DurationMillis: ptr(int64(120)), Usage: &apiv1.TokenUsage{InputTokens: 900, OutputTokens: 80},
	})
	if err != nil {
		t.Fatal(err)
	}
	in, _ := json.Marshal(ev.Input)
	if strings.Contains(string(in), "gw-secret") || !strings.Contains(string(in), `"token":"[redacted]"`) || !strings.Contains(string(in), "12345678901234567890") {
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
