package gateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const testToken = "test-token"

// fakeBackend is one environment. It records every write body it receives.
type fakeBackend struct {
	name string
	srv  *httptest.Server

	delay        atomic.Int64 // artificial latency per request, ns
	healthStatus atomic.Int32
	activeWrites atomic.Int64
	reads        atomic.Int64
	// marked flips when the test wants to know whether writes still arrive
	// (e.g. after hold returned); markedWrites counts those.
	marked       atomic.Bool
	markedWrites atomic.Int64

	mu     sync.Mutex
	writes map[string]int // body -> times received
	gate   chan struct{}  // non-nil: writes wait until closed
}

func newFakeBackend(t *testing.T, name string) *fakeBackend {
	fb := &fakeBackend{name: name, writes: map[string]int{}}
	fb.healthStatus.Store(http.StatusOK)
	fb.srv = httptest.NewServer(http.HandlerFunc(fb.serve))
	t.Cleanup(fb.srv.Close)
	return fb
}

func (fb *fakeBackend) serve(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/healthz":
		w.WriteHeader(int(fb.healthStatus.Load()))
		return
	case "/abort":
		panic(http.ErrAbortHandler) // drop the connection without answering
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		fb.reads.Add(1)
		time.Sleep(time.Duration(fb.delay.Load()))
		if r.URL.Path == "/fail" {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		_, _ = io.WriteString(w, fb.name)
		return
	}
	fb.activeWrites.Add(1)
	defer fb.activeWrites.Add(-1)
	body, _ := io.ReadAll(r.Body)
	if fb.marked.Load() {
		fb.markedWrites.Add(1)
	}
	fb.mu.Lock()
	fb.writes[string(body)]++
	gate := fb.gate
	fb.mu.Unlock()
	if gate != nil {
		<-gate
	}
	time.Sleep(time.Duration(fb.delay.Load()))
	_, _ = io.WriteString(w, "ok")
}

// blockWrites makes writes wait until the returned func is called.
func (fb *fakeBackend) blockWrites() (unblock func()) {
	ch := make(chan struct{})
	fb.mu.Lock()
	fb.gate = ch
	fb.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			fb.mu.Lock()
			fb.gate = nil
			fb.mu.Unlock()
			close(ch)
		})
	}
}

func (fb *fakeBackend) got(body string) int {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	return fb.writes[body]
}

func (fb *fakeBackend) writeCounts() map[string]int {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	out := make(map[string]int, len(fb.writes))
	for k, v := range fb.writes {
		out[k] = v
	}
	return out
}

func (fb *fakeBackend) url(t *testing.T) *url.URL {
	u, err := url.Parse(fb.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

type testGW struct {
	g        *Gateway
	proxy    *httptest.Server
	admin    *httptest.Server
	backends map[string]*fakeBackend
	client   *Client
}

// newTestGW starts fake backends with the given names and a gateway in front
// of them. cfg.Backends is filled in; AdminToken defaults to testToken.
func newTestGW(t *testing.T, cfg Config, names ...string) *testGW {
	t.Helper()
	tg := &testGW{backends: map[string]*fakeBackend{}}
	for _, n := range names {
		fb := newFakeBackend(t, n)
		tg.backends[n] = fb
		cfg.Backends = append(cfg.Backends, BackendConfig{Name: n, URL: fb.url(t)})
	}
	if cfg.AdminToken == "" {
		cfg.AdminToken = testToken
	}
	g, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	tg.g = g
	tg.proxy = httptest.NewServer(g.Proxy())
	tg.admin = httptest.NewServer(g.Admin())
	tg.client = &Client{BaseURL: tg.admin.URL, Token: cfg.AdminToken}
	t.Cleanup(func() {
		// Dropping client connections lets writes still held return, so
		// Close does not wait for max-hold.
		tg.proxy.CloseClientConnections()
		tg.proxy.Close()
		tg.admin.Close()
		g.Close()
	})
	return tg
}

// write sends a POST with body through the gateway.
func (tg *testGW) write(ctx context.Context, body string) (*http.Response, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tg.proxy.URL+"/orders", strings.NewReader(body))
	if err != nil {
		return nil, "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b), nil
}

// asyncWrite sends a write in the background; the result arrives on the channel.
func (tg *testGW) asyncWrite(ctx context.Context, body string) <-chan writeResult {
	ch := make(chan writeResult, 1)
	go func() {
		resp, b, err := tg.write(ctx, body)
		r := writeResult{body: b, err: err}
		if resp != nil {
			r.status, r.backend = resp.StatusCode, resp.Header.Get(headerBackend)
		}
		ch <- r
	}()
	return ch
}

type writeResult struct {
	status  int
	backend string
	body    string
	err     error
}

func (tg *testGW) heldWrites() int {
	tg.g.mu.Lock()
	defer tg.g.mu.Unlock()
	return tg.g.waitingLocked()
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func recv(t *testing.T, ch <-chan writeResult) writeResult {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a write to finish")
		return writeResult{}
	}
}
