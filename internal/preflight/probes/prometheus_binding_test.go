package probes

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/preflight/parity"
)

func TestPrometheusBindsEnvironmentEvenWithMatchingNamespaceAndJob(t *testing.T) {
	for _, env := range []string{"onprem", "gke", ""} {
		t.Run("reported-"+env, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !strings.Contains(r.URL.Query().Get("query"), `environment="onprem"`) {
					t.Error("environment not bound in query")
				}
				response := metricResponse(r, "1", time.Second)
				metric := response["data"].(map[string]any)["result"].([]any)[0].(map[string]any)["metric"].(map[string]string)
				if env != "" {
					metric["environment"] = env
				}
				json.NewEncoder(w).Encode(response)
			}))
			defer s.Close()
			p, err := NewPrometheusWithLabels(target(), s.URL, "shop-api", s.Client(), time.Minute, map[string]string{"environment": "onprem"})
			if err != nil {
				t.Fatal(err)
			}
			r, err := p.Check(context.Background(), target())
			if err != nil {
				t.Fatal(err)
			}
			if (r.Checks[0].Status == parity.Pass) != (env == "onprem") {
				t.Fatalf("wrong environment proved scrape health: %+v", r)
			}
			if r.Status == parity.Pass {
				t.Fatal("scrape evidence claimed untested alert delivery")
			}
		})
	}
}
