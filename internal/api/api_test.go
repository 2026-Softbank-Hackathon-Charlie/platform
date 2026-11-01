package api

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/engine"
	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/plans"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/runs"
)

const token = "test-token"

// fakeEngine emits a step event when started, then waits for finish().
type fakeEngine struct {
	id     string
	emit   func(engine.Event)
	finish chan string // outcome to end with
	mu     sync.Mutex
	st     engine.State
}

func (f *fakeEngine) RunID() string { return f.id }
func (f *fakeEngine) Close()        {}
func (f *fakeEngine) State() engine.State {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.st
}
func (f *fakeEngine) Run(ctx context.Context) error {
	f.emit(engine.Event{RunID: f.id, Step: engine.StepPrecheck, Kind: engine.EventStepStarted})
	select {
	case out := <-f.finish:
		f.mu.Lock()
		f.st.Outcome = out
		f.mu.Unlock()
		if out == engine.OutcomeSucceeded {
			f.emit(engine.Event{RunID: f.id, Kind: engine.EventDone, Message: "migration succeeded"})
		}
	case <-ctx.Done():
	}
	return nil
}

type env struct {
	t       *testing.T
	srv     *httptest.Server
	mu      sync.Mutex
	engines []*fakeEngine
	failNew bool
}

func newEnv(t *testing.T) *env {
	t.Setenv("SRC_PGPASSWORD", "TOPSECRET")
	e := &env{t: t}
	factory := func(p *plan.Plan, o engine.Options) (runs.Engine, error) {
		e.mu.Lock()
		defer e.mu.Unlock()
		if e.failNew {
			return nil, io.ErrUnexpectedEOF
		}
		f := &fakeEngine{id: p.ID + "-run" + string(rune('0'+len(e.engines))), emit: o.Emit, finish: make(chan string, 1)}
		f.st = engine.State{RunID: f.id, PlanID: p.ID, Outcome: engine.OutcomeRunning, CreatedAt: time.Now()}
		e.engines = append(e.engines, f)
		return f, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s := New(Config{Token: token}, plans.New(plans.DefaultPolicy()), runs.New(ctx, t.TempDir(), factory))
	e.srv = httptest.NewServer(s)
	t.Cleanup(e.srv.Close)
	return e
}

func (e *env) engine(i int) *fakeEngine {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.engines[i]
}

func (e *env) do(method, path, body string, hdr ...string) (int, map[string]any) {
	e.t.Helper()
	req, _ := http.NewRequest(method, e.srv.URL+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(raw), "TOPSECRET") {
		e.t.Fatalf("%s %s leaked a secret: %s", method, path, raw)
	}
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	return resp.StatusCode, m
}

func planBody(id, srcCluster string) string {
	return `{"id":"` + id + `",
	  "source":{"env":"gke","context":"gke","namespace":"shop","cluster":"` + srcCluster + `","dsn":"postgres://app:${SRC_PGPASSWORD}@db/app"},
	  "target":{"env":"eks","context":"eks","namespace":"shop","cluster":"` + srcCluster + `-eks","dsn":"postgres://app:x@db/app"}}`
}

// submitApproved returns the ref of an approved plan.
func (e *env) submitApproved(id, cluster string) string {
	e.t.Helper()
	code, rec := e.do("POST", "/v1/plans", planBody(id, cluster))
	if code != 201 {
		e.t.Fatalf("submit: %d %v", code, rec)
	}
	ref := rec["ref"].(string)
	if code, rec := e.do("POST", "/v1/plans/"+ref+"/approve", ""); code != 200 || rec["status"] != "approved" {
		e.t.Fatalf("approve: %d %v", code, rec)
	}
	return ref
}

func errCode(m map[string]any) string {
	if e, ok := m["error"].(map[string]any); ok {
		return e["code"].(string)
	}
	return ""
}

func TestAuth(t *testing.T) {
	e := newEnv(t)
	for _, h := range []string{"", "Bearer wrong", "Basic " + token} {
		req, _ := http.NewRequest("GET", e.srv.URL+"/v1/plans", nil)
		if h != "" {
			req.Header.Set("Authorization", h)
		}
		resp, _ := http.DefaultClient.Do(req)
		resp.Body.Close()
		if resp.StatusCode != 401 {
			t.Errorf("Authorization %q: status %d", h, resp.StatusCode)
		}
	}
	resp, _ := http.Get(e.srv.URL + "/healthz")
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("healthz must not require auth: %d", resp.StatusCode)
	}
}

func TestPlanSubmitAndApprove(t *testing.T) {
	e := newEnv(t)
	code, rec := e.do("POST", "/v1/plans", planBody("p1", "db"))
	if code != 201 || rec["status"] != "pending_approval" {
		t.Fatalf("%d %v", code, rec)
	}
	if _, has := rec["raw"]; has {
		t.Fatal("raw plan must not be returned")
	}
	ref := rec["ref"].(string)
	if code, rec := e.do("GET", "/v1/plans/"+ref, ""); code != 200 || rec["summary"].(map[string]any)["planId"] != "p1" {
		t.Fatalf("get: %d %v", code, rec)
	}
	if code, m := e.do("POST", "/v1/plans", `{"id":"x"}`); code != 400 || errCode(m) != "invalid_plan" {
		t.Fatalf("invalid plan: %d %v", code, m)
	}
	if code, m := e.do("GET", "/v1/plans/pln_nope", ""); code != 404 || errCode(m) != "plan_not_found" {
		t.Fatalf("missing plan: %d %v", code, m)
	}
	if code, m := e.do("POST", "/v1/plans", strings.Repeat("x", MaxPlanBytes+1)); code != 413 {
		t.Fatalf("oversized: %d %v", code, m)
	}
}

func TestStartRequiresApproval(t *testing.T) {
	e := newEnv(t)
	_, rec := e.do("POST", "/v1/plans", planBody("p1", "db"))
	code, m := e.do("POST", "/v1/migrations", `{"planRef":"`+rec["ref"].(string)+`"}`)
	if code != 409 || errCode(m) != "plan_state_conflict" {
		t.Fatalf("%d %v", code, m)
	}
	for _, body := range []string{`{}`, `not json`, `{"planRef":"x","extra":1}`} {
		if code, m := e.do("POST", "/v1/migrations", body); code != 400 || errCode(m) != "invalid_request" {
			t.Errorf("body %s: %d %v", body, code, m)
		}
	}
}

func TestStartIsAcceptedAndIdempotent(t *testing.T) {
	e := newEnv(t)
	ref := e.submitApproved("p1", "db")
	body := `{"planRef":"` + ref + `"}`

	code, m := e.do("POST", "/v1/migrations", body, "Idempotency-Key", "k1")
	if code != 202 || m["runId"] == "" || m["links"].(map[string]any)["events"] == nil {
		t.Fatalf("start: %d %v", code, m)
	}
	runID := m["runId"].(string)

	// Retry with the same key → same run, no second engine.
	code, m = e.do("POST", "/v1/migrations", body, "Idempotency-Key", "k1")
	if code != 202 || m["runId"] != runID {
		t.Fatalf("retry: %d %v", code, m)
	}
	// Same plan without the key → already started.
	if code, m := e.do("POST", "/v1/migrations", body); code != 409 || errCode(m) != "plan_state_conflict" {
		t.Fatalf("restart: %d %v", code, m)
	}
	// Key reused for another plan → conflict.
	other := e.submitApproved("p2", "other")
	if code, m := e.do("POST", "/v1/migrations", `{"planRef":"`+other+`"}`, "Idempotency-Key", "k1"); code != 409 || errCode(m) != "idempotency_key_reused" {
		t.Fatalf("reused key: %d %v", code, m)
	}
	e.mu.Lock()
	n := len(e.engines)
	e.mu.Unlock()
	if n != 1 {
		t.Fatalf("%d engines created, want 1", n)
	}

	code, st := e.do("GET", "/v1/migrations/"+runID, "")
	if code != 200 || st["outcome"] != "running" || st["planId"] != "p1" {
		t.Fatalf("get run: %d %v", code, st)
	}
	_, plan := e.do("GET", "/v1/plans/"+ref, "")
	if plan["status"] != "started" || plan["runId"] != runID {
		t.Fatalf("plan after start: %v", plan)
	}
}

func TestSameClusterIsBusy(t *testing.T) {
	e := newEnv(t)
	a := e.submitApproved("p1", "db")
	b := e.submitApproved("p2", "db")
	if code, _ := e.do("POST", "/v1/migrations", `{"planRef":"`+a+`"}`); code != 202 {
		t.Fatal(code)
	}
	code, m := e.do("POST", "/v1/migrations", `{"planRef":"`+b+`"}`)
	if code != 409 || errCode(m) != "cluster_busy" {
		t.Fatalf("%d %v", code, m)
	}
	// The rejected plan stays approved and can start once the first run ends.
	if _, p := e.do("GET", "/v1/plans/"+b, ""); p["status"] != "approved" {
		t.Fatalf("plan b = %v", p["status"])
	}
	e.engine(0).finish <- engine.OutcomeSucceeded
	waitFor(t, func() bool { _, st := e.do("GET", "/v1/migrations/p1-run0", ""); return st["outcome"] == "succeeded" })
	waitFor(t, func() bool { c, _ := e.do("POST", "/v1/migrations", `{"planRef":"`+b+`"}`); return c == 202 })
}

func TestEngineCreationFailureReleasesPlan(t *testing.T) {
	e := newEnv(t)
	ref := e.submitApproved("p1", "db")
	e.failNew = true
	code, m := e.do("POST", "/v1/migrations", `{"planRef":"`+ref+`"}`)
	if code != 502 || errCode(m) != "engine_init_failed" || strings.Contains(m["error"].(map[string]any)["message"].(string), "EOF") {
		t.Fatalf("%d %v", code, m)
	}
	if _, p := e.do("GET", "/v1/plans/"+ref, ""); p["status"] != "approved" {
		t.Fatalf("plan not released: %v", p["status"])
	}
}

func TestRunNotFound(t *testing.T) {
	e := newEnv(t)
	for _, p := range []string{"/v1/migrations/nope", "/v1/migrations/nope/events"} {
		if code, m := e.do("GET", p, ""); code != 404 || errCode(m) != "run_not_found" {
			t.Errorf("%s: %d %v", p, code, m)
		}
	}
}

func TestEventStream(t *testing.T) {
	e := newEnv(t)
	ref := e.submitApproved("p1", "db")
	_, m := e.do("POST", "/v1/migrations", `{"planRef":"`+ref+`"}`)
	runID := m["runId"].(string)

	req, _ := http.NewRequest("GET", e.srv.URL+"/v1/migrations/"+runID+"/events", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type %q", ct)
	}

	names := make(chan string, 16)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			if n, ok := strings.CutPrefix(sc.Text(), "event: "); ok {
				names <- n
			}
		}
		close(names)
	}()
	if n := <-names; n != "snapshot" {
		t.Fatalf("first event %q, want snapshot", n)
	}
	e.engine(0).finish <- engine.OutcomeSucceeded

	var got []string
	timeout := time.After(5 * time.Second)
	for {
		select {
		case n, ok := <-names:
			if !ok {
				if len(got) == 0 || got[len(got)-1] != "snapshot" {
					t.Fatalf("stream must end with a snapshot, got %v", got)
				}
				if !contains(got, engine.EventDone) {
					t.Fatalf("done event missing: %v", got)
				}
				return
			}
			got = append(got, n)
		case <-timeout:
			t.Fatalf("stream did not end; got %v", got)
		}
	}
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met")
}
