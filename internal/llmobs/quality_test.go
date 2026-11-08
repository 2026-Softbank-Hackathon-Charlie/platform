package llmobs

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/llmquality"
)

func qualityFixture() (llmquality.Policy, []map[string]any) {
	p := llmquality.Policy{App: "shop", ProjectID: "project-shop", SourceBackend: "gcp", TargetBackend: "laptop", SourceModel: "cloud-sha", TargetModel: "local-sha", DatasetRevision: "dataset-sha", PromptRevision: "prompt-sha", RetrievalRevision: "none", EvaluatorRevision: "judge-sha", JudgeModel: "independent-judge", MaxAgeSeconds: 600, MinSoakSeconds: 30, Metrics: []llmquality.Metric{{Name: "task-success", ConfigID: "task-v1", Minimum: .9, MaxRegression: .05}, {Name: "safety", ConfigID: "safety-v1", Minimum: 1, HardFailure: true}}}
	var rows []map[string]any
	for i := range 20 {
		item := fmt.Sprintf("item-%d", i)
		p.Items = append(p.Items, item)
		for _, backend := range []string{"gcp", "laptop"} {
			for _, m := range p.Metrics {
				model := p.SourceModel
				if backend == "laptop" {
					model = p.TargetModel
				}
				rows = append(rows, map[string]any{"id": fmt.Sprintf("score-%s-%s-%s", item, backend, m.Name), "projectId": p.ProjectID, "name": m.Name, "configId": m.ConfigID, "source": "EVAL", "dataType": "NUMERIC", "value": 1, "timestamp": to.Add(-time.Minute), "subject": map[string]string{"kind": "observation", "id": item + "-" + backend, "traceId": "trace-1"}, "metadata": map[string]string{"app": "shop", "backend": backend, "modelRevision": model, "datasetRevision": p.DatasetRevision, "promptRevision": p.PromptRevision, "retrievalRevision": p.RetrievalRevision, "evaluatorRevision": p.EvaluatorRevision, "judgeModel": p.JudgeModel, "itemId": item, "evaluationBatch": "batch-1"}})
			}
		}
	}
	return p, rows
}

func TestQualityReadsRealScoresNotGenerationCounts(t *testing.T) {
	p, rows := qualityFixture()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Error("quality adapter attempted mutation")
		}
		if healthAndProject(w, r, "4.75.0") {
			return
		}
		if r.URL.Path != "/api/public/v3/scores" || r.URL.Query().Get("fields") != "details,subject" {
			t.Errorf("unexpected score path: %s", r.URL.Path)
			w.WriteHeader(404)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"data": rows, "meta": map[string]any{"cursor": nil}})
	}))
	defer server.Close()
	if got := clientFor(t, server.URL).Quality(context.Background(), p, to, time.Time{}); got.Status != "pass" {
		t.Fatalf("scoped scores: %+v", got)
	}
	// Scope is rechecked locally even if upstream ignores every query filter.
	rows[0]["projectId"] = "other-project"
	if got := clientFor(t, server.URL).Quality(context.Background(), p, to, time.Time{}); got.Status != "unknown" {
		t.Fatalf("cross-project scores passed: %+v", got)
	}
}

func TestQualityUnavailableWrongBindingAndPaginationCloseGate(t *testing.T) {
	for _, mode := range []string{"unsupported", "wrong-app", "missing-value", "future", "cursor-loop", "aggregate-only"} {
		t.Run(mode, func(t *testing.T) {
			p, rows := qualityFixture()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if healthAndProject(w, r, "4.75.0") {
					return
				}
				switch mode {
				case "unsupported":
					w.WriteHeader(404)
					return
				case "wrong-app":
					rows[0]["metadata"].(map[string]string)["app"] = "other"
				case "missing-value":
					delete(rows[0], "value")
				case "future":
					rows[0]["timestamp"] = to.Add(time.Hour)
				case "aggregate-only":
					rows[0]["subject"] = map[string]string{"kind": "experiment", "id": "aggregate"}
				}
				var cursor any
				if mode == "cursor-loop" {
					cursor = "same"
				}
				json.NewEncoder(w).Encode(map[string]any{"data": rows, "meta": map[string]any{"cursor": cursor}})
			}))
			defer server.Close()
			if got := clientFor(t, server.URL).Quality(context.Background(), p, to, time.Time{}); got.Status != "unknown" {
				t.Fatalf("unsafe verdict: %+v", got)
			}
		})
	}
}
