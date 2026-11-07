package gateway

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// get sends a read through the gateway and returns status, backend and body.
func (tg *testGW) get(t *testing.T, method, path string) (int, http.Header, string) {
	t.Helper()
	req, _ := http.NewRequest(method, tg.proxy.URL+path, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, string(b)
}

// alternateRand makes a 50/50 split pick the first backend, then the second,
// and so on.
func alternateRand() func(int) int {
	i := 0
	return func(n int) int {
		i++
		if i%2 == 1 {
			return 0
		}
		return n - 1
	}
}

func (tg *testGW) metrics(t *testing.T) string {
	t.Helper()
	resp, err := http.Get(tg.admin.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

// A read that fails on one backend during a cutover is answered by the other,
// and the client sees nothing of the first answer.
func TestReadRetriedOnAnotherBackend(t *testing.T) {
	tg := newTestGW(t, Config{Rand: alternateRand()}, "src", "dst")
	ctx := context.Background()
	if err := tg.client.SetReadWeights(ctx, map[string]int{"src": 50, "dst": 50}); err != nil {
		t.Fatal(err)
	}
	tg.backends["src"].readStatus.Store(http.StatusInternalServerError)

	for i := range 10 {
		code, h, body := tg.get(t, http.MethodGet, "/items")
		if code != http.StatusOK || body != "dst" || h.Get(headerBackend) != "dst" || h.Get("X-Failed-Backend") != "" {
			t.Fatalf("read %d: %d %q backend=%q leaked=%q", i, code, body, h.Get(headerBackend), h.Get("X-Failed-Backend"))
		}
	}
	if got := tg.backends["src"].reads.Load(); got != 5 {
		t.Fatalf("src got %d reads, want its 5 by weight", got)
	}
	// A flushed HTTP body can reach the client before forward's deferred
	// counters run. Wait for the actual handler completion, not a timer or a
	// smaller expected count; every exact assertion below still applies.
	waitFor(t, "all read attempts accounted", func() bool { return tg.g.inflightReads.Load() == 0 })

	st, err := tg.client.State(ctx, 20*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	// Windows count every answer, so a gate still sees src failing.
	if s := st.Stats["src"]; s.Requests != 5 || s.Errors != 5 {
		t.Fatalf("src window %+v", s)
	}
	// Totals count what clients received.
	src, dst := st.Totals["src"], st.Totals["dst"]
	if src.Requests != 0 || src.Errors != 0 || src.Retried == nil || *src.Retried != 5 {
		t.Fatalf("src totals %+v", src)
	}
	if dst.Requests != 10 || dst.Errors != 0 {
		t.Fatalf("dst totals %+v", dst)
	}
	m := tg.metrics(t)
	for _, want := range []string{
		`charlie_gateway_read_retries_total{backend="src"} 5`,
		`charlie_gateway_requests_total{backend="src",class="read",code="500"} 5`,
		`charlie_gateway_requests_total{backend="dst",class="read",code="200"} 10`,
	} {
		if !strings.Contains(m, want) {
			t.Errorf("metrics missing %s", want)
		}
	}
}

// A backend that drops the connection (502 from the gateway) is retried too,
// and so are HEAD and streamed answers.
func TestReadRetryOnDroppedConnectionHeadAndStream(t *testing.T) {
	tg := newTestGW(t, Config{Rand: alternateRand()}, "src", "dst")
	if err := tg.client.SetReadWeights(context.Background(), map[string]int{"src": 50, "dst": 50}); err != nil {
		t.Fatal(err)
	}
	tg.backends["dst"].dropReads.Store(true)
	for i := range 4 {
		if code, h, body := tg.get(t, http.MethodGet, "/items"); code != http.StatusOK || body != "src" || h.Get(headerBackend) != "src" {
			t.Fatalf("read %d: %d %q", i, code, body)
		}
		if code, h, _ := tg.get(t, http.MethodHead, "/items"); code != http.StatusOK || h.Get(headerBackend) != "src" {
			t.Fatalf("head %d: %d", i, code)
		}
	}
	tg.backends["dst"].dropReads.Store(false)
	for i := range 2 {
		if code, _, body := tg.get(t, http.MethodGet, "/stream"); code != http.StatusOK || (body != "src-1;src-2" && body != "dst-1;dst-2") {
			t.Fatalf("stream %d: %d %q", i, code, body)
		}
	}
}

// Outside a hold, a backend without read weight may not have the data yet, so
// a failed read is not sent there; during a hold it is.
func TestReadRetryToUnweightedBackendOnlyWhileHeld(t *testing.T) {
	tg := newTestGW(t, Config{}, "src", "dst") // src: all reads and writes
	ctx := context.Background()
	tg.backends["src"].readStatus.Store(http.StatusServiceUnavailable)

	code, h, body := tg.get(t, http.MethodGet, "/items")
	if code != http.StatusServiceUnavailable || !strings.Contains(body, "boom-src") || h.Get("X-Failed-Backend") != "src" {
		t.Fatalf("not held: %d %q, want src's own 503", code, body)
	}
	if tg.backends["dst"].reads.Load() != 0 {
		t.Fatal("a read went to dst, which has no read weight")
	}

	if err := tg.client.HoldWrites(ctx); err != nil {
		t.Fatal(err)
	}
	if code, _, body := tg.get(t, http.MethodGet, "/items"); code != http.StatusOK || body != "dst" {
		t.Fatalf("held: %d %q, want dst", code, body)
	}
	if err := tg.client.ReleaseWrites(ctx); err != nil {
		t.Fatal(err)
	}
}

// Only failures are retried, only once, and never writes.
func TestReadRetryLimits(t *testing.T) {
	tg := newTestGW(t, Config{Rand: alternateRand()}, "src", "dst")
	ctx := context.Background()
	if err := tg.client.SetReadWeights(ctx, map[string]int{"src": 50, "dst": 50}); err != nil {
		t.Fatal(err)
	}

	// A client error is the answer, not a failure.
	tg.backends["src"].readStatus.Store(http.StatusNotFound)
	if code, h, _ := tg.get(t, http.MethodGet, "/items"); code != http.StatusNotFound || h.Get(headerBackend) != "src" {
		t.Fatalf("404: %d from %q", code, h.Get(headerBackend))
	}
	if tg.backends["dst"].reads.Load() != 0 {
		t.Fatal("a 404 was retried")
	}

	// Both failing: one retry, and the client gets the second answer.
	tg.backends["src"].readStatus.Store(http.StatusBadGateway)
	tg.backends["dst"].readStatus.Store(http.StatusBadGateway)
	code, h, body := tg.get(t, http.MethodGet, "/items")
	if failed := h.Get("X-Failed-Backend"); code != http.StatusBadGateway || failed == "" || !strings.Contains(body, "boom-"+failed) {
		t.Fatalf("both failing: %d %q from %q", code, body, failed)
	}
	if src, dst := tg.backends["src"].reads.Load(), tg.backends["dst"].reads.Load(); src+dst != 3 {
		t.Fatalf("reads src=%d dst=%d, want one retry", src, dst)
	}

	// Writes are never sent twice.
	if err := tg.client.Route(ctx, "dst"); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(tg.proxy.URL+"/abort", "text/plain", strings.NewReader("w1"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway || tg.backends["src"].got("w1") != 0 {
		t.Fatalf("write: %d, src got it %d times", resp.StatusCode, tg.backends["src"].got("w1"))
	}
}
