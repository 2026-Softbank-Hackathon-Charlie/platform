package probes

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/preflight/parity"
)

func metricResponse(r *http.Request, value string, age time.Duration) map[string]any {
	now, _ := strconv.ParseFloat(r.URL.Query().Get("time"), 64)
	metric := map[string]string{"namespace": "shop", "job": "shop-api", "instance": "pod-1"}
	if strings.HasPrefix(r.URL.Query().Get("query"), "timestamp(") {
		value = strconv.FormatFloat(now-age.Seconds(), 'f', 6, 64)
	} else {
		metric["__name__"] = "up"
	}
	return map[string]any{"status": "success", "data": map[string]any{"resultType": "vector", "result": []any{map[string]any{"metric": metric, "value": []any{now, value}}}}}
}

func TestPrometheusRealHTTPAndAlertRemainsUnknown(t *testing.T) {
	var queries []string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/prometheus/api/v1/query" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		queries = append(queries, r.URL.Query().Get("query"))
		json.NewEncoder(w).Encode(metricResponse(r, "1", 5*time.Second))
	}))
	defer s.Close()
	p, err := NewPrometheus(target(), s.URL+"/prometheus", "shop-api", s.Client(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	r, err := p.Check(context.Background(), target())
	if err != nil || r.Status != parity.Unknown || len(r.Checks) != 2 || r.Checks[0].Status != parity.Pass || r.Checks[1].Code != "alert_probe_not_implemented" {
		t.Fatalf("%+v %v", r, err)
	}
	if len(queries) != 2 || queries[0] != `up{namespace="shop",job="shop-api"}` || queries[1] != "timestamp("+queries[0]+")" {
		t.Fatalf("queries=%v", queries)
	}
	source := target()
	source.Environment = "aws"
	v, err := parity.Evaluate(context.Background(), parity.Request{Source: parity.Baseline{Target: source, Complete: true, Capabilities: []parity.Capability{parity.Observability}}, Target: target()}, map[parity.Capability]parity.Probe{parity.Observability: p})
	if err != nil || v.Passed {
		t.Fatalf("scrape alone passed alert capability: %+v %v", v, err)
	}
}

func TestPrometheusFailures(t *testing.T) {
	for _, name := range []string{"down", "stale", "future", "empty", "missing timestamps", "wrong labels", "different series", "duplicate series", "warnings", "http error", "malformed", "nan", "matrix", "oversized", "secret value"} {
		t.Run(name, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				age := 5 * time.Second
				if name == "stale" {
					age = 2 * time.Minute
				}
				if name == "future" {
					age = -20 * time.Second
				}
				value := "1"
				if name == "down" {
					value = "0"
				}
				if name == "nan" {
					value = "NaN"
				}
				if name == "secret value" {
					value = "TOPSECRET"
				}
				response := metricResponse(r, value, age)
				data := response["data"].(map[string]any)
				samples := data["result"].([]any)
				isTime := strings.HasPrefix(r.URL.Query().Get("query"), "timestamp(")
				switch name {
				case "empty":
					data["result"] = []any{}
				case "missing timestamps":
					if isTime {
						data["result"] = []any{}
					}
				case "wrong labels":
					samples[0].(map[string]any)["metric"].(map[string]string)["namespace"] = "other"
				case "different series":
					if isTime {
						samples[0].(map[string]any)["metric"].(map[string]string)["instance"] = "other"
					}
				case "duplicate series":
					data["result"] = append(samples, samples[0])
				case "warnings":
					response["warnings"] = []string{"TOPSECRET partial response"}
				case "http error":
					w.WriteHeader(503)
					w.Write([]byte("TOPSECRET"))
					return
				case "malformed":
					w.Write([]byte("TOPSECRET not JSON"))
					return
				case "matrix":
					data["resultType"] = "matrix"
				case "oversized":
					w.Write([]byte(strings.Repeat("x", (1<<20)+1)))
					return
				}
				json.NewEncoder(w).Encode(response)
			}))
			defer s.Close()
			p, err := NewPrometheus(target(), s.URL, "shop-api", s.Client(), time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			r, err := p.Check(context.Background(), target())
			raw, _ := json.Marshal(r)
			if err != nil || r.Status == parity.Pass || r.Checks[0].Status == parity.Pass || strings.Contains(string(raw), "TOPSECRET") {
				t.Fatalf("%s %v", raw, err)
			}
			if name == "down" && r.Status != parity.Fail {
				t.Fatalf("down not failed: %+v", r)
			}
		})
	}
}

func TestPrometheusRejectsRedirectsAndWrongTargets(t *testing.T) {
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("followed redirect") }))
	defer other.Close()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, other.URL, 302) }))
	defer s.Close()
	client := s.Client()
	p, err := NewPrometheus(target(), s.URL, "shop-api", client, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if client.CheckRedirect != nil || client.Timeout != 0 {
		t.Fatal("mutated caller's HTTP client")
	}
	r, err := p.Check(context.Background(), target())
	if err != nil || r.Checks[0].Status != parity.Unknown {
		t.Fatalf("%+v %v", r, err)
	}
	wrong := target()
	wrong.Environment = "other"
	if _, err := p.Check(context.Background(), wrong); err != ErrWrongTarget {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r, err = p.Check(ctx, target())
	if err != nil || r.Status != parity.Unknown {
		t.Fatalf("%+v %v", r, err)
	}
}

func TestPrometheusInvalidConfig(t *testing.T) {
	for _, endpoint := range []string{"", "file:///etc/passwd", "https://user:TOPSECRET@example.com", "https://example.com?token=TOPSECRET", "https://example.com#fragment", "http://"} {
		if _, err := NewPrometheus(target(), endpoint, "shop", http.DefaultClient, time.Minute); err != ErrInvalidConfig {
			t.Fatalf("%v", err)
		}
	}
	if _, err := NewPrometheus(target(), "http://localhost", "", http.DefaultClient, time.Minute); err == nil {
		t.Fatal("empty selector accepted")
	}
	if _, err := NewPrometheus(target(), "http://localhost", "shop", http.DefaultClient, 0); err == nil {
		t.Fatal("unbounded age accepted")
	}
	var p *Prometheus
	if _, err := p.Check(context.Background(), target()); err == nil {
		t.Fatal("nil probe accepted")
	}
}
