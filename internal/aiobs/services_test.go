package aiobs

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestAIServicesSeparatesCallerHostAndOnlyShowsExplicitGPU(t *testing.T) {
	for _, tc := range []struct{ provider, kind, host, gpu, wantKind, wantProvider, wantGPU string }{
		{"", "self_hosted", "onprem", "gpu", "self_hosted", "", "gpu"},
		{"aws.bedrock", "", "", "gpu", "managed", "aws", "unknown"},
		{"anthropic", "", "eks", "gpu", "external", "anthropic", "unknown"},
		{"", "", "", "", "unknown", "", "unknown"},
	} {
		t.Run(tc.wantKind, func(t *testing.T) {
			v := serviceFromRow(aiServiceRow{Caller: "gke", Provider: tc.provider, Kind: tc.kind, Host: tc.host, Accelerator: tc.gpu, Model: "qwen2.5"}, 2, t0)
			if v.CallerEnv != "gke" || v.Kind != tc.wantKind || v.Location.Provider != tc.wantProvider || v.Location.Accelerator != tc.wantGPU {
				t.Fatalf("wrong attribution: %+v", v)
			}
			if v.Kind != "self_hosted" && v.Location.Environment != "" {
				t.Fatal("caller/host crossed")
			}
		})
	}
	old := serviceFromRow(aiServiceRow{Caller: "gke", Model: "qwen-gpu"}, 0, t0)
	if old.Activity.State == "active" || old.Location.Accelerator != "unknown" {
		t.Fatal("stale calls or model name invented GPU traffic")
	}
	secret := serviceFromRow(aiServiceRow{Identity: "https://user:secret@provider", Model: "sk-secret", Caller: "password=secret"}, 1, t0)
	raw, _ := json.Marshal(secret)
	if strings.Contains(string(raw), "secret") {
		t.Fatal("untrusted metadata leaked")
	}
}

func TestAIServicesScopesBoundsAndProjectsMetadataOnly(t *testing.T) {
	until := t0.Add(time.Hour)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q, _ := io.ReadAll(r.Body)
		if r.URL.Query().Get("param_app") != "moimroom" || r.URL.Query().Get("param_x0") != "moimroom-mig" || r.URL.Query().Get("param_recent") == "" {
			t.Error("missing bound app/window/exclusion")
		}
		for _, part := range []string{"default.otel_traces", "uniqExactIf", "LIMIT 128", "dependency.name", "SpanKind", "coalesce(nullIf(ResourceAttributes['hopcharlie.environment.name'],'')"} {
			if !strings.Contains(string(q), part) {
				t.Errorf("missing query guard %s", part)
			}
		}
		if strings.Contains(string(q), "Body") || strings.Contains(string(q), "SELECT *") || strings.Contains(string(q), "server.address") {
			t.Error("raw content or endpoint was queried")
		}
		for _, model := range []string{"qwen-a", "qwen-b"} {
			b, _ := json.Marshal(aiServiceRow{Identity: "local-model", Caller: "gke", Host: "onprem", Kind: "self_hosted", Accelerator: "gpu", Model: model, Requests: "3", Last: strconv.FormatInt(until.Add(-time.Second).UnixMilli(), 10)})
			_, _ = w.Write(append(b, '\n'))
		}
	}))
	defer server.Close()
	c, err := New(server.URL, "reader", "password", "")
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.AIServices(context.Background(), "moimroom", until.Add(-5*time.Minute), until, "moimroom-mig")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Services) != 1 || *got.Services[0].Activity.Requests != 6 || got.Services[0].Kind != "self_hosted" || got.Services[0].Location.Environment != "onprem" {
		t.Fatalf("lost physical node grouping: %+v", got)
	}
	if _, err = c.AIServices(context.Background(), "bad' OR 1", until.Add(-time.Minute), until); err == nil {
		t.Fatal("unbound app accepted")
	}
}

func TestAIServicesRejectsInconsistentActivity(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"requests":"2","last":"0"}`)) }))
	defer server.Close()
	c, _ := New(server.URL, "", "", "")
	if _, err := c.AIServices(context.Background(), "shop", t0.Add(-time.Minute), t0); err == nil {
		t.Fatal("old evidence became recent traffic")
	}
}
