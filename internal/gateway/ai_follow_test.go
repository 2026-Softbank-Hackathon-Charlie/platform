package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeTraffic is the traffic gateway's unauthenticated /metrics as the AI
// follower reads it: moimroom-mig on one backend, plus another app as noise.
type fakeTraffic struct {
	srv     *httptest.Server
	mu      sync.Mutex
	backend string // the one at 1; the others are listed at 0
	held    bool
	code    int    // non-zero answers this status instead
	raw     string // non-empty answers this body instead
	hits    atomic.Int64
	queries chan string
}

func newFakeTraffic(t *testing.T, writeBackend string) *fakeTraffic {
	t.Helper()
	ft := &fakeTraffic{backend: writeBackend, queries: make(chan string, 100)}
	ft.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ft.hits.Add(1)
		select {
		case ft.queries <- r.Method + " " + r.URL.Path + "?" + r.URL.RawQuery:
		default:
		}
		if r.Header.Get("Authorization") != "" {
			t.Error("follower sent credentials to /metrics")
		}
		if r.Method != http.MethodGet || r.URL.Path != "/metrics" {
			http.NotFound(w, r)
			return
		}
		ft.mu.Lock()
		backend, held, code, raw := ft.backend, ft.held, ft.code, ft.raw
		ft.mu.Unlock()
		switch {
		case code != 0:
			http.Error(w, "boom", code)
		case raw != "":
			io.WriteString(w, raw)
		default:
			io.WriteString(w, trafficMetrics(backend, held))
		}
	}))
	t.Cleanup(ft.srv.Close)
	return ft
}

// trafficMetrics renders the two gauges as the traffic gateway exports them
// for registered apps; backend "" puts no backend at 1.
func trafficMetrics(backend string, held bool) string {
	var b strings.Builder
	b.WriteString("# HELP charlie_gateway_app_write_backend Registered apps, by app. 1 for the backend that receives writes.\n# TYPE charlie_gateway_app_write_backend gauge\n")
	names := []string{"eks", "gke", "onprem"}
	if backend != "" && !slices.Contains(names, backend) {
		names = append(names, backend)
	}
	for _, n := range names {
		fmt.Fprintf(&b, "charlie_gateway_app_write_backend{app=\"moimroom-mig\",backend=%q} %d\n", n, b2i(n == backend))
	}
	b.WriteString("charlie_gateway_app_write_backend{app=\"other\",backend=\"onprem\"} 1\n")
	b.WriteString("# HELP charlie_gateway_app_writes_held Registered apps, by app. 1 while new writes are held.\n# TYPE charlie_gateway_app_writes_held gauge\n")
	fmt.Fprintf(&b, "charlie_gateway_app_writes_held{app=\"moimroom-mig\"} %d\n", b2i(held))
	b.WriteString("charlie_gateway_app_writes_held{app=\"other\"} 1\n")
	return b.String()
}

func b2i(v bool) int {
	if v {
		return 1
	}
	return 0
}

func (ft *fakeTraffic) set(writeBackend string, held bool) {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	ft.backend, ft.held, ft.code, ft.raw = writeBackend, held, 0, ""
}

func (ft *fakeTraffic) fail(code int, raw string) {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	ft.code, ft.raw = code, raw
}

// lockedBuffer collects log output written from several goroutines.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

// records returns the JSON log records whose msg is msg.
func (l *lockedBuffer) records(msg string) []map[string]any {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []map[string]any
	for _, line := range strings.Split(l.b.String(), "\n") {
		var rec map[string]any
		if json.Unmarshal([]byte(line), &rec) == nil && rec["msg"] == msg {
			out = append(out, rec)
		}
	}
	return out
}

type followGW struct {
	g      *Gateway
	ft     *fakeTraffic
	logs   *lockedBuffer
	admin  *httptest.Server
	public *httptest.Server
}

func followConfig(trafficURL string) *AIFollowConfig {
	return &AIFollowConfig{App: "moimroom-mig", URL: trafficURL,
		Map: map[string]string{"onprem": "laptop", "gke": "gcp", "eks": "gcp"}, Interval: 2 * time.Second}
}

func newFollowGW(t *testing.T, appBackend, gcp, laptop string, transport http.RoundTripper) *followGW {
	t.Helper()
	ft := newFakeTraffic(t, appBackend)
	fg := newFollowGWAt(t, ft.srv.URL, gcp, laptop, transport)
	fg.ft = ft
	return fg
}

func newFollowGWAt(t *testing.T, trafficURL, gcp, laptop string, transport http.RoundTripper) *followGW {
	t.Helper()
	logs := &lockedBuffer{}
	gu, _ := url.Parse(gcp)
	lu, _ := url.Parse(laptop)
	cfg := Config{Backends: []BackendConfig{{Name: "gcp", URL: gu}, {Name: "laptop", URL: lu}}, AdminToken: "admin-secret", WriteTimeout: 12 * time.Second, Transport: transport,
		AIDemo: &AIDemoConfig{App: "app", Token: "ai-secret"}, AIFollow: followConfig(trafficURL), Log: slog.New(slog.NewJSONHandler(logs, nil))}
	g, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	fg := &followGW{g: g, logs: logs, admin: httptest.NewServer(g.Admin()), public: httptest.NewServer(g.Proxy())}
	t.Cleanup(func() { fg.public.Close(); fg.admin.Close() })
	return fg
}

func (fg *followGW) sync() { fg.g.router.follow.sync(context.Background()) }

func (fg *followGW) route() string {
	fg.g.router.mu.Lock()
	defer fg.g.router.mu.Unlock()
	return fg.g.router.writeBackend
}

type followView struct {
	Enabled    bool       `json:"enabled"`
	App        string     `json:"app"`
	AppBackend string     `json:"appBackend"`
	Target     string     `json:"target"`
	Fallback   bool       `json:"fallback"`
	LastSync   *time.Time `json:"lastSync"`
	LastError  string     `json:"lastError"`
}

func (fg *followGW) state(t *testing.T) (string, *followView) {
	t.Helper()
	got := aiCall(fg.admin.Client(), "GET", fg.admin.URL+"/v1/state", "admin-secret", "", "")
	requireAIStatus(t, got, 200)
	var st struct {
		WriteBackend string      `json:"writeBackend"`
		Follow       *followView `json:"follow"`
	}
	if err := json.Unmarshal(got.body, &st); err != nil {
		t.Fatal(err)
	}
	return st.WriteBackend, st.Follow
}

func (fg *followGW) metrics(t *testing.T) string {
	t.Helper()
	got := aiCall(fg.admin.Client(), "GET", fg.admin.URL+"/metrics", "", "", "")
	requireAIStatus(t, got, 200)
	return string(got.body)
}

func TestAIFollowConfigValidation(t *testing.T) {
	gu, _ := url.Parse("http://gcp")
	lu, _ := url.Parse("http://laptop")
	valid := func() Config {
		return Config{Backends: []BackendConfig{{Name: "gcp", URL: gu}, {Name: "laptop", URL: lu}}, AdminToken: "admin", AIDemo: &AIDemoConfig{App: "app", Token: "ai"},
			WriteTimeout: 12 * time.Second, AIFollow: followConfig("http://hopcharlie-gateway.charlie-platform.svc.cluster.local:9901")}
	}
	if _, err := New(valid()); err != nil {
		t.Fatalf("valid follow config rejected: %v", err)
	}
	zero := valid()
	zero.AIFollow.Interval = 0
	if g, err := New(zero); err != nil || g.router.follow.cfg.Interval != DefaultAIFollowInterval {
		t.Fatalf("zero interval: err=%v", err)
	}
	for _, tc := range []struct {
		name   string
		change func(*Config)
	}{
		{"no-ai-demo", func(c *Config) { c.AIDemo = nil; c.WriteTimeout = 0 }},
		{"no-app", func(c *Config) { c.AIFollow.App = "" }},
		{"bad-app", func(c *Config) { c.AIFollow.App = "Moim Room" }},
		{"no-url", func(c *Config) { c.AIFollow.URL = "" }},
		{"bad-scheme", func(c *Config) { c.AIFollow.URL = "ftp://gateway:9901" }},
		{"no-host", func(c *Config) { c.AIFollow.URL = "http://" }},
		{"relative", func(c *Config) { c.AIFollow.URL = "gateway:9901" }},
		{"query", func(c *Config) { c.AIFollow.URL = "http://gateway:9901?x=1" }},
		{"empty-query", func(c *Config) { c.AIFollow.URL = "http://gateway:9901?" }},
		{"fragment", func(c *Config) { c.AIFollow.URL = "http://gateway:9901#state" }},
		{"userinfo", func(c *Config) { c.AIFollow.URL = "http://user:secret@gateway:9901" }},
		{"user-only", func(c *Config) { c.AIFollow.URL = "http://user@gateway:9901" }},
		{"empty-map", func(c *Config) { c.AIFollow.Map = nil }},
		{"bad-target", func(c *Config) { c.AIFollow.Map["onprem"] = "aks" }},
		{"empty-key", func(c *Config) { c.AIFollow.Map[""] = "gcp" }},
		{"short-interval", func(c *Config) { c.AIFollow.Interval = 499 * time.Millisecond }},
		{"negative-interval", func(c *Config) { c.AIFollow.Interval = -time.Second }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := valid()
			tc.change(&c)
			if _, err := New(c); err == nil {
				t.Fatal("invalid AI follow config accepted")
			}
		})
	}
	m, err := ParseAIFollowMap(" onprem=laptop, gke=gcp ,eks=gcp")
	if err != nil || len(m) != 3 || m["onprem"] != "laptop" || m["gke"] != "gcp" || m["eks"] != "gcp" {
		t.Fatalf("parse map: %v %v", m, err)
	}
	for _, bad := range []string{"", "onprem", "=gcp", "onprem=", "gke=gcp,gke=laptop", "gke=gcp,,eks=gcp"} {
		if _, err := ParseAIFollowMap(bad); err == nil {
			t.Fatalf("map %q accepted", bad)
		}
	}
}

func TestAIFollowOffKeepsStateAndAdminUnchanged(t *testing.T) {
	g := aiTestGateway(t, "http://gcp", "http://laptop", nil)
	if g.router.follow != nil {
		t.Fatal("follower built without AIFollow")
	}
	admin := httptest.NewServer(g.Admin())
	defer admin.Close()
	got := aiCall(admin.Client(), "GET", admin.URL+"/v1/state", "admin-secret", "", "")
	requireAIStatus(t, got, 200)
	if strings.Contains(string(got.body), `"follow"`) {
		t.Fatalf("follow shown while off: %s", got.body)
	}
	got = aiCall(admin.Client(), "GET", admin.URL+"/metrics", "", "", "")
	if strings.Contains(string(got.body), "ai_follow") {
		t.Fatal("follow metrics registered while off")
	}
	// The endpoint does not exist: the mux answers as for any unknown path.
	got = aiCall(admin.Client(), "POST", admin.URL+"/v1/follow", "admin-secret", "", `{"enabled":true}`)
	if got.code != http.StatusNotFound || strings.Contains(string(got.body), "{") {
		t.Fatalf("follow endpoint registered while off: %d %s", got.code, got.body)
	}
}

func TestAIFollowSwitchesWithApp(t *testing.T) {
	fg := newFollowGW(t, "onprem", "http://gcp", "http://laptop", nil)
	if fg.route() != "gcp" {
		t.Fatalf("initial route %s", fg.route())
	}
	if fg.ft.hits.Load() != 0 {
		t.Fatal("follower polled before Run or sync")
	}
	fg.sync()
	if got := <-fg.ft.queries; got != "GET /metrics?name%5B%5D=charlie_gateway_app_write_backend&name%5B%5D=charlie_gateway_app_writes_held" {
		t.Fatalf("polled %s", got)
	}
	route, f := fg.state(t)
	if route != "laptop" || f == nil || !f.Enabled || f.App != "moimroom-mig" || f.AppBackend != "onprem" || f.Target != "laptop" || f.Fallback || f.LastSync == nil || f.LastError != "" {
		t.Fatalf("onprem: route=%s follow=%+v", route, f)
	}
	fg.ft.set("gke", false)
	fg.sync()
	if route, f = fg.state(t); route != "gcp" || f.AppBackend != "gke" || f.Target != "gcp" {
		t.Fatalf("gke: route=%s follow=%+v", route, f)
	}
	fg.ft.set("eks", false)
	fg.sync()
	if route, f = fg.state(t); route != "gcp" || f.AppBackend != "eks" {
		t.Fatalf("eks: route=%s follow=%+v", route, f)
	}
	logs := fg.logs.records("AI route follows app")
	if len(logs) != 2 {
		t.Fatalf("want one log per change, got %v", logs)
	}
	if l := logs[0]; l["from"] != "gcp" || l["to"] != "laptop" || l["appBackend"] != "onprem" || l["fallback"] != false {
		t.Fatalf("first change log %v", l)
	}
	if l := logs[1]; l["from"] != "laptop" || l["to"] != "gcp" || l["appBackend"] != "gke" {
		t.Fatalf("second change log %v", l)
	}
	if m := fg.metrics(t); !strings.Contains(m, `charlie_gateway_ai_follow_switches_total{fallback="false"} 2`) || !strings.Contains(m, "charlie_gateway_ai_follow_fallback 0") {
		t.Fatalf("switch metrics:\n%s", m)
	}
}

func TestAIFollowWaitsWhileAppHeld(t *testing.T) {
	fg := newFollowGW(t, "gke", "http://gcp", "http://laptop", nil)
	fg.sync()
	fg.ft.fail(http.StatusInternalServerError, "") // leaves a lastError behind
	fg.sync()
	f := fg.g.router.follow
	f.mu.Lock()
	f.lastSync = time.Time{} // so the held read must set it again
	f.mu.Unlock()
	fg.ft.set("onprem", true) // cutover in progress: route already moved, writes held
	fg.sync()
	route, st := fg.state(t)
	if route != "gcp" || st.LastError != "" || st.AppBackend != "onprem" || st.Target != "gcp" || st.LastSync == nil {
		t.Fatalf("while app held: route=%s follow=%+v", route, f)
	}
	if len(fg.logs.records("AI route follows app")) != 0 {
		t.Fatal("switched while held")
	}
	fg.ft.set("onprem", false)
	fg.sync()
	if fg.route() != "laptop" {
		t.Fatalf("did not follow after release: %s", fg.route())
	}
}

func TestAIFollowErrorsKeepRoute(t *testing.T) {
	for _, tc := range []struct {
		name, reason string
		breakIt      func(fg *followGW)
	}{
		{"unreachable", "unreachable", func(fg *followGW) { fg.ft.srv.Close() }},
		{"status", "status", func(fg *followGW) { fg.ft.fail(http.StatusInternalServerError, "") }},
		{"not-found", "status", func(fg *followGW) { fg.ft.fail(http.StatusNotFound, "") }},
		{"bad-text", "decode", func(fg *followGW) { fg.ft.fail(0, "charlie_gateway_app_write_backend{app=\"moimroom-mig\" 1\n") }},
		{"missing-app", "decode", func(fg *followGW) {
			fg.ft.fail(0, strings.ReplaceAll(trafficMetrics("gke", false), `app="moimroom-mig"`, `app="moimroom"`))
		}},
		{"two-backends", "decode", func(fg *followGW) {
			fg.ft.fail(0, strings.Replace(trafficMetrics("gke", false), `backend="eks"} 0`, `backend="eks"} 1`, 1))
		}},
		{"no-backend-at-1", "decode", func(fg *followGW) { fg.ft.fail(0, trafficMetrics("", false)) }},
		{"missing-held", "decode", func(fg *followGW) {
			fg.ft.fail(0, strings.Split(trafficMetrics("gke", false), "# HELP charlie_gateway_app_writes_held")[0])
		}},
		{"bad-held-value", "decode", func(fg *followGW) {
			fg.ft.fail(0, strings.Replace(trafficMetrics("gke", false), `writes_held{app="moimroom-mig"} 0`, `writes_held{app="moimroom-mig"} 2`, 1))
		}},
		{"too-large", "decode", func(fg *followGW) {
			fg.ft.fail(0, trafficMetrics("gke", false)+strings.Repeat("# padding\n", maxAIFollowBody/10+1))
		}},
		{"unmapped", "unmapped", func(fg *followGW) { fg.ft.set("aks", false) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fg := newFollowGW(t, "onprem", "http://gcp", "http://laptop", nil)
			fg.sync()
			_, before := fg.state(t)
			tc.breakIt(fg)
			fg.sync()
			fg.sync()
			route, f := fg.state(t)
			if route != "laptop" || f.LastError == "" || f.AppBackend != before.AppBackend || !f.LastSync.Equal(*before.LastSync) {
				t.Fatalf("route=%s follow=%+v before=%+v", route, f, before)
			}
			if m := fg.metrics(t); !strings.Contains(m, `charlie_gateway_ai_follow_errors_total{reason="`+tc.reason+`"} 2`) {
				t.Fatalf("error metric for %s:\n%s", tc.reason, m)
			}
			if len(fg.logs.records("AI follow failed; keeping route")) != 2 {
				t.Fatal("warning not logged")
			}
			if tc.name == "unreachable" {
				return
			}
			fg.ft.set("gke", false)
			fg.sync()
			if route, f = fg.state(t); route != "gcp" || f.LastError != "" {
				t.Fatalf("did not recover: route=%s follow=%+v", route, f)
			}
		})
	}
}

func TestAIFollowFallsBackFromUnhealthyTarget(t *testing.T) {
	fg := newFollowGW(t, "gke", "http://gcp", "http://laptop", nil)
	gcp, laptop := fg.g.router.byName["gcp"], fg.g.router.byName["laptop"]
	gcp.healthy.Store(false)
	fg.sync()
	route, f := fg.state(t)
	if route != "laptop" || f.Target != "laptop" || !f.Fallback || f.AppBackend != "gke" {
		t.Fatalf("fallback: route=%s follow=%+v", route, f)
	}
	if l := fg.logs.records("AI route follows app"); len(l) != 1 || l[0]["fallback"] != true || l[0]["to"] != "laptop" {
		t.Fatalf("fallback log %v", l)
	}
	if m := fg.metrics(t); !strings.Contains(m, `charlie_gateway_ai_follow_switches_total{fallback="true"} 1`) || !strings.Contains(m, "charlie_gateway_ai_follow_fallback 1") {
		t.Fatalf("fallback metrics:\n%s", m)
	}
	// Both down: keep whatever serves now.
	laptop.healthy.Store(false)
	fg.sync()
	if route, f = fg.state(t); route != "laptop" || f.LastError == "" || f.Target != "laptop" || f.Fallback || f.AppBackend != "gke" {
		t.Fatalf("both unhealthy: route=%s follow=%+v", route, f)
	}
	if m := fg.metrics(t); !strings.Contains(m, "charlie_gateway_ai_follow_fallback 0") {
		t.Fatalf("fallback gauge while both unhealthy:\n%s", m)
	}
	// The followed target recovers: go back to it.
	gcp.healthy.Store(true)
	fg.sync()
	if route, f = fg.state(t); route != "gcp" || f.Fallback || f.LastError != "" {
		t.Fatalf("recovered: route=%s follow=%+v", route, f)
	}
}

func TestAIFollowManualRoutePinsUntilFollowEnabled(t *testing.T) {
	fg := newFollowGW(t, "gke", "http://gcp", "http://laptop", nil)
	client := fg.admin.Client()
	fg.sync()
	requireAIStatus(t, aiCall(client, "POST", fg.admin.URL+"/v1/route", "admin-secret", "", `{"backend":"laptop"}`), 200)
	if _, f := fg.state(t); f.Enabled {
		t.Fatal("manual route did not pin")
	}
	hits := fg.ft.hits.Load()
	fg.sync()
	if fg.route() != "laptop" || fg.ft.hits.Load() != hits {
		t.Fatalf("pinned route followed: %s", fg.route())
	}
	// Bad requests change nothing.
	requireAIStatus(t, aiCall(client, "POST", fg.admin.URL+"/v1/follow", "wrong", "", `{"enabled":true}`), 401)
	requireAIStatus(t, aiCall(client, "POST", fg.admin.URL+"/v1/follow", "admin-secret", "", `{}`), 400)
	requireAIStatus(t, aiCall(client, "POST", fg.admin.URL+"/v1/follow", "admin-secret", "", `{"enabled":"yes"}`), 400)
	if _, f := fg.state(t); f.Enabled {
		t.Fatal("bad follow request enabled following")
	}
	// Enabling syncs at once; the response already shows the result.
	got := aiCall(client, "POST", fg.admin.URL+"/apps/app/v1/follow", "admin-secret", "", `{"enabled":true}`)
	requireAIStatus(t, got, 200)
	var st struct {
		WriteBackend string     `json:"writeBackend"`
		Follow       followView `json:"follow"`
	}
	if err := json.Unmarshal(got.body, &st); err != nil || st.WriteBackend != "gcp" || !st.Follow.Enabled || st.Follow.Target != "gcp" {
		t.Fatalf("enable: %s %v", got.body, err)
	}
	// Disabling pins the current route without a manual route.
	requireAIStatus(t, aiCall(client, "POST", fg.admin.URL+"/v1/follow", "admin-secret", "", `{"enabled":false}`), 200)
	fg.ft.set("onprem", false)
	fg.sync()
	if fg.route() != "gcp" {
		t.Fatal("disabled follower changed the route")
	}
	// A rejected manual route does not pin.
	requireAIStatus(t, aiCall(client, "POST", fg.admin.URL+"/v1/follow", "admin-secret", "", `{"enabled":true}`), 200)
	requireAIStatus(t, aiCall(client, "POST", fg.admin.URL+"/v1/route", "admin-secret", "", `{"backend":"aks"}`), 400)
	if route, f := fg.state(t); route != "laptop" || !f.Enabled {
		t.Fatalf("after rejected route: route=%s follow=%+v", route, f)
	}
}

func TestAIFollowUsesRouteChecks(t *testing.T) {
	fg := newFollowGW(t, "gke", "http://gcp", "http://laptop", nil)
	fg.sync()
	rt := fg.g.router
	rt.mu.Lock()
	rt.held, rt.inflightWrites = true, 1
	rt.mu.Unlock()
	fg.ft.set("onprem", false)
	fg.sync()
	route, f := fg.state(t)
	if route != "gcp" || !strings.Contains(f.LastError, "writes_draining") {
		t.Fatalf("route checks bypassed: route=%s follow=%+v", route, f)
	}
	if m := fg.metrics(t); !strings.Contains(m, `charlie_gateway_ai_follow_errors_total{reason="route"} 1`) {
		t.Fatalf("route error metric:\n%s", m)
	}
	rt.mu.Lock()
	rt.held, rt.inflightWrites = false, 0
	rt.mu.Unlock()
	fg.sync()
	if fg.route() != "laptop" {
		t.Fatalf("did not follow once drained: %s", fg.route())
	}
}

func TestAIFollowRunSyncsAtStartThenEachTick(t *testing.T) {
	fg := newFollowGW(t, "onprem", "http://gcp", "http://laptop", nil)
	f := fg.g.router.follow
	ticks := make(chan time.Time)
	f.ticks = ticks
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { f.run(ctx); close(done) }()
	waitFor(t, "sync at start", func() bool { return fg.route() == "laptop" })
	fg.ft.set("gke", false)
	ticks <- time.Now()
	waitFor(t, "sync on tick", func() bool { return fg.route() == "gcp" })
	cancel()
	aiWait(t, done)

	// Shutdown also stops the loop.
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	done2 := make(chan struct{})
	go func() { f.run(ctx2); close(done2) }()
	fg.g.BeginShutdown()
	aiWait(t, done2)
}

func TestAIFollowStartsWithGatewayRun(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") }))
	defer up.Close()
	fg := newFollowGW(t, "onprem", up.URL, up.URL, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { fg.g.Run(ctx); close(done) }()
	waitFor(t, "Run starts the follower", func() bool { return fg.route() == "laptop" })
	cancel()
	aiWait(t, done)
	aiWait(t, fg.g.router.follow.stopped)
}

func TestAIFollowInFlightFinishesOnOldBackend(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	upstream := func(name string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("X-Request-Id") == "old-001" {
				close(entered)
				<-release
			}
			io.WriteString(w, `{"message":{"content":"`+name+`"},"done":true}`)
		}
	}
	gcp := httptest.NewServer(upstream("gcp"))
	defer gcp.Close()
	laptop := httptest.NewServer(upstream("laptop"))
	defer laptop.Close()
	fg := newFollowGW(t, "gke", gcp.URL, laptop.URL, nil)
	fg.sync()
	client := &http.Client{Timeout: 3 * time.Second}
	old := make(chan aiReply, 1)
	go func() { old <- aiCall(client, "POST", fg.public.URL+"/api/chat", "ai-secret", "old-001", aiTestBody) }()
	aiWait(t, entered)
	fg.ft.set("onprem", false)
	fg.sync()
	if fg.route() != "laptop" {
		t.Fatalf("did not follow with a request in flight: %s", fg.route())
	}
	next := aiCall(client, "POST", fg.public.URL+"/api/chat", "ai-secret", "new-001", aiTestBody)
	requireAIStatus(t, next, 200)
	if next.backend != "laptop" || !strings.Contains(string(next.body), "laptop") {
		t.Fatalf("new request: %+v", next)
	}
	once.Do(func() { close(release) })
	got := aiReceive(t, old)
	requireAIStatus(t, got, 200)
	if got.backend != "gcp" || !strings.Contains(string(got.body), "gcp") {
		t.Fatalf("in-flight request moved: %+v %s", got, got.body)
	}
}

// TestAIFollowReadsRealTrafficGateway follows an app registered on a real
// traffic gateway, so the metric names and labels are the ones it exports.
func TestAIFollowReadsRealTrafficGateway(t *testing.T) {
	traffic := newTestGW(t, Config{}, "gke")
	call := func(method, path, body string) {
		t.Helper()
		got := aiCall(traffic.admin.Client(), method, traffic.admin.URL+path, testToken, "", body)
		if got.err != nil || got.code/100 != 2 {
			t.Fatalf("%s %s: %d %s %v", method, path, got.code, got.body, got.err)
		}
	}
	call("PUT", "/apps/moimroom-mig", `{"backends":{"gke":"http://gke","eks":"http://eks","onprem":"http://onprem"},"writeBackend":"onprem"}`)
	call("PUT", "/apps/other", `{"backends":{"gke":"http://gke2"},"writeBackend":"gke"}`)
	fg := newFollowGWAt(t, traffic.admin.URL, "http://gcp", "http://laptop", nil)
	// name[] keeps the answer to the two families, well under the body cap.
	got := aiCall(traffic.admin.Client(), "GET", fg.g.router.follow.metricsURL, "", "", "")
	requireAIStatus(t, got, 200)
	if strings.Contains(string(got.body), "charlie_gateway_app_requests_total") || !strings.Contains(string(got.body), `charlie_gateway_app_writes_held{app="moimroom-mig"} 0`) {
		t.Fatalf("metrics not filtered by name[]:\n%s", got.body)
	}
	fg.sync()
	if route, f := fg.state(t); route != "laptop" || f.AppBackend != "onprem" || f.LastError != "" {
		t.Fatalf("onprem: route=%s follow=%+v", route, f)
	}
	call("POST", "/apps/moimroom-mig/v1/writes/hold", "")
	call("POST", "/apps/moimroom-mig/v1/route", `{"backend":"gke"}`)
	fg.sync()
	if route, f := fg.state(t); route != "laptop" || f.AppBackend != "gke" || f.LastError != "" {
		t.Fatalf("held: route=%s follow=%+v", route, f)
	}
	call("POST", "/apps/moimroom-mig/v1/writes/release", "")
	fg.sync()
	if route, f := fg.state(t); route != "gcp" || f.AppBackend != "gke" || f.LastError != "" {
		t.Fatalf("gke: route=%s follow=%+v", route, f)
	}
	fg.g.router.follow.cfg.App = "missing"
	fg.sync()
	if route, f := fg.state(t); route != "gcp" || !strings.Contains(f.LastError, "no charlie_gateway_app_write_backend series") {
		t.Fatalf("unregistered app: route=%s follow=%+v", route, f)
	}
}

// The quality canary (#147) owns the AI route: it splits requests between its
// fixed source and target and requires writeBackend to stay on the source, so
// the follower may never move writeBackend while it is configured.
func TestAIFollowRejectedWithQualityCanary(t *testing.T) {
	gu, _ := url.Parse("http://gcp")
	lu, _ := url.Parse("http://laptop")
	_, err := New(Config{Backends: []BackendConfig{{Name: "gcp", URL: gu}, {Name: "laptop", URL: lu}}, AdminToken: "admin", AIDemo: &AIDemoConfig{App: "app", Token: "ai"},
		WriteTimeout: 12 * time.Second, AIFollow: followConfig("http://gateway:9901"), AIQuality: &AIQualityConfig{}})
	if err == nil || !strings.Contains(err.Error(), "AI quality canary") {
		t.Fatalf("follow combined with the quality canary: %v", err)
	}
}

func TestAIFollowSkipsWhileCanaryOwnsRoute(t *testing.T) {
	fg := newFollowGW(t, "gke", "http://gcp", "http://laptop", nil)
	fg.sync()
	// Defense in depth behind the startup check: a canary at any stage owns
	// the route, like a write hold.
	fg.g.cfg.AIQuality = &AIQualityConfig{}
	fg.g.router.canary = &AICanaryState{Version: 3, TargetPercent: 25}
	fg.ft.fail(http.StatusInternalServerError, "")
	fg.sync() // leaves a lastError behind
	fg.ft.set("onprem", false)
	fg.sync()
	route, f := fg.state(t)
	if route != "gcp" || f.AppBackend != "onprem" || f.Target != "gcp" || f.LastError != "" || f.LastSync == nil {
		t.Fatalf("follower moved a canary-owned route: route=%s follow=%+v", route, f)
	}
	if len(fg.logs.records("AI route follows app")) != 0 {
		t.Fatal("switch logged while canary owns the route")
	}
	// Re-enabling following does not override the canary either.
	requireAIStatus(t, aiCall(fg.admin.Client(), "POST", fg.admin.URL+"/v1/follow", "admin-secret", "", `{"enabled":true}`), 200)
	if fg.route() != "gcp" {
		t.Fatalf("follow enable moved a canary-owned route: %s", fg.route())
	}
	if m := fg.metrics(t); !strings.Contains(m, `charlie_gateway_ai_follow_errors_total{reason="route"} 0`) {
		t.Fatalf("canary skip counted as an error:\n%s", m)
	}
}
