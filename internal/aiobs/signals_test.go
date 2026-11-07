package aiobs

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/preflight/parity"
)

func TestSignalCountsQueryBindsExactAppEnvironmentNamespaceAndCluster(t *testing.T) {
	requests := 0
	now := time.Now().UTC().Truncate(time.Millisecond)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		query, _ := io.ReadAll(r.Body)
		for _, want := range []string{"{services:String}", "{namespace:String}", "{environment:String}", "{cluster:String}", "{since:Int64}", "{until:Int64}"} {
			if !strings.Contains(string(query), want) {
				t.Error("missing query binding: " + want)
			}
		}
		for key, want := range map[string]string{"services": "shop-web", "namespace": "shop", "environment": "dev", "cluster": "onprem-laptop"} {
			if r.URL.Query().Get("param_"+key) != want {
				t.Error("scope escaped")
			}
		}
		if strings.Contains(string(query), "Body") || strings.Contains(string(query), "SpanAttributes") {
			t.Error("raw telemetry queried")
		}
		io.WriteString(w, `{"samples":"0","lastSeen":"0"}`)
	}))
	defer s.Close()
	c, err := New(s.URL, "readonly", "fixture", "default.otel_logs")
	if err != nil {
		t.Fatal(err)
	}
	out, err := c.SignalCounts(context.Background(), parity.SignalScope{Target: parity.Target{Environment: "onprem", Namespace: "shop", Application: "shop"}, Cluster: "onprem-laptop", DeploymentEnvironment: "dev", Services: []string{"shop-web"}}, now.Add(-time.Minute), now)
	if err != nil || len(out) != 2 || requests != 2 {
		t.Fatalf("%+v %v", out, err)
	}
	for _, record := range out {
		if record.Samples != 0 || !record.LastSeen.IsZero() {
			t.Fatal("absent data became a measurement")
		}
	}
}
