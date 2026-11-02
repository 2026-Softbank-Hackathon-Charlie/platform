package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestParseBackends(t *testing.T) {
	bs, err := ParseBackends("gke=http://shop.gke.ts.net, eks=https://shop.eks.ts.net,on-prem2=http://10.0.0.1:8080")
	if err != nil {
		t.Fatal(err)
	}
	if len(bs) != 3 || bs[0].Name != "gke" || bs[1].URL.Host != "shop.eks.ts.net" || bs[2].Name != "on-prem2" {
		t.Fatalf("parsed %+v", bs)
	}
	for _, bad := range []string{"", "gke", "GKE=http://a", "gke=ftp://a", "gke=http://", "gke=http://a,gke=http://b", "g k=http://a"} {
		if _, err := ParseBackends(bad); err == nil {
			t.Errorf("ParseBackends(%q) succeeded", bad)
		}
	}
}

// sequenceRand returns 0, 1, 2, ... modulo n, so n reads hit each backend
// exactly in proportion to its weight.
func sequenceRand() func(int) int {
	var mu sync.Mutex
	i := 0
	return func(n int) int {
		mu.Lock()
		defer mu.Unlock()
		v := i % n
		i++
		return v
	}
}

func TestRouting(t *testing.T) {
	tg := newTestGW(t, Config{Rand: sequenceRand()}, "gke", "eks")
	ctx := context.Background()
	readCounts := func(method string, n int) map[string]int {
		t.Helper()
		out := map[string]int{}
		for range n {
			req, _ := http.NewRequest(method, tg.proxy.URL+"/items", nil)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("%s: status %d", method, resp.StatusCode)
			}
			out[resp.Header.Get(headerBackend)]++
		}
		return out
	}

	if got := readCounts(http.MethodGet, 10); got["gke"] != 10 {
		t.Fatalf("default: reads %v, want all on the first backend", got)
	}
	if err := tg.client.SetReadWeights(ctx, map[string]int{"gke": 30, "eks": 70}); err != nil {
		t.Fatal(err)
	}
	if got := readCounts(http.MethodGet, 100); got["gke"] != 30 || got["eks"] != 70 {
		t.Fatalf("30/70: reads %v", got)
	}
	if got := readCounts(http.MethodHead, 100); got["gke"] != 30 || got["eks"] != 70 {
		t.Fatalf("HEAD should be weighted like a read: %v", got)
	}

	// An unhealthy backend gets no reads; the rest share them.
	tg.g.byName["eks"].healthy.Store(false)
	if got := readCounts(http.MethodGet, 20); got["gke"] != 20 {
		t.Fatalf("eks unhealthy: reads %v", got)
	}
	// No healthy weighted backend left: fall back to the write backend.
	if err := tg.client.SetReadWeights(ctx, map[string]int{"eks": 100}); err != nil {
		t.Fatal(err)
	}
	if got := readCounts(http.MethodOptions, 5); got["gke"] != 5 {
		t.Fatalf("fallback: reads %v", got)
	}

	// Writes stay on the write backend even when its health check fails.
	tg.g.byName["gke"].healthy.Store(false)
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		req, _ := http.NewRequest(m, tg.proxy.URL+"/orders", strings.NewReader(m))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || resp.Header.Get(headerBackend) != "gke" {
			t.Fatalf("%s: status %d backend %q", m, resp.StatusCode, resp.Header.Get(headerBackend))
		}
		if tg.backends["gke"].got(m) != 1 || tg.backends["eks"].got(m) != 0 {
			t.Fatalf("%s did not reach gke exactly once", m)
		}
	}
}

func TestRouteAndWeightsValidation(t *testing.T) {
	tg := newTestGW(t, Config{}, "gke", "eks")
	ctx := context.Background()
	wantErr := func(err error, status int, code string) {
		t.Helper()
		var ae *APIError
		if !errors.As(err, &ae) || ae.Status != status || ae.Code != code {
			t.Fatalf("got %v, want %d %s", err, status, code)
		}
	}
	for _, w := range []map[string]int{
		{"gke": 50},
		{"gke": 50, "aks": 50},
		{"gke": -10, "eks": 110},
		{},
		nil,
	} {
		wantErr(tg.client.SetReadWeights(ctx, w), http.StatusBadRequest, "invalid_weights")
	}
	wantErr(tg.client.Route(ctx, "aks"), http.StatusBadRequest, "unknown_backend")
	wantErr(tg.client.Route(ctx, ""), http.StatusBadRequest, "unknown_backend")

	for _, body := range []string{`{`, `{"backend":"eks","extra":1}`} {
		req, _ := http.NewRequest(http.MethodPost, tg.admin.URL+"/v1/route", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+testToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("route %s: status %d", body, resp.StatusCode)
		}
	}

	if err := tg.client.SetReadWeights(ctx, map[string]int{"gke": 0, "eks": 100}); err != nil {
		t.Fatal(err)
	}
	st, err := tg.client.State(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if st.WriteBackend != "gke" || st.ReadWeights["eks"] != 100 || st.ReadWeights["gke"] != 0 {
		t.Fatalf("weights must not move writes: %+v", st)
	}
	if err := tg.client.Route(ctx, "eks"); err != nil {
		t.Fatal(err)
	}
	st, _ = tg.client.State(ctx, 0)
	if st.WriteBackend != "eks" || st.ReadWeights["eks"] != 100 || st.ReadWeights["gke"] != 0 {
		t.Fatalf("after route: %+v", st)
	}
}

func TestAdminAuth(t *testing.T) {
	tg := newTestGW(t, Config{}, "gke")
	do := func(method, path, auth string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(method, tg.admin.URL+path, nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var e struct {
			Error struct{ Code string } `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&e)
		return resp.StatusCode, e.Error.Code
	}
	for _, p := range []string{"/v1/state", "/v1/writes/hold", "/v1/writes/release", "/v1/route", "/v1/weights"} {
		method := http.MethodPost
		if p == "/v1/state" {
			method = http.MethodGet
		}
		for _, auth := range []string{"", "Bearer wrong", "Basic " + testToken, testToken} {
			if status, code := do(method, p, auth); status != http.StatusUnauthorized || code != "unauthorized" {
				t.Errorf("%s %s with %q: %d %s", method, p, auth, status, code)
			}
		}
	}
	if status, _ := do(http.MethodGet, "/v1/state", "Bearer "+testToken); status != http.StatusOK {
		t.Errorf("valid token: %d", status)
	}
	for _, p := range []string{"/healthz", "/metrics"} {
		if status, _ := do(http.MethodGet, p, ""); status != http.StatusOK {
			t.Errorf("%s without token: %d", p, status)
		}
	}
	if err := (&Client{BaseURL: tg.admin.URL}).HoldWrites(context.Background()); err == nil {
		t.Error("client without token could hold writes")
	}
}

func TestPersistenceAcrossRestart(t *testing.T) {
	file := filepath.Join(t.TempDir(), "state", "gateway.json")
	ctx := context.Background()
	tg := newTestGW(t, Config{StateFile: file}, "gke", "eks", "onprem")
	if err := tg.client.Route(ctx, "eks"); err != nil {
		t.Fatal(err)
	}
	if err := tg.client.SetReadWeights(ctx, map[string]int{"eks": 80, "onprem": 20}); err != nil {
		t.Fatal(err)
	}
	tg.g.Close()

	var onDisk persistedState
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &onDisk); err != nil {
		t.Fatal(err)
	}
	if onDisk.WriteBackend != "eks" || onDisk.ReadWeights["onprem"] != 20 || onDisk.WritesHeld {
		t.Fatalf("state file: %s", b)
	}

	// The initial backend only applies without a state file.
	g2, err := New(Config{Backends: tg.g.cfg.Backends, StateFile: file, InitialBackend: "gke"})
	if err != nil {
		t.Fatal(err)
	}
	defer g2.Close()
	st := g2.state(defaultWindow)
	if st.WriteBackend != "eks" || st.ReadWeights["eks"] != 80 || st.ReadWeights["onprem"] != 20 || st.WritesHeld {
		t.Fatalf("after restart: %+v", st)
	}

	// A state file naming a backend that is no longer configured is refused.
	if _, err := New(Config{Backends: tg.g.cfg.Backends[:1], StateFile: file}); err == nil {
		t.Fatal("started with a state file pointing at an unconfigured backend")
	}
	// So is a state file that cannot be written: better now than mid-cutover.
	if _, err := New(Config{Backends: tg.g.cfg.Backends, StateFile: filepath.Join(file, "nested.json")}); err == nil {
		t.Fatal("started with an unwritable state file")
	}
}

func TestPersistedHoldResumes(t *testing.T) {
	file := filepath.Join(t.TempDir(), "gateway.json")
	ctx := context.Background()
	tg := newTestGW(t, Config{StateFile: file, MaxHold: time.Hour}, "gke", "eks")
	if err := tg.client.HoldWrites(ctx); err != nil {
		t.Fatal(err)
	}
	heldSince := tg.g.state(defaultWindow).HeldSince
	tg.g.Close()

	tg2 := newTestGW(t, Config{StateFile: file, MaxHold: time.Hour}, "gke", "eks")
	st := tg2.g.state(defaultWindow)
	if !st.WritesHeld || st.HeldSince == nil || !st.HeldSince.Equal(*heldSince) {
		t.Fatalf("hold not resumed: %+v (heldSince was %v)", st, heldSince)
	}
	res := tg2.asyncWrite(ctx, "after-restart")
	waitFor(t, "write to be held", func() bool { return tg2.heldWrites() == 1 })
	if tg2.backends["gke"].got("after-restart") != 0 {
		t.Fatal("held write reached the backend")
	}
	if err := tg2.client.ReleaseWrites(ctx); err != nil {
		t.Fatal(err)
	}
	if r := recv(t, res); r.status != http.StatusOK || tg2.backends["gke"].got("after-restart") != 1 {
		t.Fatalf("after release: %+v", r)
	}
}

func TestPersistedHoldKeepsRemainingMaxHold(t *testing.T) {
	writeState := func(t *testing.T, heldSince time.Time) string {
		file := filepath.Join(t.TempDir(), "gateway.json")
		hs := heldSince.UTC()
		if err := writeFileAtomic(file, persistedState{
			WriteBackend: "gke", ReadWeights: map[string]int{"gke": 100}, WritesHeld: true, HeldSince: &hs,
		}); err != nil {
			t.Fatal(err)
		}
		return file
	}

	t.Run("remaining", func(t *testing.T) {
		const maxHold = 3 * time.Second
		file := writeState(t, time.Now().Add(-maxHold+300*time.Millisecond))
		start := time.Now()
		tg := newTestGW(t, Config{StateFile: file, MaxHold: maxHold}, "gke")
		if !tg.g.state(defaultWindow).WritesHeld {
			t.Fatal("hold not resumed")
		}
		waitFor(t, "auto release", func() bool { return !tg.g.state(defaultWindow).WritesHeld })
		// A timer restarted from zero would hold for the full 3s.
		if took := time.Since(start); took > 2*time.Second {
			t.Fatalf("released after %s; max-hold must count from heldSince", took)
		}
		if n := testutil.ToFloat64(tg.g.metrics.holdExpired); n != 1 {
			t.Fatalf("hold_expired_total = %v", n)
		}
	})

	t.Run("already expired", func(t *testing.T) {
		file := writeState(t, time.Now().Add(-time.Hour))
		tg := newTestGW(t, Config{StateFile: file, MaxHold: time.Minute}, "gke")
		waitFor(t, "immediate release, persisted", func() bool {
			b, _ := os.ReadFile(file)
			return !tg.g.state(defaultWindow).WritesHeld && strings.Contains(string(b), `"writesHeld": false`)
		})
	})
}

func TestHoldWaitsForInFlightWrites(t *testing.T) {
	tg := newTestGW(t, Config{}, "gke")
	ctx := context.Background()
	gke := tg.backends["gke"]
	unblock := gke.blockWrites()
	defer unblock()

	first := tg.asyncWrite(ctx, "in-flight")
	waitFor(t, "write to reach the backend", func() bool { return gke.activeWrites.Load() == 1 })

	held := make(chan error, 1)
	go func() { held <- tg.client.HoldWrites(ctx) }()
	waitFor(t, "hold to start", func() bool { return tg.g.state(defaultWindow).WritesHeld })
	second := tg.asyncWrite(ctx, "arrived-during-hold")
	waitFor(t, "second write to be held", func() bool { return tg.heldWrites() == 1 })
	select {
	case err := <-held:
		t.Fatalf("hold returned (%v) while a write was in flight", err)
	case <-time.After(100 * time.Millisecond):
	}

	unblock()
	if err := <-held; err != nil {
		t.Fatal(err)
	}
	if r := recv(t, first); r.status != http.StatusOK {
		t.Fatalf("in-flight write: %+v", r)
	}
	if gke.got("arrived-during-hold") != 0 {
		t.Fatal("held write was forwarded")
	}
	if err := tg.client.ReleaseWrites(ctx); err != nil {
		t.Fatal(err)
	}
	if r := recv(t, second); r.status != http.StatusOK || gke.got("arrived-during-hold") != 1 {
		t.Fatalf("held write after release: %+v", r)
	}
	// Both are idempotent.
	if err := tg.client.ReleaseWrites(ctx); err != nil {
		t.Fatal(err)
	}
	if err := tg.client.HoldWrites(ctx); err != nil {
		t.Fatal(err)
	}
	if err := tg.client.HoldWrites(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestHoldDrainTimeout(t *testing.T) {
	tg := newTestGW(t, Config{DrainTimeout: 100 * time.Millisecond}, "gke")
	ctx := context.Background()
	unblock := tg.backends["gke"].blockWrites()
	defer unblock()
	res := tg.asyncWrite(ctx, "slow")
	waitFor(t, "write in flight", func() bool { return tg.backends["gke"].activeWrites.Load() == 1 })

	var ae *APIError
	if err := tg.client.HoldWrites(ctx); !errors.As(err, &ae) || ae.Status != http.StatusGatewayTimeout || ae.Code != "drain_timeout" {
		t.Fatalf("got %v, want 504 drain_timeout", err)
	}
	if !tg.g.state(defaultWindow).WritesHeld {
		t.Fatal("a drain timeout must leave writes held")
	}
	unblock()
	recv(t, res)
	if err := tg.client.ReleaseWrites(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestMaxHeldRejects(t *testing.T) {
	tg := newTestGW(t, Config{MaxHeld: 2}, "gke")
	ctx := context.Background()
	if err := tg.client.HoldWrites(ctx); err != nil {
		t.Fatal(err)
	}
	a, b := tg.asyncWrite(ctx, "a"), tg.asyncWrite(ctx, "b")
	waitFor(t, "two held writes", func() bool { return tg.heldWrites() == 2 })

	resp, body, err := tg.write(ctx, "c")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") == "" || !strings.Contains(body, "too_many_held_writes") {
		t.Fatalf("third write: %d %q retry-after=%q", resp.StatusCode, body, resp.Header.Get("Retry-After"))
	}
	if err := tg.client.ReleaseWrites(ctx); err != nil {
		t.Fatal(err)
	}
	for _, ch := range []<-chan writeResult{a, b} {
		if r := recv(t, ch); r.status != http.StatusOK {
			t.Fatalf("held write: %+v", r)
		}
	}
	if tg.backends["gke"].got("c") != 0 {
		t.Fatal("rejected write was forwarded")
	}
}

func TestMaxHoldAutoRelease(t *testing.T) {
	tg := newTestGW(t, Config{MaxHold: 300 * time.Millisecond}, "gke", "eks")
	ctx := context.Background()
	if err := tg.client.HoldWrites(ctx); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	res := tg.asyncWrite(ctx, "late")
	waitFor(t, "write held", func() bool { return tg.heldWrites() == 1 })
	// Auto release goes to the write backend at that moment.
	if err := tg.client.Route(ctx, "eks"); err != nil {
		t.Fatal(err)
	}
	r := recv(t, res)
	if r.status != http.StatusOK || r.backend != "eks" || tg.backends["eks"].got("late") != 1 {
		t.Fatalf("auto-released write: %+v", r)
	}
	if took := time.Since(start); took < 200*time.Millisecond {
		t.Fatalf("released after %s, before max-hold", took)
	}
	if tg.g.state(defaultWindow).WritesHeld {
		t.Fatal("still held after max-hold")
	}
	if n := testutil.ToFloat64(tg.g.metrics.holdExpired); n != 1 {
		t.Fatalf("hold_expired_total = %v", n)
	}
}

func TestHeldClientDisconnect(t *testing.T) {
	tg := newTestGW(t, Config{}, "gke")
	if err := tg.client.HoldWrites(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	res := tg.asyncWrite(ctx, `{"id":"gone"}`)
	waitFor(t, "write held", func() bool { return tg.heldWrites() == 1 })
	cancel()
	if r := recv(t, res); r.err == nil {
		t.Fatalf("cancelled client got %+v", r)
	}
	waitFor(t, "held write dropped", func() bool {
		return testutil.ToFloat64(tg.g.metrics.heldDropped) == 1 && tg.heldWrites() == 0
	})
	if err := tg.client.ReleaseWrites(context.Background()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if tg.backends["gke"].got(`{"id":"gone"}`) != 0 {
		t.Fatal("a write whose client left was forwarded")
	}
	if st := tg.g.state(defaultWindow); st.InFlight.Writes != 0 {
		t.Fatalf("in-flight writes = %d", st.InFlight.Writes)
	}
}

func TestStateStatsAndMetrics(t *testing.T) {
	tg := newTestGW(t, Config{}, "gke", "eks")
	ctx := context.Background()
	for _, p := range []string{"/items", "/items", "/items", "/fail"} {
		resp, err := http.Get(tg.proxy.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	if _, _, err := tg.write(ctx, "w1"); err != nil {
		t.Fatal(err)
	}

	// A backend that drops the connection answers 502, counted as an error.
	if err := tg.client.Route(ctx, "eks"); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(tg.proxy.URL+"/abort", "text/plain", strings.NewReader("w2"))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if body := string(b); resp.StatusCode != http.StatusBadGateway || resp.Header.Get(headerBackend) != "eks" || !strings.Contains(body, "bad_gateway") {
		t.Fatalf("dead backend: %d %q %q", resp.StatusCode, resp.Header.Get(headerBackend), body)
	}

	st, err := tg.client.State(ctx, 20*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	gke, eks := st.Stats["gke"], st.Stats["eks"]
	if gke.WindowSeconds != 20 || gke.Requests != 5 || gke.Errors != 1 || gke.ErrorRate != 0.2 || gke.P50Millis <= 0 {
		t.Fatalf("gke stats: %+v", gke)
	}
	if eks.Requests != 1 || eks.Errors != 1 || st.Totals["eks"].Errors != 1 || st.Totals["gke"].Requests != 5 {
		t.Fatalf("eks stats %+v totals %+v", eks, st.Totals)
	}
	if st.MaxHold != DefaultMaxHold.String() || len(st.Backends) != 2 || st.Backends[1].Name != "eks" || st.UpdatedAt.IsZero() {
		t.Fatalf("state: %+v", st)
	}

	for _, w := range []string{"0s", "500ms", "301s", "abc"} {
		req, _ := http.NewRequest(http.MethodGet, tg.admin.URL+"/v1/state?window="+w, nil)
		req.Header.Set("Authorization", "Bearer "+testToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("window=%s: %d", w, resp.StatusCode)
		}
	}

	resp, err = http.Get(tg.admin.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	m, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	for _, want := range []string{
		`charlie_gateway_requests_total{backend="gke",class="read",code="200"} 3`,
		`charlie_gateway_requests_total{backend="gke",class="read",code="500"} 1`,
		`charlie_gateway_requests_total{backend="eks",class="write",code="502"} 1`,
		`charlie_gateway_request_duration_seconds_bucket{backend="gke",class="write"`,
		`charlie_gateway_inflight{class="read"} 0`,
		`charlie_gateway_inflight{class="write"} 0`,
		`charlie_gateway_held_writes 0`,
		`charlie_gateway_writes_held 0`,
		`charlie_gateway_read_weight{backend="eks"} 100`,
		`charlie_gateway_write_backend{backend="eks"} 1`,
		`charlie_gateway_write_backend{backend="gke"} 0`,
		`charlie_gateway_hold_expired_total 0`,
		`charlie_gateway_held_dropped_total 0`,
		`charlie_gateway_backend_healthy{backend="gke"} 1`,
	} {
		if !strings.Contains(string(m), want) {
			t.Errorf("metrics missing %s", want)
		}
	}
	if strings.Contains(string(m), "go_goroutines") {
		t.Error("metrics use the global registry")
	}
}

func TestHealthChecks(t *testing.T) {
	tg := newTestGW(t, Config{HealthInterval: 20 * time.Millisecond}, "gke", "eks")
	tg.backends["eks"].healthStatus.Store(http.StatusServiceUnavailable)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { tg.g.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	backend := func(name string) (healthy bool, lastErr string, checked bool) {
		for _, b := range tg.g.state(defaultWindow).Backends {
			if b.Name == name {
				if b.LastError != nil {
					lastErr = *b.LastError
				}
				return b.Healthy, lastErr, b.LastCheck != nil
			}
		}
		t.Fatalf("no backend %s", name)
		return
	}
	waitFor(t, "eks unhealthy", func() bool { h, _, c := backend("eks"); return c && !h })
	if h, e, c := backend("gke"); !h || e != "" || !c {
		t.Fatalf("gke: healthy=%v err=%q checked=%v", h, e, c)
	}
	if _, e, _ := backend("eks"); !strings.Contains(e, "503") {
		t.Fatalf("eks lastError %q", e)
	}
	tg.backends["eks"].healthStatus.Store(http.StatusOK)
	waitFor(t, "eks healthy again", func() bool { h, _, _ := backend("eks"); return h })
}

func TestClientErrorsExposeStatusAndCode(t *testing.T) {
	tg := newTestGW(t, Config{}, "gke")
	if err := tg.client.Health(context.Background()); err != nil {
		t.Fatal(err)
	}
	err := (&Client{BaseURL: tg.admin.URL, Token: "wrong"}).Route(context.Background(), "gke")
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != http.StatusUnauthorized || ae.Code != "unauthorized" || ae.Message == "" {
		t.Fatalf("got %#v", err)
	}
	if !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "unauthorized") {
		t.Fatalf("message %q", err.Error())
	}
}
