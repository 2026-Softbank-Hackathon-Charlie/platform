package llmobs

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

type wireObservation struct {
	ID                  string                     `json:"id"`
	TraceID             string                     `json:"traceId"`
	ProjectID           string                     `json:"projectId"`
	Type                string                     `json:"type"`
	StartTime           time.Time                  `json:"startTime"`
	EndTime             *time.Time                 `json:"endTime"`
	Model               string                     `json:"model"`
	Level               *string                    `json:"level"`
	Metadata            map[string]json.RawMessage `json:"metadata"`
	UsageDetails        map[string]json.RawMessage `json:"usageDetails"`
	Usage               map[string]json.RawMessage `json:"usage"`
	CostDetails         map[string]json.RawMessage `json:"costDetails"`
	CalculatedTotalCost *float64                   `json:"calculatedTotalCost"`
}

func (c *Client) Report(ctx context.Context, app string, from, to time.Time) Report {
	if c.configurationError {
		return Empty(app, from, to, "unavailable", "configuration_error")
	}
	r := Empty(app, from, to, "scope_unmapped", "app_scope_unmapped")
	b, ok := c.config.Apps[app]
	if !ok {
		return r
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	key := c.keys[app]
	var health struct {
		Version string `json:"version"`
	}
	if reason := c.get(ctx, "/api/public/health", nil, key, &health); reason != "" {
		return unavailable(r, reason)
	}
	version := serverVersion.FindStringSubmatch(health.Version)
	if version == nil {
		r.Source.Status = "unsupported"
		r.Source.Reason = "server_version_unsupported"
		return r
	}
	r.Source.Version = strings.Join(version[1:4], ".")
	major, _ := strconv.Atoi(version[1])
	path := "/api/public/observations"
	if major == 4 {
		path = "/api/public/v2/observations"
	} else if major != 3 {
		r.Source.Status = "unsupported"
		r.Source.Reason = "server_version_unsupported"
		return r
	}
	var projects struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if reason := c.get(ctx, "/api/public/projects", nil, key, &projects); reason != "" {
		return unavailable(r, reason)
	}
	if len(projects.Data) != 1 || projects.Data[0].ID != b.ProjectID {
		return unavailable(r, "project_scope_mismatch")
	}
	q := url.Values{"type": {"GENERATION"}, "fromStartTime": {from.UTC().Format(time.RFC3339Nano)}, "toStartTime": {to.UTC().Format(time.RFC3339Nano)}, "limit": {strconv.Itoa(SampleLimit)}}
	if major == 4 {
		q.Set("fields", "core,basic,time,metadata,model,usage")
	} else {
		q.Set("page", "1")
	}
	// The documented filter overrides other query filters, so include every
	// restriction in it. Recheck scope/time/type locally even if the server ignores it.
	filter, _ := json.Marshal([]map[string]any{
		{"type": "string", "column": "type", "operator": "=", "value": "GENERATION"},
		{"type": "datetime", "column": "startTime", "operator": ">=", "value": from.UTC().Format(time.RFC3339Nano)},
		{"type": "datetime", "column": "startTime", "operator": "<", "value": to.UTC().Format(time.RFC3339Nano)},
		{"type": "stringObject", "column": "metadata", "key": b.MetadataKey, "operator": "=", "value": app},
	})
	q.Set("filter", string(filter))
	var response struct {
		Data []wireObservation `json:"data"`
		Meta struct {
			TotalItems int     `json:"totalItems"`
			TotalPages int     `json:"totalPages"`
			Cursor     *string `json:"cursor"`
		} `json:"meta"`
	}
	if reason := c.get(ctx, path, q, key, &response); reason != "" {
		return unavailable(r, reason)
	}
	if response.Data == nil || len(response.Data) > SampleLimit {
		return unavailable(r, "response_invalid")
	}
	r.Sample.Truncated = response.Meta.TotalItems > SampleLimit || response.Meta.TotalPages > 1 || response.Meta.Cursor != nil && *response.Meta.Cursor != "" || len(response.Data) == SampleLimit
	seen := map[string]bool{}
	for _, w := range response.Data {
		var owner string
		_ = json.Unmarshal(w.Metadata[b.MetadataKey], &owner)
		if owner != app || w.Type != "GENERATION" || w.StartTime.Before(from) || !w.StartTime.Before(to) || !identifier.MatchString(w.ID) || !identifier.MatchString(w.TraceID) || w.ProjectID != "" && w.ProjectID != b.ProjectID || seen[w.ID] {
			continue
		}
		seen[w.ID] = true
		o := Observation{ID: w.ID, TraceID: w.TraceID, StartTime: w.StartTime, Level: validLevel(w.Level), Metrics: metrics(w)}
		if modelName.MatchString(w.Model) {
			o.Model = w.Model
		}
		if c.config.UIBaseURL != "" {
			o.URL = c.config.UIBaseURL + "/project/" + url.PathEscape(b.ProjectID) + "/traces/" + url.PathEscape(w.TraceID)
		}
		r.Observations = append(r.Observations, o)
	}
	sort.Slice(r.Observations, func(i, j int) bool { return r.Observations[i].StartTime.After(r.Observations[j].StartTime) })
	r.Sample.Returned = len(r.Observations)
	if len(r.Observations) == 0 {
		r.Source.Status = "unobserved"
		r.Source.Reason = "no_scoped_generations"
		return r
	}
	r.Source.Status = "observed"
	r.Source.Reason = "bounded_generation_sample"
	r.Summary = summarize(r.Observations)
	return r
}

func unavailable(r Report, reason string) Report {
	r.Source.Status = "unavailable"
	if reason == "api_unsupported" {
		r.Source.Status = "unsupported"
	}
	r.Source.Reason = reason
	return r
}

// Upstream error bodies and URL-bearing network errors never reach API responses or logs.
func (c *Client) get(ctx context.Context, path string, q url.Values, key credentials, out any) string {
	u := c.config.BaseURL + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "request_invalid"
	}
	req.SetBasicAuth(key.public, key.secret)
	req.Header.Set("Accept", "application/json")
	res, err := c.http.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return "upstream_timeout"
		}
		return "upstream_unavailable"
	}
	defer res.Body.Close()
	if res.StatusCode == 401 || res.StatusCode == 403 {
		return "upstream_auth_failed"
	}
	if res.StatusCode == 404 || res.StatusCode == 405 || res.StatusCode == 501 {
		return "api_unsupported"
	}
	if res.StatusCode != 200 {
		return "upstream_unavailable"
	}
	decoder := json.NewDecoder(io.LimitReader(res.Body, 8<<20))
	if decoder.Decode(out) != nil || decoder.Decode(new(any)) != io.EOF {
		return "response_invalid"
	}
	return ""
}

func validLevel(p *string) *string {
	if p == nil {
		return nil
	}
	switch *p {
	case "ERROR", "WARNING", "DEFAULT", "DEBUG":
		return p
	}
	return nil
}
func number(m map[string]json.RawMessage, k string) *float64 {
	raw, ok := m[k]
	var n *float64
	if !ok || json.Unmarshal(raw, &n) != nil || n == nil || *n < 0 || math.IsNaN(*n) || math.IsInf(*n, 0) || *n > 1e15 {
		return nil
	}
	return n
}
func metrics(w wireObservation) Metrics {
	usage := w.UsageDetails
	if len(usage) == 0 {
		usage = w.Usage
	}
	m := Metrics{InputTokens: number(usage, "input"), OutputTokens: number(usage, "output"), TotalTokens: number(usage, "total"), CostUSD: number(w.CostDetails, "total")}
	if m.CostUSD == nil && w.CalculatedTotalCost != nil && *w.CalculatedTotalCost >= 0 && *w.CalculatedTotalCost <= 1e15 && !math.IsNaN(*w.CalculatedTotalCost) && !math.IsInf(*w.CalculatedTotalCost, 0) {
		m.CostUSD = w.CalculatedTotalCost
	}
	if w.EndTime != nil && !w.EndTime.Before(w.StartTime) {
		v := w.EndTime.Sub(w.StartTime).Seconds() * 1000
		m.LatencyMillis = &v
	}
	return m
}
func summarize(rows []Observation) *Summary {
	traces := map[string]bool{}
	errors := 0
	allLevels := true
	s := &Summary{Generations: len(rows)}
	for _, o := range rows {
		traces[o.TraceID] = true
		if o.Level == nil {
			allLevels = false
		} else if *o.Level == "ERROR" {
			errors++
		}
	}
	s.Traces = len(traces)
	if allLevels {
		s.Errors = &errors
	}
	s.InputTokens = sum(rows, func(m Metrics) *float64 { return m.InputTokens })
	s.OutputTokens = sum(rows, func(m Metrics) *float64 { return m.OutputTokens })
	s.TotalTokens = sum(rows, func(m Metrics) *float64 { return m.TotalTokens })
	s.CostUSD = sum(rows, func(m Metrics) *float64 { return m.CostUSD })
	if total := sum(rows, func(m Metrics) *float64 { return m.LatencyMillis }); total != nil {
		mean := *total / float64(len(rows))
		s.LatencyMillis = &mean
	}
	return s
}
func sum(rows []Observation, field func(Metrics) *float64) *float64 {
	total := 0.0
	for _, r := range rows {
		v := field(r.Metrics)
		if v == nil {
			return nil
		}
		total += *v
	}
	return &total
}
