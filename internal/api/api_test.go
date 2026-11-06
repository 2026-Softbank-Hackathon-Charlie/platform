package api

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/legacy"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/engine"
	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"
	specdoc "github.com/2026-Softbank-Hackathon-Charlie/platform/api"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/auth"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/plans"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/runs"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store/storetest"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

const (
	opToken     = "operator-token-0123456789"
	agentToken  = "agent-token-0123456789ab"
	viewerToken = "viewer-token-0123456789a"
	gatewayURL  = "http://gateway.test:9901"
)

func planJSON(id, srcCtx, dstCtx string, gatewayHTTP bool) string {
	gw := ""
	if gatewayHTTP {
		gw = `,"gateway":{"mode":"http","url":"` + gatewayURL + `","token":"${GATEWAY_TOKEN}"}`
	}
	return `{"id":"` + id + `",
	"source":{"env":"gke","context":"` + srcCtx + `","namespace":"shop","cluster":"db-gke","dsn":"postgres://app:${SRC_PGPASSWORD}@db-gke.shop.ts.net/app"},
	"target":{"env":"eks","context":"` + dstCtx + `","namespace":"shop","cluster":"db-eks","dsn":"postgres://app:${DST_PGPASSWORD}@db-eks.shop.ts.net/app"}` + gw + `}`
}

// --- fake engine (tests only) ---

type fakeEngine struct {
	mu     sync.Mutex
	id     string
	emit   func(engine.Event)
	st     engine.State
	failAt string                   // this step fails
	block  map[string]chan struct{} // these steps wait until the channel is closed

	halt bool // a failure halts the run (promotion was requested)
	// resumeErr is what ResumeHalted answers; resumed counts its calls.
	resumeErr error
	resumed   int
}

func newFakeState(id, planID string) engine.State {
	st := engine.State{RunID: id, PlanID: planID, Outcome: engine.OutcomeRunning, Steps: map[string]*engine.StepRecord{}, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	for _, s := range engine.Steps {
		st.Steps[s] = &engine.StepRecord{Status: engine.StatusPending}
	}
	return st
}

func (f *fakeEngine) RunID() string { return f.id }
func (f *fakeEngine) Close()        {}

// State returns a shallow copy, like the real engine: the step records stay
// shared with the goroutine running a step, so -race catches a runner that
// reads them from anywhere else.
func (f *fakeEngine) State() engine.State {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.st
}

// deepState is for tests that inspect an engine owned by a finished run.
func (f *fakeEngine) deepState() engine.State {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := f.st
	st.Steps = map[string]*engine.StepRecord{}
	for k, v := range f.st.Steps {
		c := *v
		st.Steps[k] = &c
	}
	return st
}

func (f *fakeEngine) send(step, kind string, data map[string]any) {
	f.emit(engine.Event{Time: time.Now().UTC(), RunID: f.id, Step: step, Kind: kind, Data: data})
}

func (f *fakeEngine) RunStep(ctx context.Context, name string) error {
	f.mu.Lock()
	if f.st.Outcome == engine.OutcomeFailed {
		f.st.Outcome = engine.OutcomeRunning
	}
	halted := f.st.Outcome == engine.OutcomeHalted
	f.mu.Unlock()
	if halted { // like the engine: only ResumeHalted continues a halted run
		return fmt.Errorf("run %s is halted; start a new run", f.id)
	}
	return f.step(ctx, name)
}

func (f *fakeEngine) ResumeHalted(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resumed++
	if f.st.Outcome != engine.OutcomeHalted {
		return fmt.Errorf("run %s is %s, not halted", f.id, f.st.Outcome)
	}
	if f.resumeErr != nil {
		return f.resumeErr
	}
	f.st.Outcome, f.st.FailureStep, f.st.FailureError = engine.OutcomeRunning, "", ""
	return nil
}

func (f *fakeEngine) step(ctx context.Context, name string) error {
	f.mu.Lock()
	f.st.Steps[name].Status = engine.StatusRunning
	ch := f.block[name]
	f.mu.Unlock()
	f.send(name, engine.EventStepStarted, nil)
	if ch != nil {
		select {
		case <-ch:
		case <-ctx.Done():
			return f.fail(name, ctx.Err())
		}
	}
	if name == f.failAt {
		return f.fail(name, errors.New("boom at "+name))
	}
	f.mu.Lock()
	rec := f.st.Steps[name]
	rec.Status = engine.StatusSucceeded
	switch name {
	case engine.StepFence:
		f.st.WritesHeld = true
	case engine.StepCutover:
		f.st.DowntimeMillis, f.st.TrafficOnTarget, f.st.WritesHeld = 1234, true, false
	case engine.StepVerify:
		rec.Evidence = map[string]any{"report": map[string]any{"ok": true, "missingRows": 0, "written": map[string]any{"checked": 10, "missing": 0}}}
	case engine.StepFinalize:
		f.st.Outcome = engine.OutcomeSucceeded
	}
	f.mu.Unlock()
	if name == engine.StepCutover {
		f.send(name, engine.EventMetric, map[string]any{"downtimeMillis": 1234})
	}
	f.send(name, engine.EventStepSucceeded, nil)
	return nil
}

func (f *fakeEngine) fail(name string, err error) error {
	f.mu.Lock()
	f.st.Steps[name].Status = engine.StatusFailed
	f.st.Steps[name].Error = err.Error()
	f.st.FailureStep, f.st.FailureError = name, err.Error()
	if f.halt {
		f.st.Outcome, f.st.PromoteRequested = engine.OutcomeHalted, true
	} else if f.st.WritesHeld {
		f.st.Outcome = engine.OutcomeRolledBack
	} else {
		f.st.Outcome = engine.OutcomeFailed
	}
	f.mu.Unlock()
	f.send(name, engine.EventStepFailed, nil)
	return err
}

// --- fake gateway and environments (tests only) ---

type fakeTraffic struct {
	mu      sync.Mutex
	stats   map[string]apiv1.BackendStats
	totals  map[string]apiv1.BackendTotals
	weights []map[string]int
	write   string
}

func (f *fakeTraffic) State(context.Context, time.Duration) (apiv1.TrafficState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rw := map[string]int{f.write: 100}
	if n := len(f.weights); n > 0 {
		rw = f.weights[n-1]
	}
	return apiv1.TrafficState{
		WriteBackend: f.write, ReadWeights: rw, MaxHold: "3m0s", Backends: []apiv1.BackendState{},
		Stats: f.stats, Totals: f.totals, UpdatedAt: time.Now().UTC(),
	}, nil
}

func (f *fakeTraffic) SetReadWeights(_ context.Context, w map[string]int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.weights = append(f.weights, w)
	return nil
}

var errNoEnv = errors.New("no such env")

type fakeEnvs struct{ list []apiv1.Environment }

func (f fakeEnvs) List(context.Context) []apiv1.Environment {
	return append([]apiv1.Environment(nil), f.list...)
}
func (f fakeEnvs) Get(_ context.Context, name string) (apiv1.Environment, error) {
	for _, e := range f.list {
		if e.Name == name {
			return e, nil
		}
	}
	return apiv1.Environment{}, errNoEnv
}
func (f fakeEnvs) Names() []string {
	var out []string
	for _, e := range f.list {
		out = append(out, e.Name)
	}
	return out
}

func env(name string, p apiv1.Provider) apiv1.Environment {
	return apiv1.Environment{Name: name, Provider: p, DisplayName: name, KubeContext: name + "-ctx", GatewayBackend: name,
		Status: apiv1.EnvironmentStatus{Reachable: true, CheckedAt: time.Now().UTC()}}
}

// --- harness ---

type harness struct {
	t       *testing.T
	srv     *httptest.Server
	store   store.Store
	runner  *runs.Runner
	traffic *fakeTraffic
	router  routers.Router
	spec    *openapi3.T

	mu      sync.Mutex
	engines map[string]*fakeEngine // by run id; resumed runs reuse the state
	setup   func(*fakeEngine)      // configures the next new engine
	newErr  error
	n       int

	webhookSecret string           // Config.GitHubWebhookSecret
	gitOpsRepo    string           // Config.GitOpsRepo
	admins        *auth.Admins     // Config.Admins
	appDBTopology bool             // Config.AppDBTopology, with gke and eks as Config.AppDBEnvs
	now           func() time.Time // Server.now; nil keeps the real clock
	api           *Server          // the server behind srv
}

type option func(*harness, *runs.Config, *Deps)

func withoutGateway() option {
	return func(h *harness, rc *runs.Config, d *Deps) { rc.Traffic, d.Traffic, h.traffic = nil, nil, nil }
}

func newHarness(t *testing.T, opts ...option) *harness {
	t.Helper()
	t.Setenv("GATEWAY_TOKEN", "gw-token")
	h := &harness{t: t, store: storetest.NewMemory(), engines: map[string]*fakeEngine{}}
	h.traffic = &fakeTraffic{
		write:  "gke",
		stats:  map[string]apiv1.BackendStats{},
		totals: map[string]apiv1.BackendTotals{"gke": {Requests: 100, Errors: 5}, "eks": {Requests: 0, Errors: 0}},
	}

	spec, err := openapi3.NewLoader().LoadFromData(specdoc.OpenAPI)
	if err != nil {
		t.Fatal(err)
	}
	if err := spec.Validate(context.Background()); err != nil {
		t.Fatalf("openapi.yaml is invalid: %v", err)
	}
	spec.Servers = openapi3.Servers{{URL: "/"}}
	h.spec = spec
	if h.router, err = legacy.NewRouter(spec); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	rc := runs.Config{StateDir: t.TempDir(), Store: h.store, Factory: h.factory, Traffic: h.traffic, PollInterval: 10 * time.Millisecond, MinCanaryStep: 10 * time.Millisecond}
	policy := plans.DefaultPolicy()
	policy.GatewayURL = gatewayURL
	policy.Buckets = []string{"gcs:moimroom-photos-gke", "s3:moimroom-photos-eks"}
	d := Deps{
		Plans:       plans.New(policy, h.store),
		Store:       h.store,
		Envs:        fakeEnvs{list: []apiv1.Environment{env("gke", apiv1.ProviderGcp), env("eks", apiv1.ProviderAws), env("onprem", apiv1.ProviderOnprem)}},
		Traffic:     h.traffic,
		EnvNotFound: errNoEnv,
	}
	for _, o := range opts {
		o(h, &rc, &d)
	}
	h.runner = runs.New(ctx, rc)
	d.Runs = h.runner

	tokens := &auth.Tokens{}
	tokens.Add("op", apiv1.RoleOperator, opToken)
	tokens.Add("ai", apiv1.RoleAgent, agentToken)
	tokens.Add("view", apiv1.RoleViewer, viewerToken)
	api := New(Config{Tokens: tokens, Admins: h.admins, CORSOrigins: []string{"http://console.test"}, GitHubWebhookSecret: h.webhookSecret, GitOpsRepo: h.gitOpsRepo, AppDBTopology: h.appDBTopology, AppDBEnvs: []string{"gke", "eks"}}, d)
	if h.now != nil {
		api.now = h.now
	}
	h.api = api
	h.srv = httptest.NewServer(api)
	t.Cleanup(func() {
		h.srv.Close()
		cancel()
		wctx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		_ = h.runner.Wait(wctx)
	})
	return h
}

func (h *harness) factory(p *plan.Plan, o engine.Options) (runs.Engine, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.newErr != nil {
		return nil, h.newErr
	}
	if o.RunID != "" { // resume: same state, new engine
		old, ok := h.engines[o.RunID]
		if !ok {
			return nil, fmt.Errorf("run %s not found", o.RunID)
		}
		f := &fakeEngine{id: o.RunID, emit: o.Emit, st: old.deepState(), block: map[string]chan struct{}{}}
		if h.setup != nil {
			h.setup(f)
			h.setup = nil
		}
		h.engines[o.RunID] = f
		return f, nil
	}
	h.n++
	id := fmt.Sprintf("%s-%d", p.ID, h.n)
	f := &fakeEngine{id: id, emit: o.Emit, st: newFakeState(id, p.ID), block: map[string]chan struct{}{}}
	if h.setup != nil {
		h.setup(f)
		h.setup = nil
	}
	h.engines[id] = f
	return f, nil
}

// do sends a request and validates the response against openapi.yaml.
func (h *harness) do(method, path, token string, body any, hdr ...string) (int, []byte) {
	h.t.Helper()
	var rd io.Reader
	var raw []byte
	switch b := body.(type) {
	case nil:
	case string:
		raw = []byte(b)
	default:
		raw, _ = json.Marshal(b)
	}
	if raw != nil {
		rd = bytes.NewReader(raw)
	}
	req, _ := http.NewRequest(method, h.srv.URL+path, rd)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if raw != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	h.validate(req, resp, out)
	return resp.StatusCode, out
}

func (h *harness) validate(req *http.Request, resp *http.Response, body []byte) {
	h.t.Helper()
	vreq := req.Clone(context.Background())
	vreq.URL.Host, vreq.URL.Scheme, vreq.Host = "", "", ""
	route, params, err := h.router.FindRoute(vreq)
	if err != nil {
		h.t.Fatalf("%s %s is not in openapi.yaml: %v", req.Method, req.URL.Path, err)
	}
	in := &openapi3filter.ResponseValidationInput{
		RequestValidationInput: &openapi3filter.RequestValidationInput{Request: vreq, PathParams: params, Route: route},
		Status:                 resp.StatusCode,
		Header:                 resp.Header,
		Body:                   io.NopCloser(bytes.NewReader(body)),
		Options:                &openapi3filter.Options{IncludeResponseStatus: true},
	}
	if err := openapi3filter.ValidateResponse(context.Background(), in); err != nil {
		h.t.Fatalf("%s %s -> %d does not match openapi.yaml: %v\nbody: %s", req.Method, req.URL.Path, resp.StatusCode, err, body)
	}
}

// validateSchema checks an SSE payload against a component schema.
func (h *harness) validateSchema(name string, data []byte) {
	h.t.Helper()
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		h.t.Fatal(err)
	}
	if err := h.spec.Components.Schemas[name].Value.VisitJSON(v); err != nil {
		h.t.Fatalf("%s payload does not match openapi.yaml: %v\n%s", name, err, data)
	}
}

func decode[T any](t *testing.T, b []byte) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("decode %T: %v\n%s", v, err, b)
	}
	return v
}

func (h *harness) submit(body string) apiv1.PlanRecord {
	h.t.Helper()
	code, b := h.do("POST", "/v1/plans", agentToken, body)
	if code != 201 {
		h.t.Fatalf("submit: %d %s", code, b)
	}
	return decode[apiv1.PlanRecord](h.t, b)
}

func (h *harness) approved(body string) string {
	h.t.Helper()
	rec := h.submit(body)
	// The agent submitted it, so approval needs a passing rehearsal report
	// for this body, as the AI runner uploads (platform #24).
	rev := h.rehearsed(rec.Ref, body, "")
	if code, b := h.do("POST", "/v1/plans/"+rec.Ref+"/approve", opToken, map[string]any{"comment": "looks good", "revision": rev}); code != 200 {
		h.t.Fatalf("approve: %d %s", code, b)
	}
	return rec.Ref
}

// rehearsed uploads a passing rehearsal report for the plan body and target
// manifest (hex SHA-256, "" when the plan has none) and returns the revision.
func (h *harness) rehearsed(ref, body, manifestSHA string) int64 {
	h.t.Helper()
	return h.rehearsedAs(agentToken, ref, body, manifestSHA)
}

// rehearsedAs is rehearsed with the token of the principal that submitted the
// plan: only that principal's report counts.
func (h *harness) rehearsedAs(token, ref, body, manifestSHA string) int64 {
	h.t.Helper()
	sum := sha256.Sum256([]byte(body))
	rep := fmt.Sprintf(`{"schemaVersion":"hopcharlie.rehearsal/v1","passed":true,"candidate":{"planSha256":%q,"manifestSha256":%q}}`, hex.EncodeToString(sum[:]), manifestSHA)
	code, b := h.do("PUT", "/v1/plans/"+ref+"/attachments/rehearsal-report.json", token, rep)
	if code != 200 && code != 201 {
		h.t.Fatalf("upload rehearsal report: %d %s", code, b)
	}
	code, b = h.do("GET", "/v1/plans/"+ref, opToken, nil)
	if code != 200 {
		h.t.Fatalf("get plan: %d %s", code, b)
	}
	return decode[apiv1.PlanRecord](h.t, b).Revision
}

func (h *harness) start(req apiv1.StartMigrationRequest) string {
	h.t.Helper()
	code, b := h.do("POST", "/v1/migrations", opToken, req)
	if code != 202 {
		h.t.Fatalf("start: %d %s", code, b)
	}
	return decode[apiv1.StartMigrationResponse](h.t, b).RunId
}

func (h *harness) waitRun(id string, ok func(apiv1.Run) bool) apiv1.Run {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		code, b := h.do("GET", "/v1/migrations/"+id, viewerToken, nil)
		if code != 200 {
			h.t.Fatalf("get run: %d %s", code, b)
		}
		run := decode[apiv1.Run](h.t, b)
		if ok(run) {
			return run
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("run %s did not reach the expected state; last: status=%s phase=%s reason=%v", id, run.Status, run.Phase, deref(run.StatusReason))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func status(s apiv1.RunStatus) func(apiv1.Run) bool {
	return func(r apiv1.Run) bool { return r.Status == s }
}

func ended(s apiv1.RunStatus) func(apiv1.Run) bool {
	return func(r apiv1.Run) bool { return r.Status == s && r.EndedAt != nil }
}

func errCode(t *testing.T, b []byte) string {
	t.Helper()
	return decode[apiv1.Error](t, b).Error.Code
}

// --- tests ---

func TestRolesAndAudit(t *testing.T) {
	h := newHarness(t)
	if code, _ := h.do("GET", "/v1/plans", "", nil); code != 401 {
		t.Fatalf("no token: %d", code)
	}
	if code, b := h.do("POST", "/v1/plans", viewerToken, planJSON("p1", "a", "b", false)); code != 403 || errCode(t, b) != "forbidden" {
		t.Fatalf("viewer submit: %d %s", code, b)
	}
	rec := h.submit(planJSON("p1", "a", "b", false))
	if rec.SubmittedBy != "ai" || rec.Status != apiv1.PlanStatusPendingApproval {
		t.Fatalf("record: %+v", rec)
	}
	if code, _ := h.do("POST", "/v1/plans/"+rec.Ref+"/approve", agentToken, nil); code != 403 {
		t.Fatalf("the AI agent must not approve: %d", code)
	}
	code, b := h.do("GET", "/v1/me", agentToken, nil)
	if me := decode[apiv1.Principal](t, b); code != 200 || me.Name != "ai" || me.Role != apiv1.RoleAgent {
		t.Fatalf("me: %d %s", code, b)
	}

	_, b = h.do("GET", "/v1/audit", viewerToken, nil)
	list := decode[apiv1.AuditList](t, b)
	var got []string
	for _, e := range list.Entries {
		got = append(got, fmt.Sprintf("%s %s %d", e.Actor, e.Action, e.Status))
	}
	want := []string{
		"ai POST /v1/plans/{ref}/approve 403",
		"ai POST /v1/plans 201",
		"view POST /v1/plans 403",
		"anonymous GET /v1/plans 401",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("audit:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if d := list.Entries[1].Detail; d == nil || (*d)["planRef"] != rec.Ref {
		t.Fatalf("submit audit detail: %v", d)
	}
}

func TestRejectedPlanNeverStarts(t *testing.T) {
	h := newHarness(t)
	rec := h.submit(planJSON("p1", "a", "b", false))
	if code, b := h.do("POST", "/v1/plans/"+rec.Ref+"/reject", opToken, map[string]string{}); code != 400 {
		t.Fatalf("reject without comment: %d %s", code, b)
	}
	code, b := h.do("POST", "/v1/plans/"+rec.Ref+"/reject", opToken, map[string]string{"comment": "wrong target"})
	if r := decode[apiv1.PlanRecord](t, b); code != 200 || r.Status != apiv1.PlanStatusRejected || deref(r.ReviewedBy) != "op" {
		t.Fatalf("reject: %d %s", code, b)
	}
	if code, b := h.do("POST", "/v1/plans/"+rec.Ref+"/approve", opToken, nil); code != 409 || errCode(t, b) != "plan_state_conflict" {
		t.Fatalf("approve after reject: %d %s", code, b)
	}
	if code, b := h.do("POST", "/v1/migrations", opToken, apiv1.StartMigrationRequest{PlanRef: rec.Ref}); code != 409 {
		t.Fatalf("start rejected: %d %s", code, b)
	}
}

func TestMigrationWaitsForSwitch(t *testing.T) {
	h := newHarness(t)
	ref := h.approved(planJSON("p1", "gke-ctx", "eks-ctx", false))
	id := h.start(apiv1.StartMigrationRequest{PlanRef: ref})

	run := h.waitRun(id, status(apiv1.RunStatusAwaitingSwitch))
	if run.Phase != apiv1.RunPhaseAwaitingSwitch || run.StartedBy != "op" {
		t.Fatalf("waiting run: %+v", run)
	}
	for _, s := range []string{"precheck", "prepare", "presync", "delta"} {
		if run.Engine.Steps[s].Status != apiv1.StepStatusSucceeded {
			t.Fatalf("step %s = %s while waiting", s, run.Engine.Steps[s].Status)
		}
	}
	if run.Engine.Steps["fence"].Status != apiv1.StepStatusPending {
		t.Fatal("fence must not run before the switch is approved")
	}
	if code, b := h.do("POST", "/v1/migrations/"+id+"/switch", agentToken, nil); code != 403 {
		t.Fatalf("agent switch: %d %s", code, b)
	}

	h.traffic.mu.Lock()
	h.traffic.totals = map[string]apiv1.BackendTotals{"gke": {Requests: 250, Errors: 5}, "eks": {Requests: 50, Errors: 0}}
	h.traffic.mu.Unlock()
	if code, b := h.do("POST", "/v1/migrations/"+id+"/switch", opToken, nil); code != 200 {
		t.Fatalf("switch: %d %s", code, b)
	}
	run = h.waitRun(id, ended(apiv1.RunStatusSucceeded))
	r := run.Result
	if run.Phase != apiv1.RunPhaseDone || deref(run.SwitchApprovedBy) != "op" {
		t.Fatalf("done run: %+v", run)
	}
	if r.DowntimeMillis == nil || *r.DowntimeMillis != 1234 || r.Verified == nil || !*r.Verified || *r.MissingRows != 0 || *r.MissingWrites != 0 {
		t.Fatalf("result: %+v", r)
	}
	if r.Requests == nil || *r.Requests != 200 || *r.FailedRequests != 0 || r.DurationMillis == nil {
		t.Fatalf("traffic result: requests=%v failed=%v", r.Requests, r.FailedRequests)
	}

	_, b := h.do("GET", "/v1/plans/"+ref, viewerToken, nil)
	if p := decode[apiv1.PlanRecord](t, b); p.Status != apiv1.PlanStatusStarted || deref(p.RunId) != id {
		t.Fatalf("plan after start: %+v", p)
	}
	_, b = h.do("GET", "/v1/matrix", viewerToken, nil)
	m := decode[apiv1.Matrix](t, b)
	if len(m.Routes) != 6 {
		t.Fatalf("matrix routes = %d", len(m.Routes))
	}
	for _, row := range m.Routes {
		has := row.RunId != nil
		if (row.Source == "gke" && row.Target == "eks") != has {
			t.Fatalf("matrix row %s->%s has run = %v", row.Source, row.Target, has)
		}
	}
}

func TestSwitchOnlyWhileWaiting(t *testing.T) {
	h := newHarness(t)
	block := make(chan struct{})
	h.setup = func(f *fakeEngine) { f.block["presync"] = block }
	ref := h.approved(planJSON("p1", "a", "b", false))
	id := h.start(apiv1.StartMigrationRequest{PlanRef: ref})
	if code, b := h.do("POST", "/v1/migrations/"+id+"/switch", opToken, nil); code != 409 || errCode(t, b) != "run_state_conflict" {
		t.Fatalf("switch during sync: %d %s", code, b)
	}
	close(block)
	h.waitRun(id, status(apiv1.RunStatusAwaitingSwitch))
}

func TestEventStreamReplaysFromLastEventID(t *testing.T) {
	h := newHarness(t)
	block := make(chan struct{})
	h.setup = func(f *fakeEngine) { f.block["presync"] = block }
	ref := h.approved(planJSON("p1", "a", "b", false))
	id := h.start(apiv1.StartMigrationRequest{PlanRef: ref, AutoSwitch: ptr(true)})

	// Live: connect while presync is blocked, then let the run finish.
	msgs := make(chan []sseMsg, 1)
	go func() { msgs <- h.stream(id, "") }()
	time.Sleep(50 * time.Millisecond)
	close(block)
	live := <-msgs

	if live[0].event != "snapshot" || live[len(live)-1].event != "snapshot" {
		t.Fatalf("stream must start and end with a snapshot: %v", names(live))
	}
	final := decode[apiv1.Run](t, []byte(live[len(live)-1].data))
	if final.Status != apiv1.RunStatusSucceeded {
		t.Fatalf("final snapshot status = %s", final.Status)
	}
	var seqs []int64
	for _, m := range live[1 : len(live)-1] {
		if m.id == 0 {
			t.Fatalf("event without id: %+v", m)
		}
		if len(seqs) > 0 && m.id <= seqs[len(seqs)-1] {
			t.Fatalf("ids not increasing: %v then %d", seqs, m.id)
		}
		seqs = append(seqs, m.id)
		h.validateSchema("RunEvent", []byte(m.data))
	}
	h.validateSchema("Run", []byte(live[0].data))

	// Replay: everything after the third event, and nothing before it.
	replay := h.stream(id, strconv.FormatInt(seqs[2], 10))
	var got []int64
	for _, m := range replay {
		if m.id > 0 {
			got = append(got, m.id)
		}
	}
	if len(got) != len(seqs)-3 || got[0] != seqs[3] {
		t.Fatalf("replay after %d = %v, want %v", seqs[2], got, seqs[3:])
	}
	if !strings.Contains(strings.Join(names(live), ","), "status_changed") {
		t.Fatalf("no status_changed events: %v", names(live))
	}
}

func TestCanaryGateFailureRollsReadsBack(t *testing.T) {
	h := newHarness(t)
	h.traffic.stats["eks"] = apiv1.BackendStats{Requests: 3, ErrorRate: 0, P95Millis: 40}
	ref := h.approved(planJSON("p1", "a", "b", true))
	id := h.start(apiv1.StartMigrationRequest{PlanRef: ref, Canary: &apiv1.CanaryOptions{Weights: []int{10, 50}, StepDuration: ptr("20ms")}})

	// Too few requests is inconclusive at 10% and moves on; at the last step
	// (50%) it fails, because the target was never judged.
	run := h.waitRun(id, ended(apiv1.RunStatusRolledBack))
	c := run.Canary
	if c == nil || c.Passed == nil || *c.Passed || len(c.Steps) != 2 {
		t.Fatalf("canary: %+v", c)
	}
	if v := c.Steps[0].Verdict; v == nil || *v != apiv1.CanaryStepVerdictInconclusive || !strings.Contains(c.Steps[0].Gate.Reason, "continuing") {
		t.Fatalf("step 1: %v %+v", v, c.Steps[0].Gate)
	}
	if v := c.Steps[1].Verdict; v == nil || *v != apiv1.CanaryStepVerdictFailed || !strings.Contains(c.Steps[1].Gate.Reason, "only 3 requests") ||
		!strings.Contains(c.Steps[1].Gate.Reason, "last step") {
		t.Fatalf("step 2: %v %+v", v, c.Steps[1].Gate)
	}
	if run.Engine.Steps["fence"].Status != apiv1.StepStatusPending {
		t.Fatal("a failed canary must stop before fence")
	}
	h.traffic.mu.Lock()
	w := fmt.Sprint(h.traffic.weights)
	h.traffic.mu.Unlock()
	if w != "[map[eks:10 gke:90] map[eks:50 gke:50] map[gke:100]]" {
		t.Fatalf("read weights = %s", w)
	}
}

// The recommended gradual canary passes every step and switches.
func TestCanaryGradualStepsPass(t *testing.T) {
	h := newHarness(t)
	h.traffic.stats["eks"] = apiv1.BackendStats{Requests: 500, ErrorRate: 0, P95Millis: 120}
	ref := h.approved(planJSON("p1", "a", "b", true))
	weights := []int{1, 10, 25, 50, 75, 90, 100}
	id := h.start(apiv1.StartMigrationRequest{PlanRef: ref, AutoSwitch: ptr(true), Canary: &apiv1.CanaryOptions{Weights: weights, StepDuration: ptr("20ms")}})
	run := h.waitRun(id, ended(apiv1.RunStatusSucceeded))
	if run.Canary.Passed == nil || !*run.Canary.Passed || len(run.Canary.Steps) != len(weights) {
		t.Fatalf("canary: %+v", run.Canary)
	}
	h.traffic.mu.Lock()
	w := fmt.Sprint(h.traffic.weights[:len(weights)])
	h.traffic.mu.Unlock()
	if w != "[map[eks:1 gke:99] map[eks:10 gke:90] map[eks:25 gke:75] map[eks:50 gke:50] map[eks:75 gke:25] map[eks:90 gke:10] map[eks:100]]" {
		t.Fatalf("read weights = %s", w)
	}
}

// Errors over the limit fail a step even with few requests: at 1% a broken
// target is caught before more reads go to it.
func TestCanaryErrorsFailASmallStep(t *testing.T) {
	h := newHarness(t)
	h.traffic.stats["eks"] = apiv1.BackendStats{Requests: 4, ErrorRate: 0.5, P95Millis: 40}
	ref := h.approved(planJSON("p1", "a", "b", true))
	id := h.start(apiv1.StartMigrationRequest{PlanRef: ref, Canary: &apiv1.CanaryOptions{Weights: []int{1, 10, 100}, StepDuration: ptr("20ms")}})
	run := h.waitRun(id, ended(apiv1.RunStatusRolledBack))
	c := run.Canary
	if len(c.Steps) != 1 || c.Steps[0].Verdict == nil || *c.Steps[0].Verdict != apiv1.CanaryStepVerdictFailed ||
		!strings.Contains(c.Steps[0].Gate.Reason, "error rate 50.00% over only 4 requests") {
		t.Fatalf("canary: %+v", c.Steps)
	}
	h.traffic.mu.Lock()
	w := fmt.Sprint(h.traffic.weights)
	h.traffic.mu.Unlock()
	if w != "[map[eks:1 gke:99] map[gke:100]]" {
		t.Fatalf("read weights = %s", w)
	}
}

// A slow target (p95 over the limit with enough requests) fails the step and
// all reads go back to the source.
func TestCanarySlowTargetRollsReadsBack(t *testing.T) {
	h := newHarness(t)
	h.traffic.stats["eks"] = apiv1.BackendStats{Requests: 200, ErrorRate: 0, P95Millis: 1500}
	ref := h.approved(planJSON("p1", "a", "b", true))
	id := h.start(apiv1.StartMigrationRequest{PlanRef: ref, Canary: &apiv1.CanaryOptions{Weights: []int{1, 10, 100}, StepDuration: ptr("20ms")}})
	run := h.waitRun(id, ended(apiv1.RunStatusRolledBack))
	c := run.Canary
	if len(c.Steps) != 1 || !strings.Contains(c.Steps[0].Gate.Reason, "p95 1500ms is above 800ms") {
		t.Fatalf("canary: %+v", c.Steps)
	}
	if run.Engine.Steps["fence"].Status != apiv1.StepStatusPending || run.Engine.TrafficOnTarget {
		t.Fatalf("a failed canary must not touch writes: %+v", run.Engine)
	}
}

func TestCanaryPassThenAutoSwitch(t *testing.T) {
	h := newHarness(t)
	h.traffic.stats["eks"] = apiv1.BackendStats{Requests: 500, ErrorRate: 0.001, P95Millis: 120}
	ref := h.approved(planJSON("p1", "a", "b", true))
	id := h.start(apiv1.StartMigrationRequest{PlanRef: ref, AutoSwitch: ptr(true), Canary: &apiv1.CanaryOptions{Weights: []int{10, 50}, StepDuration: ptr("20ms")}})
	run := h.waitRun(id, ended(apiv1.RunStatusSucceeded))
	if run.Canary.Passed == nil || !*run.Canary.Passed || len(run.Canary.Steps) != 2 {
		t.Fatalf("canary: %+v", run.Canary)
	}
}

func TestCanaryNeedsGateway(t *testing.T) {
	h := newHarness(t)
	ref := h.approved(planJSON("p1", "a", "b", false)) // gateway mode none
	code, b := h.do("POST", "/v1/migrations", opToken, apiv1.StartMigrationRequest{PlanRef: ref, Canary: &apiv1.CanaryOptions{Weights: []int{10}}})
	if code != 400 || errCode(t, b) != "invalid_request" {
		t.Fatalf("canary without gateway: %d %s", code, b)
	}
	_, b = h.do("GET", "/v1/plans/"+ref, viewerToken, nil)
	if p := decode[apiv1.PlanRecord](t, b); p.Status != apiv1.PlanStatusApproved {
		t.Fatalf("plan must be released after a refused start: %s", p.Status)
	}
}

func TestAbortWhileWaitingThenResume(t *testing.T) {
	h := newHarness(t)
	ref := h.approved(planJSON("p1", "a", "b", false))
	id := h.start(apiv1.StartMigrationRequest{PlanRef: ref})
	h.waitRun(id, status(apiv1.RunStatusAwaitingSwitch))

	if code, b := h.do("POST", "/v1/migrations/"+id+"/abort", opToken, map[string]string{"reason": "demo reset"}); code != 200 {
		t.Fatalf("abort: %d %s", code, b)
	}
	run := h.waitRun(id, ended(apiv1.RunStatusAborted))
	if deref(run.AbortedBy) != "op" || deref(run.AbortReason) != "demo reset" {
		t.Fatalf("aborted run: %+v", run)
	}
	if code, b := h.do("POST", "/v1/migrations/"+id+"/resume", opToken, nil); code != 200 {
		t.Fatalf("resume: %d %s", code, b)
	}
	h.waitRun(id, status(apiv1.RunStatusAwaitingSwitch))
	if code, b := h.do("POST", "/v1/migrations/"+id+"/switch", opToken, nil); code != 200 {
		t.Fatalf("switch: %d %s", code, b)
	}
	h.waitRun(id, ended(apiv1.RunStatusSucceeded))
	if code, b := h.do("POST", "/v1/migrations/"+id+"/resume", opToken, nil); code != 409 {
		t.Fatalf("resume a succeeded run: %d %s", code, b)
	}
}

func TestStepFailureBeforeFenceThenResume(t *testing.T) {
	h := newHarness(t)
	h.setup = func(f *fakeEngine) { f.failAt = "presync" }
	ref := h.approved(planJSON("p1", "a", "b", false))
	id := h.start(apiv1.StartMigrationRequest{PlanRef: ref, AutoSwitch: ptr(true)})
	run := h.waitRun(id, ended(apiv1.RunStatusFailed))
	if !strings.Contains(deref(run.StatusReason), "boom at presync") {
		t.Fatalf("reason: %v", deref(run.StatusReason))
	}
	h.mu.Lock()
	h.engines[id].failAt = "" // the operator fixed the cause
	h.mu.Unlock()
	if code, b := h.do("POST", "/v1/migrations/"+id+"/resume", opToken, nil); code != 200 {
		t.Fatalf("resume: %d %s", code, b)
	}
	h.waitRun(id, ended(apiv1.RunStatusSucceeded))
}

// A halted run resumes through the same endpoint once the engine confirms
// the promotion (engine.ResumeHalted); while it cannot, the run stays halted.
func TestHaltedRunResumesAfterEngineConfirms(t *testing.T) {
	h := newHarness(t)
	h.setup = func(f *fakeEngine) { f.failAt, f.halt = engine.StepCutover, true }
	ref := h.approved(planJSON("p1", "a", "b", false))
	id := h.start(apiv1.StartMigrationRequest{PlanRef: ref, AutoSwitch: ptr(true)})
	h.waitRun(id, ended(apiv1.RunStatusHalted))

	if code, b := h.do("POST", "/v1/migrations/"+id+"/resume", viewerToken, nil); code != 403 {
		t.Fatalf("viewer resume: %d %s", code, b)
	}
	h.setup = func(f *fakeEngine) { f.resumeErr = errors.New("target is not the topology primary") }
	if code, b := h.do("POST", "/v1/migrations/"+id+"/resume", opToken, nil); code != 200 {
		t.Fatalf("resume: %d %s", code, b)
	}
	run := h.waitRun(id, ended(apiv1.RunStatusHalted))
	if r := deref(run.StatusReason); !strings.Contains(r, "resume after halt refused") || !strings.Contains(r, "not the topology primary") {
		t.Fatalf("reason: %q", r)
	}
	if run.Phase != apiv1.RunPhaseDone || run.Engine.Steps[engine.StepCutover].Status != apiv1.StepStatusFailed {
		t.Fatalf("refused resume ran steps: %+v", run)
	}

	if code, b := h.do("POST", "/v1/migrations/"+id+"/resume", opToken, nil); code != 200 {
		t.Fatalf("resume: %d %s", code, b)
	}
	h.waitRun(id, ended(apiv1.RunStatusSucceeded))
	h.mu.Lock()
	resumed := h.engines[id].resumed
	h.mu.Unlock()
	if resumed != 1 {
		t.Fatalf("ResumeHalted calls on the last engine = %d", resumed)
	}

	_, b := h.do("GET", "/v1/audit", opToken, nil)
	if n := strings.Count(string(b), `"afterHalt":true`); n != 2 {
		t.Fatalf("resumes after halt in the audit log = %d: %s", n, b)
	}
}

// A restart during a resume after halt leaves the run interrupted (Recover)
// while the engine is still halted, or already running again if ResumeHalted
// had finished. Both resume.
func TestHaltedResumeCutShortByRestartResumes(t *testing.T) {
	for _, engineResumed := range []bool{false, true} {
		h := newHarness(t)
		h.setup = func(f *fakeEngine) { f.failAt, f.halt = engine.StepCutover, true }
		ref := h.approved(planJSON("p1", "a", "b", false))
		id := h.start(apiv1.StartMigrationRequest{PlanRef: ref, AutoSwitch: ptr(true)})
		h.waitRun(id, ended(apiv1.RunStatusHalted))

		// What the store holds when the process dies during the resume.
		row, err := h.store.GetRun(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		doc := decode[apiv1.Run](t, row.Doc)
		doc.Status, doc.EndedAt = apiv1.RunStatusRunning, nil
		row.Doc, _ = json.Marshal(doc)
		row.Status = string(doc.Status)
		if err := h.store.UpdateRun(context.Background(), row); err != nil {
			t.Fatal(err)
		}
		if n, err := h.runner.Recover(context.Background()); n != 1 || err != nil {
			t.Fatalf("recover: %d %v", n, err)
		}
		if run := h.waitRun(id, status(apiv1.RunStatusInterrupted)); run.Engine.Outcome != apiv1.EngineOutcomeHalted {
			t.Fatalf("engine outcome after recover = %s", run.Engine.Outcome)
		}

		if engineResumed {
			h.setup = func(f *fakeEngine) {
				f.st.Outcome, f.st.FailureStep, f.st.FailureError = engine.OutcomeRunning, "", ""
			}
		}
		if code, b := h.do("POST", "/v1/migrations/"+id+"/resume", opToken, nil); code != 200 {
			t.Fatalf("engineResumed=%t: resume: %d %s", engineResumed, code, b)
		}
		h.waitRun(id, ended(apiv1.RunStatusSucceeded))
		h.mu.Lock()
		resumed := h.engines[id].resumed
		h.mu.Unlock()
		if want := map[bool]int{false: 1, true: 0}[engineResumed]; resumed != want {
			t.Fatalf("engineResumed=%t: ResumeHalted calls = %d, want %d", engineResumed, resumed, want)
		}
	}
}

func TestAbortDuringFirstCanaryStepRestoresReads(t *testing.T) {
	h := newHarness(t)
	ref := h.approved(planJSON("p1", "a", "b", true))
	id := h.start(apiv1.StartMigrationRequest{PlanRef: ref, Canary: &apiv1.CanaryOptions{Weights: []int{10, 50}, StepDuration: ptr("5s")}})
	h.waitRun(id, func(r apiv1.Run) bool { return r.Phase == apiv1.RunPhaseCanary })
	time.Sleep(50 * time.Millisecond)
	if code, b := h.do("POST", "/v1/migrations/"+id+"/abort", opToken, nil); code != 200 {
		t.Fatalf("abort: %d %s", code, b)
	}
	h.waitRun(id, ended(apiv1.RunStatusAborted))
	h.traffic.mu.Lock()
	w := fmt.Sprint(h.traffic.weights)
	h.traffic.mu.Unlock()
	if w != "[map[eks:10 gke:90] map[gke:100]]" {
		t.Fatalf("read weights = %s", w)
	}
}

func TestFenceFailureAfterCanaryRestoresReads(t *testing.T) {
	h := newHarness(t)
	h.traffic.stats["eks"] = apiv1.BackendStats{Requests: 500, P95Millis: 50}
	h.setup = func(f *fakeEngine) { f.failAt = "fence" }
	ref := h.approved(planJSON("p1", "a", "b", true))
	id := h.start(apiv1.StartMigrationRequest{PlanRef: ref, AutoSwitch: ptr(true), Canary: &apiv1.CanaryOptions{Weights: []int{50}, StepDuration: ptr("20ms")}})
	run := h.waitRun(id, ended(apiv1.RunStatusFailed))
	if run.Phase != apiv1.RunPhaseDone || !strings.Contains(deref(run.StatusReason), "boom at fence") {
		t.Fatalf("run: status=%s reason=%v", run.Status, deref(run.StatusReason))
	}
	h.traffic.mu.Lock()
	w := fmt.Sprint(h.traffic.weights)
	h.traffic.mu.Unlock()
	if w != "[map[eks:50 gke:50] map[gke:100]]" {
		t.Fatalf("read weights = %s", w)
	}
}

func TestShutdownDuringSyncDoesNotWaitForSwitch(t *testing.T) {
	h := newHarness(t)
	block := make(chan struct{})
	h.setup = func(f *fakeEngine) { f.block["presync"] = block }
	ref := h.approved(planJSON("p1", "a", "b", false))
	id := h.start(apiv1.StartMigrationRequest{PlanRef: ref})
	h.waitRun(id, func(r apiv1.Run) bool { return r.Engine.Steps["presync"].Status == apiv1.StepStatusRunning })
	if n := h.runner.InterruptWaiting(); n != 0 {
		t.Fatalf("waiting runs = %d", n)
	}
	close(block) // the step finishes after the shutdown began
	h.waitRun(id, ended(apiv1.RunStatusInterrupted))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := h.runner.Wait(ctx); err != nil {
		t.Fatal("runner did not finish after the shutdown")
	}
}

func TestPlanSummaryShowsDBHost(t *testing.T) {
	h := newHarness(t)
	rec := h.submit(planJSON("p1", "a", "b", false))
	if deref(rec.Summary.Source.DbHost) != "db-gke.shop.ts.net:5432" || deref(rec.Summary.Target.DbHost) != "db-eks.shop.ts.net:5432" {
		t.Fatalf("db hosts: %v %v", deref(rec.Summary.Source.DbHost), deref(rec.Summary.Target.DbHost))
	}
	if code, b := h.do("POST", "/v1/plans", agentToken, strings.Replace(planJSON("p1", "a", "b", false), `"id":"p1"`, `"id":"../../x"`, 1)); code != 400 || errCode(t, b) != "invalid_plan" {
		t.Fatalf("path-like id: %d %s", code, b)
	}
}

func TestRecoverMarksActiveRunsInterrupted(t *testing.T) {
	h := newHarness(t)
	ref := h.approved(planJSON("p1", "a", "b", false))
	id := h.start(apiv1.StartMigrationRequest{PlanRef: ref})
	h.waitRun(id, status(apiv1.RunStatusAwaitingSwitch))

	// A second runner over the same store plays the restarted server.
	r2 := runs.New(context.Background(), runs.Config{StateDir: t.TempDir(), Store: h.store, Factory: h.factory})
	n, err := r2.Recover(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("recover = %d, %v", n, err)
	}
	doc, err := r2.Get(context.Background(), id)
	if err != nil || doc.Status != apiv1.RunStatusInterrupted {
		t.Fatalf("after recover: %v %v", doc.Status, err)
	}
	evs, _ := r2.Events(context.Background(), id, 0)
	if last := evs[len(evs)-1]; last.Kind != runs.EventStatusChanged || deref(last.Message) != "interrupted" {
		t.Fatalf("last event: %+v", last)
	}
}

func TestShutdownInterruptsWaitingRuns(t *testing.T) {
	h := newHarness(t)
	ref := h.approved(planJSON("p1", "a", "b", false))
	id := h.start(apiv1.StartMigrationRequest{PlanRef: ref})
	h.waitRun(id, status(apiv1.RunStatusAwaitingSwitch))
	if n := h.runner.InterruptWaiting(); n != 1 {
		t.Fatalf("interrupted %d", n)
	}
	h.waitRun(id, ended(apiv1.RunStatusInterrupted))
}

func TestStartIdempotency(t *testing.T) {
	h := newHarness(t)
	ref := h.approved(planJSON("p1", "a", "b", false))
	ref2 := h.approved(planJSON("p2", "c", "d", false))
	code, b1 := h.do("POST", "/v1/migrations", opToken, apiv1.StartMigrationRequest{PlanRef: ref}, "Idempotency-Key", "k1")
	code2, b2 := h.do("POST", "/v1/migrations", opToken, apiv1.StartMigrationRequest{PlanRef: ref}, "Idempotency-Key", "k1")
	if code != 202 || code2 != 202 || decode[apiv1.StartMigrationResponse](t, b1).RunId != decode[apiv1.StartMigrationResponse](t, b2).RunId {
		t.Fatalf("retry: %d %s / %d %s", code, b1, code2, b2)
	}
	if code, b := h.do("POST", "/v1/migrations", opToken, apiv1.StartMigrationRequest{PlanRef: ref2}, "Idempotency-Key", "k1"); code != 409 || errCode(t, b) != "idempotency_key_reused" {
		t.Fatalf("reused key: %d %s", code, b)
	}
}

func TestSameClusterIsBusyAndPlanIsReleased(t *testing.T) {
	h := newHarness(t)
	ref := h.approved(planJSON("p1", "gke-ctx", "eks-ctx", false))
	ref2 := h.approved(planJSON("p2", "gke-ctx", "eks-ctx", false))
	h.start(apiv1.StartMigrationRequest{PlanRef: ref})
	if code, b := h.do("POST", "/v1/migrations", opToken, apiv1.StartMigrationRequest{PlanRef: ref2}); code != 409 || errCode(t, b) != "cluster_busy" {
		t.Fatalf("second start: %d %s", code, b)
	}
	_, b := h.do("GET", "/v1/plans/"+ref2, viewerToken, nil)
	if p := decode[apiv1.PlanRecord](t, b); p.Status != apiv1.PlanStatusApproved {
		t.Fatalf("busy plan status = %s", p.Status)
	}
}

func TestEngineInitFailureReleasesPlan(t *testing.T) {
	h := newHarness(t)
	h.newErr = errors.New(`context "nope" does not exist in /home/secret/.kube/config`)
	ref := h.approved(planJSON("p1", "a", "b", false))
	code, b := h.do("POST", "/v1/migrations", opToken, apiv1.StartMigrationRequest{PlanRef: ref})
	if code != 502 || errCode(t, b) != "engine_init_failed" || strings.Contains(string(b), "secret") {
		t.Fatalf("start: %d %s", code, b)
	}
	_, b = h.do("GET", "/v1/plans/"+ref, viewerToken, nil)
	if p := decode[apiv1.PlanRecord](t, b); p.Status != apiv1.PlanStatusApproved {
		t.Fatalf("plan status = %s", p.Status)
	}
}

func TestEnvironmentsAndTraffic(t *testing.T) {
	h := newHarness(t)
	_, b := h.do("GET", "/v1/envs", viewerToken, nil)
	list := decode[apiv1.EnvironmentList](t, b)
	if len(list.Environments) != 3 || !list.Environments[0].Live || list.Environments[1].Live {
		t.Fatalf("envs: %+v", list)
	}
	if code, b := h.do("GET", "/v1/envs/nope", viewerToken, nil); code != 404 || errCode(t, b) != "env_not_found" {
		t.Fatalf("unknown env: %d %s", code, b)
	}
	if code, b := h.do("GET", "/v1/envs/gke", viewerToken, nil); code != 200 || !decode[apiv1.Environment](t, b).Live {
		t.Fatalf("gke: %d %s", code, b)
	}
	if code, b := h.do("GET", "/v1/traffic?window=30s", viewerToken, nil); code != 200 || decode[apiv1.TrafficState](t, b).WriteBackend != "gke" {
		t.Fatalf("traffic: %d %s", code, b)
	}
	if code, _ := h.do("GET", "/v1/traffic?window=1h", viewerToken, nil); code != 400 {
		t.Fatalf("bad window: %d", code)
	}

	h2 := newHarness(t, withoutGateway())
	if code, b := h2.do("GET", "/v1/traffic", viewerToken, nil); code != 503 || errCode(t, b) != "gateway_not_configured" {
		t.Fatalf("no gateway: %d %s", code, b)
	}
}

func TestCORSPreflight(t *testing.T) {
	h := newHarness(t)
	req, _ := http.NewRequest("OPTIONS", h.srv.URL+"/v1/migrations", nil)
	req.Header.Set("Origin", "http://console.test")
	req.Header.Set("Access-Control-Request-Method", "POST")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 204 || resp.Header.Get("Access-Control-Allow-Origin") != "http://console.test" ||
		!strings.Contains(resp.Header.Get("Access-Control-Allow-Headers"), "Last-Event-ID") {
		t.Fatalf("preflight: %d %v", resp.StatusCode, resp.Header)
	}
	req.Header.Set("Origin", "http://evil.test")
	resp, _ = http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("unknown origin must not be allowed")
	}
}

func TestOpenAPIServed(t *testing.T) {
	h := newHarness(t)
	resp, err := http.Get(h.srv.URL + "/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !bytes.Equal(b, specdoc.OpenAPI) {
		t.Fatalf("openapi.yaml: %d", resp.StatusCode)
	}
}

// --- SSE helpers ---

type sseMsg struct {
	id    int64
	event string
	data  string
}

func (h *harness) stream(id, lastEventID string) []sseMsg {
	h.t.Helper()
	req, _ := http.NewRequest("GET", h.srv.URL+"/v1/migrations/"+id+"/events", nil)
	req.Header.Set("Authorization", "Bearer "+viewerToken)
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Error(err)
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		h.t.Errorf("events: %d", resp.StatusCode)
		return nil
	}
	var out []sseMsg
	var cur sseMsg
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if cur.event != "" {
				out = append(out, cur)
			}
			cur = sseMsg{}
		case strings.HasPrefix(line, "id: "):
			cur.id, _ = strconv.ParseInt(strings.TrimPrefix(line, "id: "), 10, 64)
		case strings.HasPrefix(line, "event: "):
			cur.event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			cur.data = strings.TrimPrefix(line, "data: ")
		}
	}
	return out
}

func names(ms []sseMsg) []string {
	var out []string
	for _, m := range ms {
		out = append(out, m.event)
	}
	return out
}

func ptr[T any](v T) *T { return &v }

// An agent's plan is approved only with a passing rehearsal report for the
// plan as it is now; GET shows why approval is blocked (platform #24).
func TestApproveAIPlanNeedsRehearsal(t *testing.T) {
	h := newHarness(t)
	body := planJSON("p1", "a", "b", false)
	rec := h.submit(body)
	if rec.SubmittedRole == nil || *rec.SubmittedRole != apiv1.RoleAgent || rec.Rehearsal == nil || !rec.Rehearsal.Required || rec.Rehearsal.Passed {
		t.Fatalf("new AI plan: %+v %+v", rec.SubmittedRole, rec.Rehearsal)
	}
	if code, b := h.do("POST", "/v1/plans/"+rec.Ref+"/approve", opToken, nil); code != 409 || errCode(t, b) != "rehearsal_required" {
		t.Fatalf("approve without a report: %d %s", code, b)
	}
	failed := `{"schemaVersion":"hopcharlie.rehearsal/v1","passed":false,"candidate":{"planSha256":"x"}}`
	if code, b := h.do("PUT", "/v1/plans/"+rec.Ref+"/attachments/rehearsal-report.json", agentToken, failed); code != 200 {
		t.Fatalf("upload: %d %s", code, b)
	}
	_, b := h.do("GET", "/v1/plans/"+rec.Ref, viewerToken, nil)
	if got := decode[apiv1.PlanRecord](t, b); got.Rehearsal == nil || got.Rehearsal.Passed || got.Rehearsal.Reason == nil || !strings.Contains(*got.Rehearsal.Reason, "did not pass") {
		t.Fatalf("gate after a failed rehearsal: %s", b)
	}
	if code, b := h.do("POST", "/v1/plans/"+rec.Ref+"/approve", opToken, map[string]any{"revision": 2}); code != 409 || errCode(t, b) != "rehearsal_required" {
		t.Fatalf("approve after a failed rehearsal: %d %s", code, b)
	}
	rev := h.rehearsed(rec.Ref, body, "")
	if code, b := h.do("POST", "/v1/plans/"+rec.Ref+"/approve", opToken, map[string]any{"revision": rev}); code != 200 {
		t.Fatalf("approve after a passing rehearsal: %d %s", code, b)
	}
	// An operator's plan needs no report.
	code, b := h.do("POST", "/v1/plans", opToken, planJSON("p2", "a", "b", false))
	op := decode[apiv1.PlanRecord](t, b)
	if code != 201 || op.Rehearsal == nil || op.Rehearsal.Required {
		t.Fatalf("operator plan: %d %s", code, b)
	}
	if code, b := h.do("POST", "/v1/plans/"+op.Ref+"/approve", opToken, nil); code != 200 {
		t.Fatalf("approve an operator plan: %d %s", code, b)
	}
}
