package llmobs

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/llmquality"
)

// Quality reads score values only. It never fetches generation input/output,
// submits evaluations, invokes a judge or accepts an API-provided URL/project.
// /v3/scores is the documented cursor API; unsupported servers close the gate.
func (c *Client) Quality(ctx context.Context, p llmquality.Policy, now, notBefore time.Time) llmquality.Verdict {
	unknown := func(reason string) llmquality.Verdict { return llmquality.Unknown(now, reason) }
	if c == nil || c.configurationError {
		return unknown("configuration_error")
	}
	if p.Validate() != nil {
		return unknown("invalid_policy")
	}
	b, ok := c.config.Apps[p.App]
	if !ok || b.ProjectID != p.ProjectID {
		return unknown("project_scope_unmapped")
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	key := c.keys[p.App]
	var projects struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if reason := c.get(ctx, "/api/public/projects", nil, key, &projects); reason != "" {
		return unknown(reason)
	}
	if len(projects.Data) != 1 || projects.Data[0].ID != p.ProjectID {
		return unknown("project_scope_mismatch")
	}
	names, configs := []string{}, []string{}
	for _, m := range p.Metrics {
		names = append(names, m.Name)
		configs = append(configs, m.ConfigID)
	}
	source := p.ScoreSource
	if source == "" {
		source = "EVAL"
	}
	from := now.Add(-time.Duration(p.MaxAgeSeconds) * time.Second)
	if notBefore.After(from) {
		from = notBefore
	}
	q := url.Values{"fields": {"details,subject"}, "limit": {"100"}, "name": {strings.Join(names, ",")}, "configId": {strings.Join(configs, ",")}, "source": {source}, "fromTimestamp": {from.UTC().Format(time.RFC3339Nano)}, "toTimestamp": {now.UTC().Format(time.RFC3339Nano)}}
	var rows []llmquality.Score
	cursors := map[string]bool{}
	for page := 0; page < 10; page++ {
		var response struct {
			Data []struct {
				ID        string                     `json:"id"`
				ProjectID string                     `json:"projectId"`
				Name      string                     `json:"name"`
				ConfigID  string                     `json:"configId"`
				Source    string                     `json:"source"`
				DataType  string                     `json:"dataType"`
				Value     json.RawMessage            `json:"value"`
				Timestamp time.Time                  `json:"timestamp"`
				Metadata  map[string]json.RawMessage `json:"metadata"`
				Subject   struct {
					Kind    string `json:"kind"`
					ID      string `json:"id"`
					TraceID string `json:"traceId"`
				} `json:"subject"`
			} `json:"data"`
			Meta struct {
				Cursor *string `json:"cursor"`
			} `json:"meta"`
		}
		if reason := c.get(ctx, "/api/public/v3/scores", q, key, &response); reason != "" {
			return unknown(reason)
		}
		if response.Data == nil || len(response.Data) > 100 {
			return unknown("scores_response_invalid")
		}
		for _, r := range response.Data {
			if r.Source != source || r.ProjectID != p.ProjectID || (r.Subject.Kind != "observation" && r.Subject.Kind != "trace") || !identifier.MatchString(r.Subject.ID) || (r.Subject.Kind == "observation" && !identifier.MatchString(r.Subject.TraceID)) {
				return unknown("score_provenance_mismatch")
			}
			var value *float64
			if r.DataType == "BOOLEAN" {
				var b *bool
				if json.Unmarshal(r.Value, &b) != nil || b == nil {
					return unknown("score_value_invalid")
				}
				n := 0.0
				if *b {
					n = 1
				}
				value = &n
			} else if r.DataType != "NUMERIC" || json.Unmarshal(r.Value, &value) != nil || value == nil {
				return unknown("score_value_invalid")
			}
			get := func(name string) string { var v string; _ = json.Unmarshal(r.Metadata[name], &v); return v }
			rows = append(rows, llmquality.Score{SubjectID: r.Subject.ID, ID: r.ID, ProjectID: r.ProjectID, Name: r.Name, ConfigID: r.ConfigID, Value: *value, Timestamp: r.Timestamp, App: get(b.MetadataKey), Backend: get("backend"), ModelRevision: get("modelRevision"), DatasetRevision: get("datasetRevision"), PromptRevision: get("promptRevision"), RetrievalRevision: get("retrievalRevision"), EvaluatorRevision: get("evaluatorRevision"), JudgeModel: get("judgeModel"), Item: get("itemId"), Batch: get("evaluationBatch")})
		}
		if response.Meta.Cursor == nil || *response.Meta.Cursor == "" {
			return evaluateLatestBatch(p, rows, now, notBefore)
		}
		cursor := *response.Meta.Cursor
		if len(cursor) > 2048 || cursors[cursor] {
			return unknown("scores_pagination_invalid")
		}
		cursors[cursor] = true
		q.Set("cursor", cursor)
	}
	return unknown("scores_sample_truncated")
}

// Never fall back to an older passing batch if the latest batch is incomplete
// or regressed. Older completed experiments may coexist in the age window.
func evaluateLatestBatch(p llmquality.Policy, rows []llmquality.Score, now, notBefore time.Time) llmquality.Verdict {
	var newest time.Time
	var batch string
	ambiguous := false
	for _, row := range rows {
		if row.Timestamp.After(newest) {
			newest, batch = row.Timestamp, row.Batch
			ambiguous = false
		} else if row.Timestamp.Equal(newest) && row.Batch != batch {
			ambiguous = true
		}
	}
	if ambiguous {
		return llmquality.Unknown(now, "latest_batch_ambiguous")
	}
	latest := []llmquality.Score{}
	for _, row := range rows {
		if row.Batch == batch {
			latest = append(latest, row)
		}
	}
	return llmquality.Evaluate(p, latest, now, notBefore)
}
