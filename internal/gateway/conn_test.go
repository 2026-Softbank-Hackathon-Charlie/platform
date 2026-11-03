package gateway

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// countConns counts new TCP connections a backend accepts.
func countConns(fb *fakeBackend) *atomic.Int64 {
	var n atomic.Int64
	fb.srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			n.Add(1)
		}
	}
	return &n
}

func newCountedGW(t *testing.T, cfg Config) (*testGW, *atomic.Int64) {
	t.Helper()
	fb := &fakeBackend{name: "gke", writes: map[string]int{}}
	fb.healthStatus.Store(http.StatusOK)
	fb.srv = httptest.NewUnstartedServer(http.HandlerFunc(fb.serve))
	conns := countConns(fb)
	fb.srv.Start()
	t.Cleanup(fb.srv.Close)
	cfg.Backends = []BackendConfig{{Name: "gke", URL: fb.url(t)}}
	cfg.AdminToken = testToken
	g, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	tg := &testGW{g: g, backends: map[string]*fakeBackend{"gke": fb}, proxy: httptest.NewServer(g.Proxy())}
	t.Cleanup(func() { tg.proxy.Close(); g.Close() })
	return tg, conns
}

func (tg *testGW) read(t *testing.T) {
	t.Helper()
	resp, err := http.Get(tg.proxy.URL + "/items")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

// Every health check dials (and so resolves the backend's name) again; it
// never rides on a pooled proxy connection that may point at an old address.
func TestHealthChecksDialEveryTime(t *testing.T) {
	tg, conns := newCountedGW(t, Config{})
	client := &http.Client{Transport: tg.g.cfg.HealthTransport, Timeout: healthTimeout}
	for range 3 {
		tg.g.router.checkBackend(context.Background(), client, tg.g.router.backends[0])
	}
	waitFor(t, "three health connections", func() bool { return conns.Load() == 3 })

	// Proxied requests still reuse their connection.
	tg.read(t)
	tg.read(t)
	time.Sleep(20 * time.Millisecond)
	if n := conns.Load(); n != 4 {
		t.Fatalf("connections after two proxied reads: %d, want 4 (one reused)", n)
	}
}

// recycleConns makes the next request dial again.
func TestRecycleClosesIdleConnections(t *testing.T) {
	tg, conns := newCountedGW(t, Config{})
	tg.read(t)
	tg.read(t)
	waitFor(t, "one proxy connection", func() bool { return conns.Load() == 1 })
	tg.g.recycleConns()
	tg.read(t)
	waitFor(t, "a new connection after recycling", func() bool { return conns.Load() == 2 })
}

// Run recycles on its own every ConnRecycle.
func TestRunRecyclesPeriodically(t *testing.T) {
	tg, conns := newCountedGW(t, Config{ConnRecycle: 20 * time.Millisecond, HealthInterval: time.Hour})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { tg.g.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	waitFor(t, "the first health check", func() bool { return conns.Load() >= 1 })
	start := conns.Load()
	tg.read(t)
	time.Sleep(80 * time.Millisecond) // several recycle rounds
	tg.read(t)
	waitFor(t, "a fresh proxy connection", func() bool { return conns.Load() == start+2 })
}

// A name that does not resolve is reported as a DNS problem.
func TestHealthReportsDNSFailure(t *testing.T) {
	dnsFail := &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) {
		return nil, &net.DNSError{Err: "no such host", Name: "shop.gke.hopcharlie.internal", IsNotFound: true}
	}}
	tg, _ := newCountedGW(t, Config{Transport: dnsFail})
	b := tg.g.router.backends[0]
	client := &http.Client{Transport: tg.g.cfg.HealthTransport, Timeout: healthTimeout}
	tg.g.router.checkBackend(context.Background(), client, b)
	if b.healthy.Load() {
		t.Fatal("still healthy")
	}
	st := tg.g.router.state(time.Second)
	if e := st.Backends[0].LastError; e == nil || !strings.HasPrefix(*e, "dns: ") || !strings.Contains(*e, "shop.gke.hopcharlie.internal") {
		t.Fatalf("lastError %v", e)
	}
	if probeError(errors.New("connection refused")) != "connection refused" || probeError(nil) != "" {
		t.Fatal("other errors must pass through")
	}
}

// A health change drops pooled connections: the backend may have moved.
func TestHealthChangeRecyclesConnections(t *testing.T) {
	tg, conns := newCountedGW(t, Config{})
	tg.read(t)
	waitFor(t, "one proxy connection", func() bool { return conns.Load() == 1 })
	client := &http.Client{Transport: tg.g.cfg.HealthTransport, Timeout: healthTimeout}
	b := tg.g.router.backends[0]
	tg.backends["gke"].healthStatus.Store(http.StatusServiceUnavailable)
	tg.g.router.checkBackend(context.Background(), client, b) // healthy -> unhealthy
	tg.backends["gke"].healthStatus.Store(http.StatusOK)
	tg.g.router.checkBackend(context.Background(), client, b) // back to healthy
	before := conns.Load()                                    // 1 proxy + 2 health
	tg.read(t)
	waitFor(t, "a new proxy connection", func() bool { return conns.Load() == before+1 })
}

func TestDefaultsKeepProxyAndHealthTransportsApart(t *testing.T) {
	var c Config
	c.setDefaults()
	p, ok1 := c.Transport.(*http.Transport)
	h, ok2 := c.HealthTransport.(*http.Transport)
	if !ok1 || !ok2 || p == h || p.DisableKeepAlives || !h.DisableKeepAlives || p.IdleConnTimeout != DefaultConnRecycle || c.ConnRecycle != DefaultConnRecycle {
		t.Fatalf("proxy %+v health %+v", p, h)
	}
	// A custom round tripper (tests, wrappers) is used for both.
	rt := roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("x") })
	c = Config{Transport: rt}
	c.setDefaults()
	if c.HealthTransport == nil {
		t.Fatal("no health transport")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
