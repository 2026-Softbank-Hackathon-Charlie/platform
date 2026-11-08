package api

import (
	"context"
	"testing"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/llmobs"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/runs"
)

type fakeLLM struct {
	app      string
	from, to time.Time
	observed bool
}

func (f *fakeLLM) Report(_ context.Context, app string, from, to time.Time) llmobs.Report {
	f.app, f.from, f.to = app, from, to
	r := llmobs.Empty(app, from, to, "unobserved", "no_scoped_generations")
	if f.observed {
		r.Source.Status = "observed"
		r.Source.Reason = "bounded_generation_sample"
		r.Observations = []llmobs.Observation{{ID: "generation-1", TraceID: "trace-1", StartTime: from.Add(time.Second)}}
		r.Sample.Returned = 1
		r.Summary = &llmobs.Summary{Generations: 1, Traces: 1}
	}
	return r
}
func withLLM(f *fakeLLM) option {
	return func(_ *harness, _ *runs.Config, d *Deps) { d.LLMObservability = f }
}

func TestLLMObservabilityAuthScopeAndContract(t *testing.T) {
	f := &fakeLLM{}
	h := newHarness(t, withApps(""), withLLM(f))
	if code, _ := h.do("GET", "/v1/apps/moimroom/llm-observability", "", nil); code != 401 {
		t.Fatalf("anonymous %d", code)
	}
	if code, _ := h.do("GET", "/v1/apps/unknown/llm-observability", viewerToken, nil); code != 404 {
		t.Fatalf("unknown app %d", code)
	}
	for _, query := range []string{"minutes=0", "minutes=1441", "project=other", "baseUrl=https://outside.test", "filter=private"} {
		if code, _ := h.do("GET", "/v1/apps/moimroom/llm-observability?"+query, viewerToken, nil); code != 400 {
			t.Errorf("query %s got %d", query, code)
		}
	}
	code, b := h.do("GET", "/v1/apps/moimroom/llm-observability?minutes=5", viewerToken, nil)
	if code != 200 || f.app != "moimroom" || f.to.Sub(f.from) != 5*time.Minute {
		t.Fatalf("scoped read: %d %+v", code, f)
	}
	h.validateSchema("LLMObservability", b)
	f.observed = true
	code, b = h.do("GET", "/v1/apps/moimroom/llm-observability?minutes=5", viewerToken, nil)
	if code != 200 {
		t.Fatalf("observed status %d", code)
	}
	h.validateSchema("LLMObservability", b)
}
func TestLLMObservabilityMissingConfigurationIsNotZero(t *testing.T) {
	h := newHarness(t, withApps(""))
	code, b := h.do("GET", "/v1/apps/moimroom/llm-observability", viewerToken, nil)
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	out := decode[llmobs.Report](t, b)
	if out.Source.Status != "not_configured" || out.Summary != nil || len(out.Observations) != 0 {
		t.Fatalf("missing source became data: %s", b)
	}
	h.validateSchema("LLMObservability", b)
}
