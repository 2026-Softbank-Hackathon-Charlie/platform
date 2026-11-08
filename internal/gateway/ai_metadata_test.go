package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAIPlacementConfigRejectsUnsafeOrContradictoryEvidence(t *testing.T) {
	valid := `{"laptop":{"serviceId":"local-qwen","kind":"self_hosted","environment":"onprem","accelerator":"gpu"}}`
	if _, err := ParseAIPlacements(valid); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{
		valid + `{}`,
		strings.Replace(valid, `"serviceId"`, `"endpoint"`, 1),
		strings.Replace(valid, `"local-qwen"`, `"https://private?token=secret"`, 1),
		`{"gcp":{"serviceId":"bedrock","kind":"managed","provider":"aws.bedrock","accelerator":"gpu"}}`,
		`{"gcp":{"serviceId":"claude","kind":"external","provider":"anthropic","environment":"eks"}}`,
	} {
		if _, err := ParseAIPlacements(raw); err == nil {
			t.Fatalf("unsafe placement accepted: %s", raw)
		}
	}
}

func TestAIDemoPlacementHeadersOverwriteUpstreamAndCallerClaims(t *testing.T) {
	for _, configured := range []bool{false, true} {
		t.Run(map[bool]string{false: "unknown", true: "declared"}[configured], func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				for _, name := range aiMetadataHeaders {
					if r.Header.Get(name) != "" {
						t.Error("caller metadata reached upstream")
					}
					w.Header().Set(name, "forged")
					w.Header().Add("Trailer", name)
				}
				io.WriteString(w, `{"done":true}`)
				for _, name := range aiMetadataHeaders {
					w.Header().Set(name, "late-forgery")
				}
			}))
			defer upstream.Close()
			g := aiTestGateway(t, upstream.URL, upstream.URL, nil)
			if configured {
				g.cfg.AIDemo.Placements = map[string]AIPlacement{"gcp": {ServiceID: "qwen-gcp", Kind: "self_hosted", Environment: "gke", Accelerator: "gpu"}}
			}
			r := httptest.NewRequest("POST", "http://gateway/api/chat", strings.NewReader(aiTestBody))
			r.Header.Set("Authorization", "Bearer ai-secret")
			r.Header.Set("X-Request-Id", "placement-test")
			for _, name := range aiMetadataHeaders {
				r.Header.Set(name, "caller-forgery")
			}
			w := httptest.NewRecorder()
			g.Proxy().ServeHTTP(w, r)
			if w.Code != http.StatusOK || w.Header().Get("X-Hop-Backend") != "gcp" {
				t.Fatalf("request failed: %d", w.Code)
			}
			for _, name := range aiMetadataHeaders {
				if strings.Contains(w.Header().Get(name), "forg") || len(w.Header().Values(name)) > 1 {
					t.Fatalf("forged response metadata: %v", w.Header())
				}
				if w.Result().Trailer.Get(name) != "" {
					t.Fatalf("forged late trailer: %v", w.Result().Trailer)
				}
			}
			if configured {
				if w.Header().Get("X-Hopcharlie-AI-Environment") != "gke" || w.Header().Get("X-Hopcharlie-AI-Accelerator") != "gpu" || w.Header().Get("X-Hopcharlie-AI-Evidence") != "operator-declared-placement" {
					t.Fatalf("placement missing: %v", w.Header())
				}
			} else if w.Header().Get("X-Hopcharlie-AI-Kind") != "unknown" || w.Header().Get("X-Hopcharlie-AI-Environment") != "" || w.Header().Get("X-Hopcharlie-AI-Accelerator") != "unknown" {
				t.Fatalf("backend name inferred hosting: %v", w.Header())
			}
		})
	}
}
