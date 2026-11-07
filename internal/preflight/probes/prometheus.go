package probes

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/preflight/parity"
)

// Prometheus verifies actual scrape health and sample age for a configured job
// in the target namespace. It does NOT prove alert delivery: metrics-alerting
// remains unknown until an end-to-end alert probe is implemented.
type Prometheus struct {
	target parity.Target
	url    string
	job    string
	client http.Client
	maxAge time.Duration
	labels map[string]string
}

// NewPrometheus accepts only a server-owned endpoint and client (including any
// authentication transport). It disables redirects to avoid forwarding
// credentials to a different endpoint. It never reads a URL from a plan.
func NewPrometheus(target parity.Target, endpoint, job string, client *http.Client, maxAge time.Duration) (*Prometheus, error) {
	return NewPrometheusWithLabels(target, endpoint, job, client, maxAge, nil)
}

// NewPrometheusWithLabels binds a shared query endpoint to the configured
// environment/cluster as well as the app namespace and job. Labels are trusted
// server configuration, never supplied by a migration plan.
func NewPrometheusWithLabels(target parity.Target, endpoint, job string, client *http.Client, maxAge time.Duration, labels map[string]string) (*Prometheus, error) {
	u, err := url.Parse(endpoint)
	if !parity.ValidTarget(target) || err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
		(u.Scheme != "http" && u.Scheme != "https") || strings.TrimSpace(job) == "" || len(job) > 200 || strings.ContainsAny(job, "\r\n\x00") ||
		maxAge < time.Second || maxAge > 10*time.Minute || client == nil {
		return nil, ErrInvalidConfig
	}
	queryURL, err := url.JoinPath(endpoint, "api/v1/query")
	if err != nil {
		return nil, ErrInvalidConfig
	}
	c := *client
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if c.Timeout <= 0 || c.Timeout > 10*time.Second {
		c.Timeout = 10 * time.Second
	}
	owned := map[string]string{}
	for key, value := range labels {
		if (key != "environment" && key != "cluster") || len(value) == 0 || len(value) > 200 || strings.ContainsAny(value, "\r\n\x00") {
			return nil, ErrInvalidConfig
		}
		owned[key] = value
	}
	return &Prometheus{target: target, url: queryURL, job: job, client: c, maxAge: maxAge, labels: owned}, nil
}

func (p *Prometheus) Check(ctx context.Context, target parity.Target) (parity.Result, error) {
	now := time.Now().UTC()
	r := parity.Result{Capability: parity.Observability, Target: target, Status: parity.Unknown, Code: "observability_incomplete", ObservedAt: now, Checks: []parity.Check{}}
	if p == nil || p.url == "" {
		return r, ErrInvalidConfig
	}
	if target != p.target {
		return r, ErrWrongTarget
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	check := p.checkMetrics(ctx, now)
	r.Checks = append(r.Checks, check, parity.Check{ID: "alert-delivery", Status: parity.Unknown, Code: "alert_probe_not_implemented"})
	if check.Status == parity.Fail {
		r.Status, r.Code = parity.Fail, "metrics_failed"
	}
	return r, nil
}

func (p *Prometheus) checkMetrics(ctx context.Context, now time.Time) parity.Check {
	c := parity.Check{ID: "metrics-target", Status: parity.Unknown, Code: "metrics_unavailable"}
	selector := "up{namespace=" + strconv.Quote(p.target.Namespace) + ",job=" + strconv.Quote(p.job)
	keys := make([]string, 0, len(p.labels))
	for key := range p.labels {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		selector += "," + key + "=" + strconv.Quote(p.labels[key])
	}
	selector += "}"
	up, err := p.query(ctx, selector, now)
	if err != nil {
		return c
	}
	if len(up) == 0 {
		c.Code = "metrics_missing"
		return c
	}
	// An instant-query sample's outer timestamp is the evaluation time, not
	// the last scrape. timestamp(up) exposes the actual underlying sample age.
	timestamps, err := p.query(ctx, "timestamp("+selector+")", now)
	if err != nil || len(timestamps) != len(up) {
		c.Code = "metrics_timestamps_missing"
		return c
	}
	for key, value := range up {
		stamp, ok := timestamps[key]
		if !ok {
			c.Code = "metrics_timestamps_missing"
			return c
		}
		age := float64(now.UnixNano())/1e9 - stamp
		if age < -5 || age > p.maxAge.Seconds() {
			c.Code = "metrics_stale"
			return c
		}
		if value != 0 && value != 1 {
			c.Code = "metrics_invalid"
			return c
		}
	}
	for _, value := range up {
		if value == 0 {
			c.Status, c.Code = parity.Fail, "target_not_up"
			return c
		}
	}
	c.Status, c.Code = parity.Pass, "scrape_verified"
	c.Evidence = fmt.Sprintf("%d target(s): up=1; sample age <= %s", len(up), p.maxAge)
	return c
}

type vectorResponse struct {
	Status   string   `json:"status"`
	Warnings []string `json:"warnings"`
	Data     struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string `json:"metric"`
			Value  []json.RawMessage `json:"value"`
		} `json:"result"`
	} `json:"data"`
}

func (p *Prometheus) query(ctx context.Context, query string, now time.Time) (map[string]float64, error) {
	// All failures use a fixed local error. HTTP bodies, URLs and credentials
	// must not become evidence or provider error messages in parity reports.
	bad := ErrInvalidConfig
	u, err := url.Parse(p.url)
	if err != nil {
		return nil, bad
	}
	q := u.Query()
	q.Set("query", query)
	q.Set("time", strconv.FormatFloat(float64(now.UnixNano())/1e9, 'f', 9, 64))
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, bad
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, bad
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, bad
	}
	const limit = 1 << 20
	raw, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil || len(raw) > limit {
		return nil, bad
	}
	var v vectorResponse
	if err := json.Unmarshal(raw, &v); err != nil || v.Status != "success" || v.Data.ResultType != "vector" || len(v.Warnings) != 0 {
		return nil, bad
	}
	out := map[string]float64{}
	for _, sample := range v.Data.Result {
		if sample.Metric["namespace"] != p.target.Namespace || sample.Metric["job"] != p.job || len(sample.Value) != 2 {
			return nil, bad
		}
		for key, want := range p.labels {
			if sample.Metric[key] != want {
				return nil, bad
			}
		}
		var outer float64
		var value string
		if json.Unmarshal(sample.Value[0], &outer) != nil || outer <= 0 || math.IsInf(outer, 0) || math.IsNaN(outer) ||
			json.Unmarshal(sample.Value[1], &value) != nil {
			return nil, bad
		}
		n, err := strconv.ParseFloat(value, 64)
		if err != nil || math.IsNaN(n) || math.IsInf(n, 0) {
			return nil, bad
		}
		delete(sample.Metric, "__name__") // timestamp() drops the metric name.
		key, err := json.Marshal(sample.Metric)
		if err != nil {
			return nil, bad
		}
		if _, exists := out[string(key)]; exists {
			return nil, bad
		}
		out[string(key)] = n
	}
	return out, nil
}
