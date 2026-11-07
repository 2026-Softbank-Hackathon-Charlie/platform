package llmquality

import (
	"fmt"
	"testing"
	"time"
)

func fixture() (Policy, []Score, time.Time) {
	now := time.Date(2026, 11, 8, 1, 0, 0, 0, time.UTC)
	p := Policy{App: "moimroom-ai", ProjectID: "project", DatasetRevision: "dataset-sha", PromptRevision: "prompt-sha", RetrievalRevision: "retrieval-sha", EvaluatorRevision: "judge-sha", JudgeModel: "independent-judge", SourceModel: "qwen-cloud-sha", TargetModel: "qwen-local-sha", SourceBackend: "gcp", TargetBackend: "laptop", MaxAgeSeconds: 600, MinSoakSeconds: 30, Metrics: []Metric{{Name: "task-success", ConfigID: "task-v1", Minimum: .8, MaxRegression: .05}, {Name: "safety", ConfigID: "safety-v1", Minimum: 1, MaxRegression: 0, HardFailure: true}}}
	var rows []Score
	for i := range 20 {
		item := fmt.Sprintf("item-%02d", i)
		p.Items = append(p.Items, item)
		for _, backend := range []string{p.SourceBackend, p.TargetBackend} {
			for _, metric := range p.Metrics {
				model := p.SourceModel
				if backend == p.TargetBackend {
					model = p.TargetModel
				}
				rows = append(rows, Score{SubjectID: item + "-" + backend, ID: fmt.Sprintf("%s-%s-%s", item, backend, metric.Name), ProjectID: p.ProjectID, App: p.App, Backend: backend, ModelRevision: model, DatasetRevision: p.DatasetRevision, PromptRevision: p.PromptRevision, RetrievalRevision: p.RetrievalRevision, EvaluatorRevision: p.EvaluatorRevision, JudgeModel: p.JudgeModel, Item: item, Batch: "batch-1", Name: metric.Name, ConfigID: metric.ConfigID, Value: 1, Timestamp: now.Add(-time.Minute)})
			}
		}
	}
	return p, rows, now
}

func TestQualityRequiresCompleteBoundEvidence(t *testing.T) {
	p, rows, now := fixture()
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := Evaluate(p, rows, now, time.Time{}); got.Status != "pass" || got.Pairs != 20 || got.ValidUntil.IsZero() {
		t.Fatalf("valid scores: %+v", got)
	}
	tests := []struct {
		name   string
		change func(*Policy, *[]Score)
	}{
		{"missing", func(_ *Policy, r *[]Score) { *r = (*r)[1:] }},
		{"duplicate", func(_ *Policy, r *[]Score) { *r = append(*r, (*r)[0]) }},
		{"wrong-app", func(_ *Policy, r *[]Score) { (*r)[0].App = "other" }},
		{"wrong-project", func(_ *Policy, r *[]Score) { (*r)[0].ProjectID = "other" }},
		{"wrong-model", func(_ *Policy, r *[]Score) { (*r)[0].ModelRevision = "other" }},
		{"wrong-dataset", func(_ *Policy, r *[]Score) { (*r)[0].DatasetRevision = "other" }},
		{"wrong-prompt", func(_ *Policy, r *[]Score) { (*r)[0].PromptRevision = "other" }},
		{"wrong-retrieval", func(_ *Policy, r *[]Score) { (*r)[0].RetrievalRevision = "other" }},
		{"wrong-judge", func(_ *Policy, r *[]Score) { (*r)[0].EvaluatorRevision = "other" }},
		{"mixed-batch", func(_ *Policy, r *[]Score) { (*r)[0].Batch = "other" }},
		{"future", func(_ *Policy, r *[]Score) { (*r)[0].Timestamp = now.Add(time.Second) }},
		{"stale", func(_ *Policy, r *[]Score) { (*r)[0].Timestamp = now.Add(-time.Hour) }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p, r, n := fixture()
			tc.change(&p, &r)
			if got := Evaluate(p, r, n, time.Time{}); got.Status != "unknown" {
				t.Fatalf("unsafe verdict: %+v", got)
			}
		})
	}
	if got := Evaluate(p, rows, now, now.Add(-30*time.Second)); got.Status != "unknown" {
		t.Fatalf("old stage evidence: %+v", got)
	}
}

func TestQualityRegressionAndJudgeIndependence(t *testing.T) {
	p, rows, now := fixture()
	for i := range rows {
		if rows[i].Backend == p.TargetBackend && rows[i].Name == "task-success" {
			rows[i].Value = .7
		}
	}
	if got := Evaluate(p, rows, now, time.Time{}); got.Status != "regressed" {
		t.Fatalf("regression: %+v", got)
	}
	p.JudgeModel = p.TargetModel
	if p.Validate() == nil {
		t.Fatal("target cannot grade itself")
	}
}

func TestQualityUncertaintyDoesNotClaimProvenRegression(t *testing.T) {
	p, rows, now := fixture()
	for i := range rows {
		if rows[i].Backend == p.TargetBackend && rows[i].Name == "task-success" {
			rows[i].Value = 0
			break
		}
	}
	if got := Evaluate(p, rows, now, time.Time{}); got.Status != "unknown" || got.Reason != "quality_inconclusive" {
		t.Fatalf("uncertainty was called regression: %+v", got)
	}
	p, rows, now = fixture()
	for i := range rows {
		if rows[i].Backend == p.TargetBackend && rows[i].Name == "safety" {
			rows[i].Value = 0
			break
		}
	}
	if got := Evaluate(p, rows, now, time.Time{}); got.Status != "regressed" {
		t.Fatalf("hard safety failure accepted: %+v", got)
	}
}
