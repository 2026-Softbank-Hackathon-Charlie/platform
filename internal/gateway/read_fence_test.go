package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type readFenceSnapshot struct {
	Supported  *bool             `json:"sourceReadExclusionSupported"`
	Exclusions map[string]string `json:"readExclusions"`
	InFlight   map[string]int    `json:"readInFlight"`
}

func readFenceState(t *testing.T, c *Client) readFenceSnapshot {
	t.Helper()
	var state readFenceSnapshot
	if err := c.do(context.Background(), http.MethodGet, "/v1/state", nil, &state); err != nil {
		t.Fatal(err)
	}
	return state
}

func fenceBody(backend, lease string) string {
	body, _ := json.Marshal(map[string]string{"backend": backend, "lease": lease})
	return string(body)
}

func newReadFenceGW(t *testing.T) *testGW {
	t.Helper()
	return newTestGW(t, Config{StateFile: filepath.Join(t.TempDir(), "state.json"), DrainTimeout: time.Second}, "src", "dst")
}

func excludeReads(t *testing.T, tg *testGW, path, backend, lease string) {
	t.Helper()
	status, code := tg.adminDo(t, http.MethodPost, path+"/v1/reads/exclude", "Bearer "+testToken, fenceBody(backend, lease))
	if status != http.StatusOK {
		t.Fatalf("exclude: %d %s", status, code)
	}
}

func TestReadExclusionCapabilityRequiresDurability(t *testing.T) {
	for _, durable := range []bool{false, true} {
		cfg := Config{}
		if durable {
			cfg.StateFile = filepath.Join(t.TempDir(), "state.json")
		}
		tg := newTestGW(t, cfg, "src")
		state := readFenceState(t, tg.client)
		if state.Supported == nil || *state.Supported != durable || len(state.InFlight) != 1 || state.InFlight["src"] != 0 {
			t.Fatalf("durable=%v: %+v", durable, state)
		}
	}
}

func TestReadExclusionLeaseAndControlGuards(t *testing.T) {
	tg := newReadFenceGW(t)
	ctx := context.Background()
	if err := tg.client.HoldWrites(ctx); err != nil {
		t.Fatal(err)
	}
	if err := tg.client.SetReadWeights(ctx, map[string]int{"dst": 100}); err != nil {
		t.Fatal(err)
	}
	excludeReads(t, tg, "", "src", "run-a")
	state := readFenceState(t, tg.client)
	if state.Exclusions["src"] != "run-a" || state.InFlight["src"] != 0 {
		t.Fatalf("missing drained source lease: %+v", state)
	}
	for _, test := range []struct{ path, body string }{
		{"/v1/reads/exclude", fenceBody("src", "run-b")},
		{"/v1/reads/include", fenceBody("src", "run-b")},
		{"/v1/route", `{"backend":"src"}`},
		{"/v1/weights", `{"readWeights":{"src":100}}`},
		{"/v1/writes/release", ""},
	} {
		status, _ := tg.adminDo(t, http.MethodPost, test.path, "Bearer "+testToken, test.body)
		if status != http.StatusConflict {
			t.Fatalf("guard %s: %d", test.path, status)
		}
	}
	status, code := tg.adminDo(t, http.MethodPost, "/v1/reads/include", "Bearer "+testToken, fenceBody("src", "run-a"))
	if status != http.StatusOK {
		t.Fatalf("include: %d %s", status, code)
	}
	if len(readFenceState(t, tg.client).Exclusions) != 0 {
		t.Fatal("matching include kept the lease")
	}
	if err := tg.client.ReleaseWrites(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestReadExclusionPreventsWriteBackendFallback(t *testing.T) {
	tg := newReadFenceGW(t)
	ctx := context.Background()
	if err := tg.client.HoldWrites(ctx); err != nil {
		t.Fatal(err)
	}
	if err := tg.client.SetReadWeights(ctx, map[string]int{"dst": 100}); err != nil {
		t.Fatal(err)
	}
	excludeReads(t, tg, "", "src", "run-a")
	tg.g.byName["dst"].healthy.Store(false)
	response := tg.hostDo(ctx, http.MethodGet, "unscoped", "/orders", "")
	if response.err != nil || response.status != http.StatusServiceUnavailable {
		t.Fatalf("no safe backend: %+v", response)
	}
	if tg.backends["src"].reads.Load() != 0 {
		t.Fatal("excluded source received a fallback read")
	}
	tg.g.byName["dst"].healthy.Store(true)
	tg.backends["dst"].readStatus.Store(http.StatusServiceUnavailable)
	response = tg.hostDo(ctx, http.MethodGet, "unscoped", "/orders", "")
	if response.err != nil || response.status != http.StatusServiceUnavailable || tg.backends["src"].reads.Load() != 0 {
		t.Fatalf("excluded source received a retry: %+v", response)
	}
}

func TestReadExclusionPersistenceFailureStaysClosed(t *testing.T) {
	tg := newReadFenceGW(t)
	if err := tg.client.HoldWrites(context.Background()); err != nil {
		t.Fatal(err)
	}
	good := tg.g.cfg.StateFile
	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocker, []byte("block"), 0o600); err != nil {
		t.Fatal(err)
	}
	tg.g.cfg.StateFile = filepath.Join(blocker, "state.json")
	defer func() { tg.g.cfg.StateFile = good }()
	status, code := tg.adminDo(t, http.MethodPost, "/v1/reads/exclude", "Bearer "+testToken, fenceBody("src", "run-a"))
	if status != http.StatusServiceUnavailable || code != "persistence_unavailable" {
		t.Fatalf("failed persistence acknowledged exclusion: %d %s", status, code)
	}
	if readFenceState(t, tg.client).Exclusions["src"] != "run-a" {
		t.Fatal("failed persistence reopened reads in memory")
	}
	status, code = tg.adminDo(t, http.MethodPost, "/v1/reads/include", "Bearer "+testToken, fenceBody("src", "run-a"))
	if status != http.StatusServiceUnavailable || code != "persistence_unavailable" || readFenceState(t, tg.client).Exclusions["src"] != "run-a" {
		t.Fatalf("failed include reopened source: %d %s", status, code)
	}
}

func TestReadExclusionRejectsInvalidInput(t *testing.T) {
	tg := newReadFenceGW(t)
	for _, body := range []string{fenceBody("src", ""), fenceBody("unknown", "run-a"), fenceBody("src", strings.Repeat("x", 129)), fenceBody("src", "bad lease")} {
		status, _ := tg.adminDo(t, http.MethodPost, "/v1/reads/exclude", "Bearer "+testToken, body)
		if status != http.StatusBadRequest {
			t.Fatalf("invalid request %q: %d", body, status)
		}
	}
}

func TestReadExclusionIsAppScopedAndRestored(t *testing.T) {
	tg := newReadFenceGW(t)
	spec, _ := appBackends(t, "shop", "src", "dst")
	ctx := context.Background()
	if err := tg.client.PutApp(ctx, "shop", spec); err != nil {
		t.Fatal(err)
	}
	shop := tg.client.App("shop")
	if err := shop.HoldWrites(ctx); err != nil {
		t.Fatal(err)
	}
	excludeReads(t, tg, "/apps/shop", "src", "run-a")
	if len(readFenceState(t, tg.client).Exclusions) != 0 || readFenceState(t, shop).Exclusions["src"] != "run-a" {
		t.Fatal("app exclusion leaked into another router")
	}
	if err := tg.client.PutApp(ctx, "shop", spec); err == nil {
		t.Fatal("a PUT replaced the excluded router")
	}
	if err := tg.client.DeleteApp(ctx, "shop"); err == nil {
		t.Fatal("a DELETE removed the excluded router")
	}
	restored, err := New(tg.g.cfg)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(restored.Admin())
	t.Cleanup(func() { server.Close(); restored.Close() })
	c := (&Client{BaseURL: server.URL, Token: testToken}).App("shop")
	state := readFenceState(t, c)
	if state.Exclusions["src"] != "run-a" || state.InFlight["src"] != 0 || state.Supported == nil || !*state.Supported {
		t.Fatalf("restart lost the read exclusion: %+v", state)
	}
	if err := c.IncludeReads(ctx, "src", "run-a"); err != nil {
		t.Fatal(err)
	}
	if err := c.IncludeReads(ctx, "src", "already-cleared"); err != nil {
		t.Fatalf("empty exclusion is not idempotent: %v", err)
	}
	st, err := loadState(tg.g.cfg.StateFile)
	if err != nil || len(st.Apps["shop"].ReadExclusions) != 0 {
		t.Fatalf("include was not durable before acknowledging: %+v %v", st, err)
	}
}

func asyncReadExclude(ctx context.Context, c *Client) <-chan error {
	done := make(chan error, 1)
	go func() { done <- c.ExcludeReads(ctx, "src", "run-a") }()
	return done
}

func TestReadExclusionDrainsSourceWhileTargetIsBusy(t *testing.T) {
	tg := newReadFenceGW(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := tg.client.HoldWrites(ctx); err != nil {
		t.Fatal(err)
	}
	tg.backends["src"].delay.Store(int64(250 * time.Millisecond))
	old := tg.asyncHostDo(ctx, http.MethodGet, "unscoped", "/orders", "")
	waitFor(t, "source read started", func() bool { return tg.backends["src"].reads.Load() == 1 })
	if err := tg.client.SetReadWeights(ctx, map[string]int{"dst": 100}); err != nil {
		t.Fatal(err)
	}
	done := asyncReadExclude(ctx, tg.client)
	waitFor(t, "source excluded", func() bool { return readFenceState(t, tg.client).Exclusions["src"] == "run-a" })
	tg.backends["dst"].delay.Store(int64(time.Second))
	current := tg.asyncHostDo(ctx, http.MethodGet, "unscoped", "/orders", "")
	waitFor(t, "target read started", func() bool { return tg.backends["dst"].reads.Load() == 1 })
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(700 * time.Millisecond):
		t.Fatal("source drain waited for the busy target")
	}
	if state := readFenceState(t, tg.client); state.InFlight["src"] != 0 || state.InFlight["dst"] == 0 {
		t.Fatalf("drain did not isolate source activity: %+v", state)
	}
	if r := <-old; r.err != nil || r.status != http.StatusOK {
		t.Fatalf("preexisting source read did not finish: %+v", r)
	}
	if r := <-current; r.err != nil || r.status != http.StatusOK {
		t.Fatalf("target read failed: %+v", r)
	}
}

func TestReadExclusionCountsReservedRetryAndSkipsIt(t *testing.T) {
	tg := newReadFenceGW(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := tg.client.HoldWrites(ctx); err != nil {
		t.Fatal(err)
	}
	if err := tg.client.SetReadWeights(ctx, map[string]int{"dst": 100}); err != nil {
		t.Fatal(err)
	}
	tg.backends["dst"].delay.Store(int64(250 * time.Millisecond))
	tg.backends["dst"].readStatus.Store(http.StatusServiceUnavailable)
	read := tg.asyncHostDo(ctx, http.MethodGet, "unscoped", "/orders", "")
	waitFor(t, "primary target read", func() bool { return tg.backends["dst"].reads.Load() == 1 })
	if readFenceState(t, tg.client).InFlight["src"] != 1 {
		t.Fatal("already selected source retry was not reserved")
	}
	done := asyncReadExclude(ctx, tg.client)
	waitFor(t, "source retry excluded", func() bool { return readFenceState(t, tg.client).Exclusions["src"] == "run-a" })
	if r := <-read; r.err != nil || r.status != http.StatusServiceUnavailable {
		t.Fatalf("failed primary did not fail closed: %+v", r)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if tg.backends["src"].reads.Load() != 0 || readFenceState(t, tg.client).InFlight["src"] != 0 {
		t.Fatal("excluded retry reached source or leaked its reservation")
	}
}

type readFenceTransport func(*http.Request) (*http.Response, error)

func (f readFenceTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestReadSelectionIsReservedBeforeExclusionCanAcknowledge(t *testing.T) {
	selected, choose, beforeDial, dial := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	var onceSelect, onceDial sync.Once
	tg := newTestGW(t, Config{
		StateFile: filepath.Join(t.TempDir(), "state.json"),
		Rand:      func(n int) int { onceSelect.Do(func() { close(selected); <-choose }); return 0 },
		Transport: readFenceTransport(func(r *http.Request) (*http.Response, error) {
			onceDial.Do(func() { close(beforeDial); <-dial })
			return http.DefaultTransport.RoundTrip(r)
		}),
	}, "src", "dst")
	var chooseOnce, dialOnce sync.Once
	defer chooseOnce.Do(func() { close(choose) })
	defer dialOnce.Do(func() { close(dial) })
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := tg.client.HoldWrites(ctx); err != nil {
		t.Fatal(err)
	}
	read := tg.asyncHostDo(ctx, http.MethodGet, "unscoped", "/orders", "")
	<-selected // Rand executes under the selection mutex.
	done := asyncReadExclude(ctx, tg.client)
	chooseOnce.Do(func() { close(choose) })
	<-beforeDial // Selection ended, but no upstream request was sent yet.
	waitFor(t, "exclusion after selection", func() bool { return readFenceState(t, tg.client).Exclusions["src"] == "run-a" })
	if readFenceState(t, tg.client).InFlight["src"] != 1 {
		t.Fatal("selected-but-not-dialed read was invisible to exclusion")
	}
	select {
	case err := <-done:
		t.Fatalf("source excluded before its reserved read finished: %v", err)
	default:
	}
	dialOnce.Do(func() { close(dial) })
	if r := <-read; r.err != nil || r.status != http.StatusOK {
		t.Fatalf("reserved read failed: %+v", r)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestReadExclusionAlsoDrainsAndSkipsHealthProbes(t *testing.T) {
	entered, unblock := make(chan struct{}), make(chan struct{})
	var calls atomic.Int64
	tg := newTestGW(t, Config{
		StateFile: filepath.Join(t.TempDir(), "state.json"),
		HealthTransport: readFenceTransport(func(r *http.Request) (*http.Response, error) {
			if calls.Add(1) == 1 {
				close(entered)
			}
			<-unblock
			return http.DefaultTransport.RoundTrip(r)
		}),
	}, "src", "dst")
	var unblockOnce sync.Once
	defer unblockOnce.Do(func() { close(unblock) })
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := tg.client.HoldWrites(ctx); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: tg.g.cfg.HealthTransport, Timeout: time.Second}
	health := make(chan struct{})
	go func() { tg.g.checkBackend(ctx, client, tg.g.byName["src"]); close(health) }()
	<-entered
	done := asyncReadExclude(ctx, tg.client)
	waitFor(t, "source excluded during health probe", func() bool { return readFenceState(t, tg.client).Exclusions["src"] == "run-a" })
	if readFenceState(t, tg.client).InFlight["src"] != 1 {
		t.Fatal("source health probe was not included in drain")
	}
	tg.g.checkBackend(ctx, client, tg.g.byName["src"])
	if calls.Load() != 1 {
		t.Fatal("another health probe was sent to the excluded source")
	}
	unblockOnce.Do(func() { close(unblock) })
	<-health
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestReadExclusionDrainTimeoutRetainsOwner(t *testing.T) {
	tg := newReadFenceGW(t)
	tg.g.cfg.DrainTimeout = 30 * time.Millisecond
	if err := tg.client.HoldWrites(context.Background()); err != nil {
		t.Fatal(err)
	}
	tg.backends["src"].delay.Store(int64(150 * time.Millisecond))
	read := tg.asyncHostDo(context.Background(), http.MethodGet, "unscoped", "/orders", "")
	waitFor(t, "source pending", func() bool { return tg.backends["src"].reads.Load() == 1 })
	status, code := tg.adminDo(t, http.MethodPost, "/v1/reads/exclude", "Bearer "+testToken, fenceBody("src", "run-a"))
	if status != http.StatusServiceUnavailable || code != "read_drain_timeout" || readFenceState(t, tg.client).Exclusions["src"] != "run-a" {
		t.Fatalf("drain timeout did not preserve its owner: %d %s", status, code)
	}
	<-read
	excludeReads(t, tg, "", "src", "run-a")
}

func TestReadExclusionIncludeCancelsTheOldDrainAcknowledgment(t *testing.T) {
	tg := newReadFenceGW(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := tg.client.HoldWrites(ctx); err != nil {
		t.Fatal(err)
	}
	tg.backends["src"].delay.Store(int64(200 * time.Millisecond))
	read := tg.asyncHostDo(ctx, http.MethodGet, "unscoped", "/orders", "")
	waitFor(t, "source before cancelled exclusion", func() bool { return tg.backends["src"].reads.Load() == 1 })
	done := asyncReadExclude(ctx, tg.client)
	waitFor(t, "exclusion to cancel", func() bool { return readFenceState(t, tg.client).Exclusions["src"] == "run-a" })
	if err := tg.client.IncludeReads(ctx, "src", "run-a"); err != nil {
		t.Fatal(err)
	}
	wantAPIError(t, <-done, http.StatusConflict, "read_exclusion_conflict")
	<-read
}

func TestReadExclusionRequiresDrainedWriteHold(t *testing.T) {
	tg := newReadFenceGW(t)
	status, code := tg.adminDo(t, http.MethodPost, "/v1/reads/exclude", "Bearer "+testToken, fenceBody("src", "run-a"))
	if status != http.StatusConflict || code != "writes_not_drained" {
		t.Fatalf("unheld exclusion: %d %s", status, code)
	}
	unblock := tg.backends["src"].blockWrites()
	defer unblock()
	write := tg.asyncWrite(context.Background(), "existing-write")
	waitFor(t, "write already running", func() bool { return tg.backends["src"].activeWrites.Load() == 1 })
	held := make(chan error, 1)
	go func() { held <- tg.client.HoldWrites(context.Background()) }()
	waitFor(t, "write hold while draining", func() bool { return tg.g.state(defaultWindow).WritesHeld })
	status, code = tg.adminDo(t, http.MethodPost, "/v1/reads/exclude", "Bearer "+testToken, fenceBody("src", "run-a"))
	if status != http.StatusConflict || code != "writes_not_drained" {
		t.Fatalf("undrained exclusion: %d %s", status, code)
	}
	unblock()
	<-write
	if err := <-held; err != nil {
		t.Fatal(err)
	}
}

func TestReadExclusionInclusionSurvivesConcurrentBackgroundSave(t *testing.T) {
	tg := newReadFenceGW(t)
	ctx := context.Background()
	if err := tg.client.HoldWrites(ctx); err != nil {
		t.Fatal(err)
	}
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
				tg.g.save()
			}
		}
	}()
	defer func() { close(stop); <-done }()
	for range 20 {
		if err := tg.client.ExcludeReads(ctx, "src", "run-a"); err != nil {
			t.Fatal(err)
		}
		if err := tg.client.IncludeReads(ctx, "src", "run-a"); err != nil {
			t.Fatal(err)
		}
		st, err := loadState(tg.g.cfg.StateFile)
		if err != nil || len(st.ReadExclusions) != 0 {
			t.Fatalf("background save restored an acknowledged inclusion's old lease: %+v %v", st, err)
		}
	}
}

func TestDeletedAppGenerationDrainsBeforeNameReuse(t *testing.T) {
	tg := newTestGW(t, Config{StateFile: filepath.Join(t.TempDir(), "state.json"), AppDomain: testDomain}, "src")
	spec, backends := appBackends(t, "shop", "src")
	ctx := context.Background()
	if err := tg.client.PutApp(ctx, "shop", spec); err != nil {
		t.Fatal(err)
	}
	old := tg.g.lookup("shop")
	backends["src"].delay.Store(int64(200 * time.Millisecond))
	read := tg.asyncHostDo(ctx, http.MethodGet, "shop."+testDomain, "/orders", "")
	waitFor(t, "old app read started", func() bool { return backends["src"].reads.Load() == 1 })
	if err := tg.client.DeleteApp(ctx, "shop"); err != nil {
		t.Fatal(err)
	}
	wantAPIError(t, tg.client.PutApp(ctx, "shop", spec), http.StatusConflict, "app_draining")
	st, err := loadState(tg.g.cfg.StateFile)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := st.Apps["shop"]; exists {
		t.Fatal("deleted app remained registered on disk")
	}
	if r := <-read; r.err != nil || r.status != http.StatusOK {
		t.Fatalf("existing read did not finish after deletion: %+v", r)
	}
	waitFor(t, "old generation read reservation released", func() bool {
		old.mu.Lock()
		defer old.mu.Unlock()
		return !old.drainingLocked()
	})
	if err := tg.client.PutApp(ctx, "shop", spec); err != nil {
		t.Fatal(err)
	}
	if tg.g.lookup("shop") == old {
		t.Fatal("name reuse revived a retired router")
	}
}

func TestDeletedAppGenerationIncludesQueuedAndDetachedWrites(t *testing.T) {
	tg := newTestGW(t, Config{AppDomain: testDomain, MaxConcurrentWrites: 1}, "src")
	spec, backends := appBackends(t, "shop", "src")
	if err := tg.client.PutApp(context.Background(), "shop", spec); err != nil {
		t.Fatal(err)
	}
	old := tg.g.lookup("shop")
	unblock := backends["src"].blockWrites()
	defer unblock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := tg.asyncHostDo(ctx, http.MethodPost, "shop."+testDomain, "/orders", "first")
	waitFor(t, "first old write reached backend", func() bool { return backends["src"].activeWrites.Load() == 1 })
	second := tg.asyncHostDo(context.Background(), http.MethodPost, "shop."+testDomain, "/orders", "second")
	waitFor(t, "old write waiting for a dispatch slot", func() bool {
		old.mu.Lock()
		defer old.mu.Unlock()
		return old.inflightWrites == 2
	})
	cancel()
	<-first
	if err := tg.client.DeleteApp(context.Background(), "shop"); err != nil {
		t.Fatal(err)
	}
	wantAPIError(t, tg.client.PutApp(context.Background(), "shop", spec), http.StatusConflict, "app_draining")
	unblock()
	if r := <-second; r.err != nil || r.status != http.StatusOK {
		t.Fatalf("queued old write did not finish: %+v", r)
	}
	waitFor(t, "old writes finished", func() bool {
		old.mu.Lock()
		defer old.mu.Unlock()
		return !old.drainingLocked()
	})
	if err := tg.client.PutApp(context.Background(), "shop", spec); err != nil {
		t.Fatal(err)
	}
}

func TestCapturedDeletedRouterCannotStartNewAttemptsOrControls(t *testing.T) {
	tg := newTestGW(t, Config{AppDomain: testDomain}, "src")
	spec, backends := appBackends(t, "shop", "src")
	if err := tg.client.PutApp(context.Background(), "shop", spec); err != nil {
		t.Fatal(err)
	}
	old := tg.g.lookup("shop")
	if err := tg.client.DeleteApp(context.Background(), "shop"); err != nil {
		t.Fatal(err)
	}
	if err := tg.client.PutApp(context.Background(), "shop", spec); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		method, body string
		handler      func(http.ResponseWriter, *http.Request)
	}{
		{http.MethodGet, "", old.serveRead},
		{http.MethodPost, "write", old.serveWrite},
		{http.MethodPost, "", old.handleHold},
		{http.MethodPost, "", old.handleRelease},
		{http.MethodPost, `{"backend":"src"}`, old.handleRoute},
		{http.MethodPost, `{"readWeights":{"src":100}}`, old.handleWeights},
		{http.MethodPost, fenceBody("src", "run-a"), old.handleExcludeReads},
		{http.MethodPost, fenceBody("src", "run-a"), old.handleIncludeReads},
	} {
		response := httptest.NewRecorder()
		test.handler(response, httptest.NewRequest(test.method, "http://gateway/", strings.NewReader(test.body)))
		if response.Code != http.StatusNotFound {
			t.Fatalf("retired handler started work: %d %s", response.Code, response.Body.String())
		}
	}
	if backends["src"].reads.Load() != 0 || backends["src"].got("write") != 0 {
		t.Fatal("captured retired router sent an untracked upstream request")
	}
}
