package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
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

// TestPersistedHoldNeverExpires: a resumed hold has no timer. Long after
// heldSince + max-hold it still holds; only the writes themselves time out.
func TestPersistedHoldNeverExpires(t *testing.T) {
	file := filepath.Join(t.TempDir(), "gateway.json")
	heldSince := time.Now().Add(-time.Hour).UTC()
	if err := writeFileAtomic(file, persistedState{
		WriteBackend: "gke", ReadWeights: map[string]int{"gke": 100}, WritesHeld: true, HeldSince: &heldSince,
	}); err != nil {
		t.Fatal(err)
	}
	tg := newTestGW(t, Config{StateFile: file, MaxHold: 100 * time.Millisecond}, "gke")
	ctx := context.Background()

	if r := recv(t, tg.asyncWrite(ctx, "waited")); !r.isRetryLater("hold_timeout") {
		t.Fatalf("write held past max-hold: %+v", r)
	}
	time.Sleep(100 * time.Millisecond)
	st := tg.g.state(defaultWindow)
	if !st.WritesHeld || st.HeldSince == nil || !st.HeldSince.Equal(heldSince) {
		t.Fatalf("resumed hold ended: %+v", st)
	}
	if b, _ := os.ReadFile(file); !strings.Contains(string(b), `"writesHeld": true`) {
		t.Fatalf("state file: %s", b)
	}
	if tg.backends["gke"].got("waited") != 0 {
		t.Fatal("a timed-out write was forwarded")
	}
	// Nothing is in flight, so a hold call joins the resumed hold at once.
	if err := tg.client.HoldWrites(ctx); err != nil {
		t.Fatal(err)
	}
	if err := tg.client.ReleaseWrites(ctx); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(file); !strings.Contains(string(b), `"writesHeld": false`) {
		t.Fatalf("state file after release: %s", b)
	}
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

// TestHoldDrainTimeoutReverts: a hold that cannot drain is undone before it
// answers, because the engine releases nothing after a failed hold.
func TestHoldDrainTimeoutReverts(t *testing.T) {
	file := filepath.Join(t.TempDir(), "gateway.json")
	tg := newTestGW(t, Config{DrainTimeout: 300 * time.Millisecond, StateFile: file}, "gke", "eks")
	ctx := context.Background()
	gke := tg.backends["gke"]
	unblock := gke.blockWrites()
	defer unblock()
	slow := tg.asyncWrite(ctx, "slow")
	waitFor(t, "write in flight", func() bool { return gke.activeWrites.Load() == 1 })

	held := make(chan error, 1)
	go func() { held <- tg.client.HoldWrites(ctx) }()
	waitFor(t, "hold to start", func() bool { return tg.g.state(defaultWindow).WritesHeld })
	waiting := tg.asyncWrite(ctx, "waiting")
	waitFor(t, "write held", func() bool { return tg.heldWrites() == 1 })

	var ae *APIError
	if err := <-held; !errors.As(err, &ae) || ae.Status != http.StatusGatewayTimeout || ae.Code != "drain_timeout" {
		t.Fatalf("got %v, want 504 drain_timeout", err)
	}
	if tg.g.state(defaultWindow).WritesHeld {
		t.Fatal("a hold that did not drain must be reverted")
	}
	if b, _ := os.ReadFile(file); !strings.Contains(string(b), `"writesHeld": false`) {
		t.Fatalf("revert not persisted: %s", b)
	}
	// The waiting write goes where the in-flight one went: routing is unchanged.
	waitFor(t, "waiting write dispatched", func() bool { return gke.got("waiting") == 1 })
	unblock()
	for _, ch := range []<-chan writeResult{slow, waiting} {
		if r := recv(t, ch); r.status != http.StatusOK || r.backend != "gke" {
			t.Fatalf("write: %+v", r)
		}
	}
	if n := len(tg.backends["eks"].writeCounts()); n != 0 {
		t.Fatalf("eks got %d writes", n)
	}
}

// TestHoldRevertedWhenCallerLeaves: the caller giving up while the hold
// drains counts as a failed hold, too.
func TestHoldRevertedWhenCallerLeaves(t *testing.T) {
	tg := newTestGW(t, Config{}, "gke")
	gke := tg.backends["gke"]
	unblock := gke.blockWrites()
	defer unblock()
	slow := tg.asyncWrite(context.Background(), "slow")
	waitFor(t, "write in flight", func() bool { return gke.activeWrites.Load() == 1 })

	ctx, cancel := context.WithCancel(context.Background())
	held := make(chan error, 1)
	go func() { held <- tg.client.HoldWrites(ctx) }()
	waitFor(t, "hold to start", func() bool { return tg.g.state(defaultWindow).WritesHeld })
	cancel()
	if err := <-held; err == nil {
		t.Fatal("cancelled hold succeeded")
	}
	waitFor(t, "hold reverted", func() bool { return !tg.g.state(defaultWindow).WritesHeld })
	unblock()
	recv(t, slow)
}

// TestHoldCancelledByRelease: hold answers 200 only if the hold it started is
// still on when draining completes. A release, even one followed by a new
// hold, ends it.
func TestHoldCancelledByRelease(t *testing.T) {
	tg := newTestGW(t, Config{}, "gke")
	ctx := context.Background()
	gke := tg.backends["gke"]
	unblock := gke.blockWrites()
	defer unblock()
	slow := tg.asyncWrite(ctx, "slow")
	waitFor(t, "write in flight", func() bool { return gke.activeWrites.Load() == 1 })

	first := make(chan error, 1)
	go func() { first <- tg.client.HoldWrites(ctx) }()
	waitFor(t, "first hold", func() bool { return tg.g.state(defaultWindow).WritesHeld })
	if err := tg.client.ReleaseWrites(ctx); err != nil {
		t.Fatal(err)
	}
	second := make(chan error, 1)
	go func() { second <- tg.client.HoldWrites(ctx) }()
	waitFor(t, "second hold", func() bool {
		tg.g.mu.Lock()
		defer tg.g.mu.Unlock()
		return tg.g.gen != nil && tg.g.gen.id == 2
	})
	unblock()

	var ae *APIError
	if err := <-first; !errors.As(err, &ae) || ae.Status != http.StatusConflict || ae.Code != "hold_cancelled" {
		t.Fatalf("first hold: got %v, want 409 hold_cancelled", err)
	}
	if err := <-second; err != nil {
		t.Fatalf("second hold: %v", err)
	}
	if !tg.g.state(defaultWindow).WritesHeld {
		t.Fatal("second hold not on")
	}
	recv(t, slow)
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

// TestMaxHoldIsPerWrite: max-hold limits how long one write waits, not the
// hold. A write past it gets 503 hold_timeout and is never forwarded, and the
// hold stays on until release.
func TestMaxHoldIsPerWrite(t *testing.T) {
	const maxHold = 300 * time.Millisecond
	tg := newTestGW(t, Config{MaxHold: maxHold}, "gke", "eks")
	ctx := context.Background()
	gke, eks := tg.backends["gke"], tg.backends["eks"]
	if err := tg.client.HoldWrites(ctx); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if r := recv(t, tg.asyncWrite(ctx, "expired")); !r.isRetryLater("hold_timeout") {
		t.Fatalf("write held past max-hold: %+v", r)
	}
	if took := time.Since(start); took < maxHold {
		t.Fatalf("answered after %s, before max-hold", took)
	}
	if n := testutil.ToFloat64(tg.g.metrics.holdExpired); n != 1 {
		t.Fatalf("hold_expired_total = %v", n)
	}
	if !tg.g.state(defaultWindow).WritesHeld {
		t.Fatal("max-hold ended the hold")
	}

	// The hold is now older than max-hold; a new write still waits for release.
	later := tg.asyncWrite(ctx, "later")
	waitFor(t, "later write held", func() bool { return tg.heldWrites() == 1 })
	if gke.got("later") != 0 {
		t.Fatal("write forwarded while held")
	}
	if err := tg.client.Route(ctx, "eks"); err != nil {
		t.Fatal(err)
	}
	if err := tg.client.ReleaseWrites(ctx); err != nil {
		t.Fatal(err)
	}
	if r := recv(t, later); r.status != http.StatusOK || r.backend != "eks" {
		t.Fatalf("released write: %+v", r)
	}
	if gke.got("expired")+eks.got("expired") != 0 || gke.got("later") != 0 || eks.got("later") != 1 {
		t.Fatalf("gke %v, eks %v", gke.writeCounts(), eks.writeCounts())
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

// TestWriteOutlivesClient: a write whose client leaves keeps running at the
// backend, which may still commit it, so it stays in flight and hold waits.
func TestWriteOutlivesClient(t *testing.T) {
	tg := newTestGW(t, Config{}, "gke")
	gke := tg.backends["gke"]
	unblock := gke.blockWrites()
	defer unblock()
	cctx, cancel := context.WithCancel(context.Background())
	res := tg.asyncWrite(cctx, "client-left")
	waitFor(t, "write at the backend", func() bool { return gke.activeWrites.Load() == 1 })
	cancel()
	if r := recv(t, res); r.err == nil {
		t.Fatalf("cancelled client got %+v", r)
	}

	ctx := context.Background()
	held := make(chan error, 1)
	go func() { held <- tg.client.HoldWrites(ctx) }()
	select {
	case err := <-held:
		t.Fatalf("hold returned (%v) while the backend still ran a write", err)
	case <-time.After(200 * time.Millisecond):
	}
	if n := tg.g.state(defaultWindow).InFlight.Writes; n != 1 {
		t.Fatalf("in-flight writes = %d, want 1", n)
	}
	unblock()
	if err := <-held; err != nil {
		t.Fatal(err)
	}
	if st := tg.g.state(defaultWindow); st.InFlight.Writes != 0 || st.Stats["gke"].Requests != 1 || st.Stats["gke"].Errors != 0 {
		t.Fatalf("after the backend answered: %+v", st)
	}
	if err := tg.client.ReleaseWrites(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestWriteTimeout(t *testing.T) {
	tg := newTestGW(t, Config{WriteTimeout: 100 * time.Millisecond}, "gke")
	unblock := tg.backends["gke"].blockWrites()
	defer unblock()
	resp, body, err := tg.write(context.Background(), "stuck")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusGatewayTimeout || resp.Header.Get(headerBackend) != "gke" || !strings.Contains(body, "backend_timeout") {
		t.Fatalf("stuck write: %d %q", resp.StatusCode, body)
	}
	if n := tg.g.state(defaultWindow).InFlight.Writes; n != 0 {
		t.Fatalf("in-flight writes = %d", n)
	}
}

// TestShutdownAnswersHeldWrites: shutting down answers held writes instead of
// dropping them, and keeps the hold for the next start.
func TestShutdownAnswersHeldWrites(t *testing.T) {
	file := filepath.Join(t.TempDir(), "gateway.json")
	tg := newTestGW(t, Config{StateFile: file}, "gke")
	ctx := context.Background()
	if err := tg.client.HoldWrites(ctx); err != nil {
		t.Fatal(err)
	}
	a, b := tg.asyncWrite(ctx, "a"), tg.asyncWrite(ctx, "b")
	waitFor(t, "two held writes", func() bool { return tg.heldWrites() == 2 })

	tg.g.BeginShutdown()
	for _, ch := range []<-chan writeResult{a, b} {
		if r := recv(t, ch); !r.isRetryLater("shutting_down") {
			t.Fatalf("held write at shutdown: %+v", r)
		}
	}
	if r := recv(t, tg.asyncWrite(ctx, "c")); !r.isRetryLater("shutting_down") {
		t.Fatalf("write arriving during shutdown: %+v", r)
	}
	if n := len(tg.backends["gke"].writeCounts()); n != 0 {
		t.Fatalf("%d writes forwarded", n)
	}
	if tg.heldWrites() != 0 {
		t.Fatalf("held writes = %d", tg.heldWrites())
	}

	tg.g.Close()
	if b, _ := os.ReadFile(file); !strings.Contains(string(b), `"writesHeld": true`) {
		t.Fatalf("state file: %s", b)
	}
	g2, err := New(Config{Backends: tg.g.cfg.Backends, StateFile: file})
	if err != nil {
		t.Fatal(err)
	}
	defer g2.Close()
	if !g2.state(defaultWindow).WritesHeld {
		t.Fatal("restart did not keep holding")
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("connection reset by peer") }

func TestHeldBodyReadError(t *testing.T) {
	tg := newTestGW(t, Config{}, "gke")
	if err := tg.client.HoldWrites(context.Background()); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	tg.g.Proxy().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/orders", errReader{}))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"code":"bad_request"`) {
		t.Fatalf("body read error: %d %q", rec.Code, rec.Body.String())
	}
	if n := tg.heldWrites(); n != 0 {
		t.Fatalf("slot not given back: held writes = %d", n)
	}
	if n := testutil.ToFloat64(tg.g.metrics.heldDropped); n != 1 {
		t.Fatalf("held_dropped_total = %v", n)
	}
}

// TestHeldWriteTakesSlotBeforeBody: a held write counts against max-held
// while its body is still arriving, so max-held bounds held-write memory.
func TestHeldWriteTakesSlotBeforeBody(t *testing.T) {
	tg := newTestGW(t, Config{MaxHeld: 1}, "gke")
	ctx := context.Background()
	if err := tg.client.HoldWrites(ctx); err != nil {
		t.Fatal(err)
	}
	pr, pw := io.Pipe()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tg.proxy.URL+"/orders", pr)
	if err != nil {
		t.Fatal(err)
	}
	slow := make(chan writeResult, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			slow <- writeResult{err: err}
			return
		}
		resp.Body.Close()
		slow <- writeResult{status: resp.StatusCode}
	}()
	more := make(chan struct{})
	go func() {
		_, _ = pw.Write([]byte("part-"))
		<-more
		_, _ = pw.Write([]byte("rest"))
		pw.Close()
	}()
	waitFor(t, "slot taken while the body is still arriving", func() bool { return tg.heldWrites() == 1 })

	resp, body, err := tg.write(ctx, "second")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(body, "too_many_held_writes") {
		t.Fatalf("second write: %d %q", resp.StatusCode, body)
	}
	close(more)
	waitFor(t, "body read", func() bool {
		tg.g.mu.Lock()
		defer tg.g.mu.Unlock()
		return tg.g.buffering == 0 && tg.g.gen.waiting == 1
	})
	if err := tg.client.ReleaseWrites(ctx); err != nil {
		t.Fatal(err)
	}
	if r := recv(t, slow); r.status != http.StatusOK {
		t.Fatalf("slow-body write: %+v", r)
	}
	if gke := tg.backends["gke"]; gke.got("part-rest") != 1 || gke.got("second") != 0 {
		t.Fatalf("gke got %v", gke.writeCounts())
	}
}

func TestForwardedProto(t *testing.T) {
	tg := newTestGW(t, Config{}, "gke")
	get := func(proto string) string {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, tg.proxy.URL+"/forwarded-proto", nil)
		if proto != "" {
			req.Header.Set("X-Forwarded-Proto", proto)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}
	if got := get("https"); got != "https" {
		t.Fatalf("cloudflared's https became %q", got)
	}
	if got := get(""); got != "http" {
		t.Fatalf("without the header: %q", got)
	}
}

// TestRestoreRemovedBackend: a backend dropped from the config does not stop a
// restart unless it still has reads or writes.
func TestRestoreRemovedBackend(t *testing.T) {
	var backends []BackendConfig
	for _, n := range []string{"gke", "eks"} {
		backends = append(backends, BackendConfig{Name: n, URL: newFakeBackend(t, n).url(t)})
	}
	start := func(st persistedState) (*Gateway, string, error) {
		file := filepath.Join(t.TempDir(), "gateway.json")
		if err := writeFileAtomic(file, st); err != nil {
			t.Fatal(err)
		}
		g, err := New(Config{Backends: backends, StateFile: file})
		return g, file, err
	}

	g, file, err := start(persistedState{WriteBackend: "eks", ReadWeights: map[string]int{"gke": 30, "eks": 70, "aks": 0}})
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	if st := g.state(defaultWindow); st.WriteBackend != "eks" || len(st.ReadWeights) != 2 || st.ReadWeights["gke"] != 30 || st.ReadWeights["eks"] != 70 {
		t.Fatalf("after restart: %+v", st)
	}
	if b, _ := os.ReadFile(file); strings.Contains(string(b), "aks") {
		t.Fatalf("state file still names the removed backend: %s", b)
	}

	for _, st := range []persistedState{
		{WriteBackend: "gke", ReadWeights: map[string]int{"gke": 90, "aks": 10}},
		{WriteBackend: "aks", ReadWeights: map[string]int{"gke": 100, "aks": 0}},
	} {
		if _, _, err := start(st); err == nil || !strings.Contains(err.Error(), `"aks"`) || !strings.Contains(err.Error(), "not configured") {
			t.Errorf("state %+v: got %v, want a refusal naming aks", st, err)
		}
	}
}
