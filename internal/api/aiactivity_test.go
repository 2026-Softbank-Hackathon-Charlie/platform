package api

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/aiobs"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/runs"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

type fakeActivity struct {
	events []aiobs.Event
	err    error
	since  time.Time
}

func (f *fakeActivity) Events(_ context.Context, since time.Time, _ int) ([]aiobs.Event, error) {
	f.since = since
	return f.events, f.err
}

func withAIActivity(f AIActivityReader) option {
	return func(_ *harness, _ *runs.Config, d *Deps) { d.AIActivity = f }
}

func TestAIActivityNotConfigured(t *testing.T) {
	h := newHarness(t)
	if code, b := h.do("GET", "/v1/ai-activity", viewerToken, nil); code != 503 || errCode(t, b) != "observability_not_configured" {
		t.Fatalf("without ClickHouse: %d %s", code, b)
	}
}

func TestAIActivityAnswersParsedFieldsOnly(t *testing.T) {
	t0 := time.Date(2026, 11, 7, 7, 38, 15, 0, time.UTC)
	passed := true
	f := &fakeActivity{events: []aiobs.Event{
		{Time: t0, Kind: aiobs.KindModel, Model: "claude-sonnet-5-5", StopReason: "tool_use", ToolCalls: []string{"get_cnpg_cluster", "get_app"}, TokensIn: 5543, TokensOut: 203},
		{Time: t0.Add(time.Second), Kind: aiobs.KindToolCall, Tool: "get_cnpg_cluster"},
		{Time: t0.Add(2 * time.Second), Kind: aiobs.KindToolDone, Tool: "get_cnpg_cluster", Outcome: "ok"},
		{Time: t0.Add(time.Minute), Kind: aiobs.KindRehearsal, Outcome: "succeeded", Passed: &passed, Seconds: 102},
	}}
	h := newHarness(t, withAIActivity(f))
	if code, _ := h.do("GET", "/v1/ai-activity", "", nil); code != 401 {
		t.Fatalf("anonymous: %d", code)
	}
	code, b := h.do("GET", "/v1/ai-activity?minutes=30", viewerToken, nil)
	if code != 200 {
		t.Fatalf("viewer: %d %s", code, b)
	}
	out := decode[apiv1.AIActivity](t, b)
	if len(out.Events) != 4 || out.Events[0].Kind != "model" || out.Events[3].Passed == nil || !*out.Events[3].Passed {
		t.Fatalf("events: %s", b)
	}
	s := out.Summary
	if s.ModelCalls != 1 || s.TokensIn != 5543 || s.TokensOut != 203 || s.Tools["get_cnpg_cluster"] != 1 || s.Rehearsals != 1 || s.Model == nil || *s.Model != "claude-sonnet-5-5" {
		t.Fatalf("summary: %s", b)
	}
	if got := f.since; time.Since(got) < 29*time.Minute || time.Since(got) > 31*time.Minute {
		t.Fatalf("since = %v, want about 30 minutes ago", got)
	}
}

func TestAIActivityRejectsBadWindow(t *testing.T) {
	h := newHarness(t, withAIActivity(&fakeActivity{}))
	for _, q := range []string{"minutes=0", "minutes=1441", "minutes=abc", "minutes=-5"} {
		if code, b := h.do("GET", "/v1/ai-activity?"+q, viewerToken, nil); code != 400 || errCode(t, b) != "invalid_request" {
			t.Fatalf("%s: %d %s", q, code, b)
		}
	}
}

func TestAIActivityHidesClickHouseErrors(t *testing.T) {
	h := newHarness(t, withAIActivity(&fakeActivity{err: errors.New("aiobs: query: dial tcp 10.0.0.5:8123: password=hunter2")}))
	code, b := h.do("GET", "/v1/ai-activity", viewerToken, nil)
	if code != 502 || errCode(t, b) != "observability_unavailable" {
		t.Fatalf("%d %s", code, b)
	}
	if strings.Contains(string(b), "hunter2") || strings.Contains(string(b), "10.0.0.5") {
		t.Fatalf("the error leaked: %s", b)
	}
}
