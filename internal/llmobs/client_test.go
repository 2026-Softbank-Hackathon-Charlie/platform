package llmobs

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var from = time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC)
var to = from.Add(time.Hour)

func clientFor(t *testing.T, url string) *Client {
	t.Helper()
	dir := t.TempDir()
	pub := filepath.Join(dir, "public")
	secret := filepath.Join(dir, "secret")
	if err := os.WriteFile(pub, []byte("fixture-public"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secret, []byte("fixture-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := New(Config{BaseURL: url, UIBaseURL: "https://langfuse.example", Apps: map[string]Binding{"shop": {ProjectID: "project-shop", MetadataKey: "app", PublicKeyFile: pub, SecretKeyFile: secret}}})
	if err != nil {
		t.Fatal(err)
	}
	return c
}
func healthAndProject(w http.ResponseWriter, r *http.Request, version string) bool {
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/api/public/health":
		fmt.Fprintf(w, `{"version":%q,"status":"OK"}`, version)
		return true
	case "/api/public/projects":
		fmt.Fprint(w, `{"data":[{"id":"project-shop"}]}`)
		return true
	}
	return false
}
func TestVersionsScopeAndNullableMetrics(t *testing.T) {
	for _, major := range []int{3, 4} {
		t.Run(fmt.Sprint(major), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				pub, secret, ok := r.BasicAuth()
				if !ok || pub != "fixture-public" || secret != "fixture-secret" {
					t.Error("missing project authentication")
				}
				if r.Method != "GET" {
					t.Fatal("read adapter attempted mutation")
				}
				if healthAndProject(w, r, fmt.Sprintf("%d.133.0", major)) {
					return
				}
				want := "/api/public/observations"
				if major == 4 {
					want = "/api/public/v2/observations"
				}
				if r.URL.Path != want {
					t.Errorf("path %s, want %s", r.URL.Path, want)
				}
				q := r.URL.Query()
				if q.Get("limit") != "100" || !strings.Contains(q.Get("filter"), `"value":"shop"`) {
					t.Error("scope or query bound missing")
				}
				if major == 4 && strings.Contains(q.Get("fields"), "io") {
					t.Error("requested prompt/output field group")
				}
				fmt.Fprint(w, `{"data":[
			 {"id":"ours","traceId":"trace-ours","projectId":"project-shop","type":"GENERATION","startTime":"2026-11-08T00:10:00Z","endTime":"2026-11-08T00:10:00.100Z","level":"DEFAULT","model":"openai/gpt-4","metadata":{"app":"shop","prompt":"private metadata"},"input":"private prompt","output":"private output","usageDetails":{"input":0,"output":4,"total":4},"costDetails":{"total":0}},
			 {"id":"ours-two","traceId":"trace-ours","type":"GENERATION","startTime":"2026-11-08T00:11:00Z","endTime":"2026-11-08T00:11:00.500Z","metadata":{"app":"shop"}},
			 {"id":"foreign","traceId":"trace-foreign","type":"GENERATION","startTime":"2026-11-08T00:10:00Z","metadata":{"app":"other"},"input":"foreign prompt"},
			 {"id":"future","traceId":"future-trace","type":"GENERATION","startTime":"2026-11-08T02:00:00Z","metadata":{"app":"shop"}}
			],"meta":{"totalItems":101}}`)
			}))
			defer server.Close()
			c := clientFor(t, server.URL)
			r := c.Report(context.Background(), "shop", from, to)
			if r.Source.Status != "observed" || r.Summary == nil || r.Summary.Generations != 2 || r.Summary.Traces != 1 || !r.Sample.Truncated {
				t.Fatalf("unexpected summary %+v", r)
			}
			if r.Summary.InputTokens != nil || r.Summary.CostUSD != nil || r.Summary.Errors != nil {
				t.Fatal("missing values became measured zero")
			}
			if r.Summary.LatencyMillis == nil || *r.Summary.LatencyMillis != 300 {
				t.Fatalf("mean latency %+v", r.Summary.LatencyMillis)
			}
			o := r.Observations[1]
			if o.InputTokens == nil || *o.InputTokens != 0 || o.CostUSD == nil || *o.CostUSD != 0 {
				t.Fatal("reported zeros were lost")
			}
			if o.Model != "openai/gpt-4" {
				t.Errorf("model lost: %s", o.Model)
			}
			if o.URL != "https://langfuse.example/project/project-shop/traces/trace-ours" {
				t.Errorf("unexpected trace URL %s", o.URL)
			}
			b, _ := json.Marshal(r)
			for _, forbidden := range []string{"private prompt", "private output", "private metadata", "foreign", "fixture-secret", "input\"", "metadata\""} {
				if strings.Contains(string(b), forbidden) {
					t.Errorf("sensitive field escaped whitelist: %s", forbidden)
				}
			}
		})
	}
}
func TestUnavailableDoesNotLeakAndUnmappedDoesNotQuery(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(401)
		fmt.Fprint(w, `{"error":"fixture-secret upstream private prompt"}`)
	}))
	defer server.Close()
	c := clientFor(t, server.URL)
	if r := c.Report(context.Background(), "unbound", from, to); r.Source.Status != "scope_unmapped" || calls != 0 {
		t.Fatal("unbound app queried project")
	}
	r := c.Report(context.Background(), "shop", from, to)
	b, _ := json.Marshal(r)
	if r.Source.Status != "unavailable" || r.Source.Reason != "upstream_auth_failed" || r.Summary != nil || strings.Contains(string(b), "fixture-secret") {
		t.Fatalf("unsafe error response %s", b)
	}
}
func TestProjectMismatchVersionAndRedirect(t *testing.T) {
	for _, mode := range []string{"project", "version", "redirect"} {
		t.Run(mode, func(t *testing.T) {
			redirected := false
			target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirected = true }))
			defer target.Close()
			observations := false
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if mode == "redirect" {
					http.Redirect(w, r, target.URL, http.StatusFound)
					return
				}
				if r.URL.Path == "/api/public/health" {
					v := "3.133.0"
					if mode == "version" {
						v = "5.0.0"
					}
					fmt.Fprintf(w, `{"version":%q}`, v)
					return
				}
				if r.URL.Path == "/api/public/projects" {
					fmt.Fprint(w, `{"data":[{"id":"other-project"}]}`)
					return
				}
				observations = true
				fmt.Fprint(w, `{"data":[]}`)
			}))
			defer server.Close()
			r := clientFor(t, server.URL).Report(context.Background(), "shop", from, to)
			if redirected || observations || r.Summary != nil {
				t.Fatal("scope boundary escaped")
			}
			if mode == "version" && r.Source.Status != "unsupported" {
				t.Fatalf("unsupported version %+v", r.Source)
			}
		})
	}
}
func TestConfigurationRejectsDynamicDestinations(t *testing.T) {
	for _, base := range []string{"http://outside.example", "https://user:private@example.test", "https://example.test/?token=private", "https://example.test/path", "file:///private"} {
		if _, err := New(Config{BaseURL: base}); err == nil {
			t.Errorf("unsafe base accepted: %s", base)
		}
	}
	if _, err := New(Config{BaseURL: "https://example.test", UIBaseURL: "https://example.test/?api_key=private"}); err == nil {
		t.Fatal("credential-bearing UI URL accepted")
	}
}

func TestLegacyUsageUnitsAndNullValues(t *testing.T) {
	var wire wireObservation
	if err := json.Unmarshal([]byte(`{"usage":{"input":0,"output":3,"total":3,"unit":"TOKENS"},"costDetails":{"total":null}}`), &wire); err != nil {
		t.Fatal(err)
	}
	m := metrics(wire)
	if m.InputTokens == nil || *m.InputTokens != 0 || m.OutputTokens == nil || *m.OutputTokens != 3 || m.CostUSD != nil || m.LatencyMillis != nil {
		t.Fatalf("legacy/null metrics %+v", m)
	}
}

func TestModelLabelsDoNotExposeAddressesOrCredentialLikeStrings(t *testing.T) {
	for _, value := range []string{"https://private.example/model", "sk-secret", "SK-secret", "pk-lf-private", "AKIAEXAMPLE000000", "ASIAEXAMPLE000000", "Bearer-private"} {
		if safeModel(value) != "" {
			t.Errorf("sensitive model label accepted")
		}
	}
	for _, value := range []string{"openai/gpt-4", "Qwen/Qwen2.5-7B-Instruct", "qwen2.5:7b"} {
		if safeModel(value) != value {
			t.Errorf("valid model label lost: %s", value)
		}
	}
}

func TestLegacyUsageRequiresExplicitTokenUnits(t *testing.T) {
	for _, raw := range []string{
		`{"usage":{"input":12,"output":24,"total":36,"unit":"CHARACTERS"}}`,
		`{"usage":{"input":12,"output":24,"total":36}}`,
		`{"usage":{"input":12,"output":24,"total":36,"unit":null}}`,
	} {
		var wire wireObservation
		if err := json.Unmarshal([]byte(raw), &wire); err != nil {
			t.Fatal(err)
		}
		m := metrics(wire)
		if m.InputTokens != nil || m.OutputTokens != nil || m.TotalTokens != nil {
			t.Fatal("non-token or unknown legacy unit became token counts")
		}
	}
	var modern wireObservation
	if err := json.Unmarshal([]byte(`{"usageDetails":{"input":0,"output":2,"total":2},"usage":{"input":100,"unit":"CHARACTERS"}}`), &modern); err != nil {
		t.Fatal(err)
	}
	m := metrics(modern)
	if m.InputTokens == nil || *m.InputTokens != 0 || m.OutputTokens == nil || *m.OutputTokens != 2 {
		t.Fatal("modern token usage was replaced by legacy units")
	}
}

func TestV4TotalCostAndMissingCosts(t *testing.T) {
	for _, value := range []float64{0, 0.0000025} {
		var wire wireObservation
		if err := json.Unmarshal([]byte(fmt.Sprintf(`{"totalCost":%g}`, value)), &wire); err != nil {
			t.Fatal(err)
		}
		if cost := metrics(wire).CostUSD; cost == nil || *cost != value {
			t.Fatal("v4 totalCost was lost")
		}
	}
	for _, raw := range []string{`{}`, `{"totalCost":null}`, `{"totalCost":-1}`} {
		var wire wireObservation
		if err := json.Unmarshal([]byte(raw), &wire); err != nil {
			t.Fatal(err)
		}
		if metrics(wire).CostUSD != nil {
			t.Fatal("missing or invalid cost became measured")
		}
	}
}
