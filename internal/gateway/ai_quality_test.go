package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/llmquality"
)

type qualityProviderFunc func(context.Context, llmquality.Policy, time.Time, time.Time) llmquality.Verdict

func (f qualityProviderFunc) Quality(c context.Context, p llmquality.Policy, n, b time.Time) llmquality.Verdict {
	return f(c, p, n, b)
}

func qualityGW(t *testing.T, file string, provider QualityProvider) *Gateway {
	t.Helper()
	u, _ := url.Parse("http://gcp")
	v, _ := url.Parse("http://laptop")
	p := llmquality.Policy{App: "app", ProjectID: "project", SourceBackend: "gcp", TargetBackend: "laptop", SourceModel: "cloud-revision", TargetModel: "local-revision", DatasetRevision: "dataset-v1", PromptRevision: "prompt-v1", RetrievalRevision: "retrieval-v1", EvaluatorRevision: "judge-v1", JudgeModel: "independent-judge", MaxAgeSeconds: 600, MinSoakSeconds: 30, Metrics: []llmquality.Metric{{Name: "correctness", ConfigID: "correct-v1", Minimum: .9, MaxRegression: .05}, {Name: "safety", ConfigID: "safety-v1", Minimum: 1, HardFailure: true}}}
	for i := range 20 {
		p.Items = append(p.Items, fmt.Sprintf("item-%d", i))
	}
	g, err := New(Config{Backends: []BackendConfig{{Name: "gcp", URL: u}, {Name: "laptop", URL: v}}, AdminToken: "admin-secret", AIDemo: &AIDemoConfig{App: "app", Token: "ai-secret"}, AIQuality: &AIQualityConfig{Policy: p, Provider: provider}, WriteTimeout: 12 * time.Second, StateFile: file, Transport: aiRT(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(r.URL.Host)), Request: r}, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func canaryCall(client *http.Client, method, endpoint, token, id, body string) aiReply {
	r, err := http.NewRequest(method, endpoint, strings.NewReader(body))
	if err != nil {
		return aiReply{err: err}
	}
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Idempotency-Key", id)
	r.Header.Set("Content-Type", "application/json")
	res, err := client.Do(r)
	if err != nil {
		return aiReply{err: err}
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	return aiReply{code: res.StatusCode, backend: res.Header.Get("X-Hop-Backend"), body: b, err: err}
}

func passQuality(_ context.Context, _ llmquality.Policy, n, _ time.Time) llmquality.Verdict {
	return llmquality.Verdict{Status: "pass", Reason: "paired_quality_within_policy", CheckedAt: n, ValidUntil: n.Add(time.Minute), Batch: "batch-v1", Pairs: 20}
}

func TestAIQualityActualPOSTCanaryAndBypassGuards(t *testing.T) {
	g := qualityGW(t, "", qualityProviderFunc(passQuality))
	public, admin := httptest.NewServer(g.Proxy()), httptest.NewServer(g.Admin())
	defer public.Close()
	defer admin.Close()
	requireAIStatus(t, canaryCall(admin.Client(), "POST", admin.URL+"/v1/ai/canary/advance", "wrong", "", `{"expectedVersion":0,"targetPercent":5}`), 401)
	requireAIStatus(t, aiCall(admin.Client(), "POST", admin.URL+"/v1/route", "admin-secret", "", `{"backend":"laptop"}`), 409)
	requireAIStatus(t, aiCall(admin.Client(), "POST", admin.URL+"/v1/weights", "admin-secret", "", `{"readWeights":{"laptop":100}}`), 409)
	r := canaryCall(admin.Client(), "POST", admin.URL+"/v1/ai/canary/advance", "admin-secret", "op-1", `{"expectedVersion":0,"targetPercent":100}`)
	requireAIStatus(t, r, 409)
	r = canaryCall(admin.Client(), "POST", admin.URL+"/v1/ai/canary/advance", "admin-secret", "op-1", `{"expectedVersion":0,"targetPercent":5}`)
	requireAIStatus(t, r, 200)
	var state AICanaryState
	if json.Unmarshal(r.body, &state) != nil || state.Version != 1 || state.TargetPercent != 5 {
		t.Fatalf("state %s", r.body)
	}
	// Duplicate command is idempotent; old CAS with a new key is rejected.
	requireAIStatus(t, canaryCall(admin.Client(), "POST", admin.URL+"/v1/ai/canary/advance", "admin-secret", "op-1", `{"expectedVersion":0,"targetPercent":5}`), 200)
	requireAIStatus(t, canaryCall(admin.Client(), "POST", admin.URL+"/v1/ai/canary/advance", "admin-secret", "op-2", `{"expectedVersion":0,"targetPercent":25}`), 409)
	requireAIStatus(t, canaryCall(admin.Client(), "POST", admin.URL+"/v1/ai/canary/advance", "admin-secret", "op-2", `{"expectedVersion":1,"targetPercent":25}`), 409)
	counts := map[string]int{}
	for i := range 200 {
		reply := aiCall(public.Client(), "POST", public.URL+"/api/chat", "ai-secret", fmt.Sprintf("req-%d", i), aiTestBody)
		requireAIStatus(t, reply, 200)
		counts[reply.backend]++
	}
	if counts["laptop"] < 1 || counts["gcp"] < 1 {
		t.Fatalf("POST did not use both backends: %v", counts)
	}
	g.mu.Lock()
	g.canary.Verdict.ValidUntil = time.Now().Add(-time.Second)
	g.mu.Unlock()
	for i := range 30 {
		reply := aiCall(public.Client(), "POST", public.URL+"/api/chat", "ai-secret", fmt.Sprintf("expired-%d", i), aiTestBody)
		if reply.backend != "gcp" {
			t.Fatalf("expired evidence routed to %q", reply.backend)
		}
	}
}

func TestAIQualityUnknownRegressionAndRestart(t *testing.T) {
	status := "unknown"
	provider := qualityProviderFunc(func(_ context.Context, _ llmquality.Policy, n, _ time.Time) llmquality.Verdict {
		v := passQuality(nil, llmquality.Policy{}, n, time.Time{})
		v.Status = status
		return v
	})
	file := filepath.Join(t.TempDir(), "state.json")
	g := qualityGW(t, file, provider)
	admin := httptest.NewServer(g.Admin())
	defer admin.Close()
	requireAIStatus(t, canaryCall(admin.Client(), "POST", admin.URL+"/v1/ai/canary/advance", "admin-secret", "op-1", `{"expectedVersion":0,"targetPercent":5}`), 412)
	status = "pass"
	requireAIStatus(t, canaryCall(admin.Client(), "POST", admin.URL+"/v1/ai/canary/advance", "admin-secret", "op-1", `{"expectedVersion":0,"targetPercent":5}`), 200)
	restarted := qualityGW(t, file, provider)
	restarted.mu.Lock()
	if restarted.canary.Verdict.Status != "unknown" || restarted.canary.TargetPercent != 5 {
		t.Fatal("restart reused evidence or lost planned state")
	}
	restarted.mu.Unlock()
	status = "regressed"
	g.refreshAIQuality(context.Background())
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.canary.TargetPercent != 0 || g.canary.Version != 2 || g.writeBackend != "gcp" {
		t.Fatalf("regression did not roll back AI only: %+v", g.canary)
	}
}

func TestAIQualityRefreshCannotOverwriteConcurrentAdvanceOrBlockHold(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	provider := qualityProviderFunc(func(c context.Context, p llmquality.Policy, n, b time.Time) llmquality.Verdict {
		if calls.Add(1) == 1 {
			close(started)
			select {
			case <-release:
			case <-c.Done():
				return llmquality.Unknown(n, "cancelled")
			}
			return llmquality.Verdict{Status: "regressed", CheckedAt: n, Reason: "target_quality_regressed"}
		}
		return passQuality(c, p, n, b)
	})
	g := qualityGW(t, "", provider)
	done := make(chan struct{})
	go func() { g.refreshAIQuality(context.Background()); close(done) }()
	<-started
	admin := httptest.NewServer(g.Admin())
	defer admin.Close()
	requireAIStatus(t, canaryCall(admin.Client(), "POST", admin.URL+"/v1/ai/canary/advance", "admin-secret", "advance-1", `{"expectedVersion":0,"targetPercent":5}`), 200)
	// Score fetch has no control lock: a migration hold can still drain.
	requireAIStatus(t, aiCall(admin.Client(), "POST", admin.URL+"/v1/writes/hold", "admin-secret", "", ""), 200)
	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("refresh did not terminate")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.canary.Version != 1 || g.canary.TargetPercent != 5 || g.canary.Verdict.Status != "pass" {
		t.Fatalf("old refresh overwrote advance: %+v", g.canary)
	}
}

func TestAIQualityCancellationPersistenceAndConfigurationFailures(t *testing.T) {
	provider := qualityProviderFunc(func(c context.Context, _ llmquality.Policy, n, _ time.Time) llmquality.Verdict {
		<-c.Done()
		return llmquality.Unknown(n, "cancelled")
	})
	g := qualityGW(t, "", provider)
	g.mu.Lock()
	g.canary.TargetPercent = 5
	g.canary.Version = 1
	g.canary.Verdict = passQuality(nil, llmquality.Policy{}, time.Now(), time.Time{})
	g.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() { g.runAIQuality(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancelled quality worker did not terminate")
	}
	g.mu.Lock()
	if g.canary.Version != 1 || g.canary.TargetPercent != 5 || g.canary.Verdict.Status != "pass" {
		t.Fatal("cancelled evaluation changed canary state")
	}
	g.mu.Unlock()
	file := filepath.Join(t.TempDir(), "state.json")
	g = qualityGW(t, file, qualityProviderFunc(passQuality))
	// Replace the state file with a directory to make atomic rename fail.
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(file, 0700); err != nil {
		t.Fatal(err)
	}
	admin := httptest.NewServer(g.Admin())
	defer admin.Close()
	requireAIStatus(t, canaryCall(admin.Client(), "POST", admin.URL+"/v1/ai/canary/advance", "admin-secret", "persist-fail", `{"expectedVersion":0,"targetPercent":5}`), 503)
	g.mu.Lock()
	if g.canary.Version != 0 || g.canary.TargetPercent != 0 {
		t.Fatal("failed persistence exposed increased local share")
	}
	g.mu.Unlock()
}

func TestAIQualityPolicyLoaderAndRestartCannotDisableGuard(t *testing.T) {
	file := filepath.Join(t.TempDir(), "state.json")
	g := qualityGW(t, file, qualityProviderFunc(passQuality))
	policyFile := filepath.Join(t.TempDir(), "policy.json")
	encoded, err := json.Marshal(g.cfg.AIQuality.Policy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(policyFile, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadAIQualityPolicy(policyFile); err != nil {
		t.Fatal(err)
	}
	encoded = append(encoded[:len(encoded)-1], []byte(`,"untrustedUrl":"https://other"}`)...)
	if err := os.WriteFile(policyFile, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadAIQualityPolicy(policyFile); err == nil {
		t.Fatal("unknown policy field accepted")
	}
	cfg := g.cfg
	cfg.AIQuality = nil
	if _, err := New(cfg); err == nil {
		t.Fatal("removed quality config bypassed persisted guard")
	}
	cfg = g.cfg
	copy := *cfg.AIQuality
	copy.Policy.App = "other"
	cfg.AIQuality = &copy
	if _, err := New(cfg); err == nil {
		t.Fatal("another app's scores can govern this gateway")
	}
}

func TestAIQualityLatePassingAdvanceCannotOverwriteNewerRevocationAtZero(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	provider := qualityProviderFunc(func(c context.Context, p llmquality.Policy, n, b time.Time) llmquality.Verdict {
		if calls.Add(1) == 1 {
			close(started)
			select {
			case <-release:
			case <-c.Done():
				return llmquality.Unknown(n, "cancelled")
			}
			return passQuality(c, p, n, b)
		}
		return llmquality.Verdict{Status: "regressed", Reason: "target_quality_regressed", CheckedAt: n}
	})
	g := qualityGW(t, "", provider)
	admin := httptest.NewServer(g.Admin())
	defer admin.Close()
	result := make(chan aiReply, 1)
	go func() {
		result <- canaryCall(admin.Client(), "POST", admin.URL+"/v1/ai/canary/advance", "admin-secret", "advance-old", `{"expectedVersion":0,"targetPercent":5}`)
	}()
	<-started
	g.refreshAIQuality(context.Background())
	close(release)
	select {
	case reply := <-result:
		requireAIStatus(t, reply, 412)
	case <-time.After(time.Second):
		t.Fatal("advance did not finish")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.canary.TargetPercent != 0 || g.canary.Verdict.Status != "regressed" {
		t.Fatalf("older pass overwrote newer revocation: %+v", g.canary)
	}
}

func TestAIQualityIneligibleTargetUsesSourceOnceBeforeAdmission(t *testing.T) {
	for _, mode := range []string{"unhealthy", "excluded", "missing"} {
		t.Run(mode, func(t *testing.T) {
			g := qualityGW(t, "", qualityProviderFunc(passQuality))
			g.mu.Lock()
			g.canary.TargetPercent = 100
			g.canary.Verdict = passQuality(nil, llmquality.Policy{}, time.Now(), time.Time{})
			switch mode {
			case "unhealthy":
				g.byName["laptop"].healthy.Store(false)
			case "excluded":
				g.readExclusions["laptop"] = "migration-lease"
			case "missing":
				delete(g.byName, "laptop")
			}
			g.mu.Unlock()
			public, admin := httptest.NewServer(g.Proxy()), httptest.NewServer(g.Admin())
			defer public.Close()
			defer admin.Close()
			reply := aiCall(public.Client(), "POST", public.URL+"/api/chat", "ai-secret", "pre-admission", aiTestBody)
			requireAIStatus(t, reply, 200)
			if reply.backend != "gcp" {
				t.Fatalf("unavailable target did not use source: %s", reply.backend)
			}
			stateReply := aiCall(admin.Client(), "GET", admin.URL+"/v1/ai/canary", "admin-secret", "", "")
			requireAIStatus(t, stateReply, 200)
			var state struct {
				EffectiveTargetPercent int `json:"effectiveTargetPercent"`
			}
			if json.Unmarshal(stateReply.body, &state) != nil || state.EffectiveTargetPercent != 0 {
				t.Fatalf("target reported effective while unavailable: %s", stateReply.body)
			}
			observed := aiObserve(t, admin.URL)
			if len(observed.Requests) != 1 || observed.Requests[0].Backend != "gcp" {
				t.Fatalf("request was replayed: %+v", observed.Requests)
			}
			g.mu.Lock()
			g.byName["gcp"].healthy.Store(false)
			g.mu.Unlock()
			requireAIStatus(t, aiCall(public.Client(), "POST", public.URL+"/api/chat", "ai-secret", "no-eligible-backend", aiTestBody), 503)
		})
	}
}

func directCanaryAdvance(g *Gateway, body, key string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "http://admin/v1/ai/canary/advance", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer admin-secret")
	r.Header.Set("Idempotency-Key", key)
	w := httptest.NewRecorder()
	g.Admin().ServeHTTP(w, r)
	return w
}

func TestAIQualitySlowEvaluationDoesNotCountAsNewStageObservation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		provider := qualityProviderFunc(func(c context.Context, p llmquality.Policy, n, b time.Time) llmquality.Verdict {
			time.Sleep(5 * time.Second)
			return passQuality(c, p, n, b)
		})
		g := qualityGW(t, "", provider)
		first := directCanaryAdvance(g, `{"expectedVersion":0,"targetPercent":5}`, "slow-evaluation")
		if first.Code != 200 {
			t.Fatalf("first: %d %s", first.Code, first.Body)
		}
		g.mu.Lock()
		at := g.canary.LastAdvanceAt
		g.mu.Unlock()
		if !at.Equal(time.Now().UTC()) {
			t.Fatalf("query time reused as activation: %v; now %v", at, time.Now())
		}
		time.Sleep(26 * time.Second) // 31s since query, only 26s since activation
		second := directCanaryAdvance(g, `{"expectedVersion":1,"targetPercent":25}`, "advance-too-soon")
		if second.Code != 409 || !strings.Contains(second.Body.String(), "canary_soak_incomplete") {
			t.Fatalf("query time counted as soak: %d %s", second.Code, second.Body)
		}
	})
}

func TestAIQualityExpiresWhileWaitingForControl(t *testing.T) {
	// Mutex waiters are not durably blocked in synctest. Use a short real
	// expiration here; no network or inference runs while control is locked.
	evaluated := make(chan struct{})
	provider := qualityProviderFunc(func(c context.Context, p llmquality.Policy, n, b time.Time) llmquality.Verdict {
		v := passQuality(c, p, n, b)
		v.ValidUntil = n.Add(200 * time.Millisecond)
		close(evaluated)
		return v
	})
	g := qualityGW(t, "", provider)
	g.controlMu.Lock()
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- directCanaryAdvance(g, `{"expectedVersion":0,"targetPercent":5}`, "expired-in-control")
	}()
	<-evaluated
	time.Sleep(250 * time.Millisecond)
	g.controlMu.Unlock()
	w := <-done
	if w.Code != 412 {
		t.Fatalf("expired evidence published: %d %s", w.Code, w.Body)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.canary.TargetPercent != 0 || g.canary.Version != 0 {
		t.Fatal("expired decision advanced stage")
	}
}
