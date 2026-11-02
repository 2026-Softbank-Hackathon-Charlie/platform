package gateway

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	enginegw "github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/gateway"
)

// Read phases of the load test.
const (
	phaseBefore = 0 // all reads on gke
	phaseHalf   = 2 // 50/50
	phaseRouted = 4 // everything on eks
)

// TestCutoverUnderLoad runs a full cutover while clients keep sending reads
// and writes: no client request may fail, no write may be lost, duplicated
// or sent to the old backend after the hold returned.
//
// When the fence outlives max-hold, writes that waited that long get 503
// hold_timeout instead (the client retries); none of them is ever forwarded,
// and no write reaches the old backend after the hold either way.
func TestCutoverUnderLoad(t *testing.T) {
	if testing.Short() {
		t.Skip("load test")
	}
	t.Run("fence within max-hold", func(t *testing.T) {
		testCutoverUnderLoad(t, Config{}, 300*time.Millisecond, false)
	})
	t.Run("fence outlives max-hold", func(t *testing.T) {
		testCutoverUnderLoad(t, Config{MaxHold: 150 * time.Millisecond}, 800*time.Millisecond, true)
	})
}

func testCutoverUnderLoad(t *testing.T, cfg Config, fence time.Duration, wantTimeouts bool) {
	const (
		writers = 16
		readers = 16
	)
	tg := newTestGW(t, cfg, "gke", "eks")
	gke, eks := tg.backends["gke"], tg.backends["eks"]
	for _, fb := range []*fakeBackend{gke, eks} {
		fb.delay.Store(int64(3 * time.Millisecond))
	}
	eng := enginegw.NewHTTP(tg.admin.URL, testToken)
	ctx := context.Background()
	hc := &http.Client{
		Transport: &http.Transport{MaxIdleConnsPerHost: writers + readers},
		Timeout:   30 * time.Second,
	}

	var (
		stop      atomic.Bool
		wg        sync.WaitGroup
		nextID    atomic.Int64
		failures  atomic.Int64
		holdDone  atomic.Bool  // set once HoldWrites returned
		phase     atomic.Int32 // phaseBefore, phaseHalf, phaseRouted; odd while changing
		maxWrite  atomic.Int64 // slowest write seen by a client, ns
		mu        sync.Mutex
		failMsgs  []string
		acked     = map[string]bool{}
		afterHold []string // writes sent after HoldWrites returned
		timedOut  []string // writes answered 503 hold_timeout
		reads     [phaseRouted + 1]map[string]int
	)
	for i := range reads {
		reads[i] = map[string]int{}
	}
	fail := func(format string, args ...any) {
		failures.Add(1)
		mu.Lock()
		if len(failMsgs) < 5 {
			failMsgs = append(failMsgs, fmt.Sprintf(format, args...))
		}
		mu.Unlock()
	}

	for range writers {
		wg.Go(func() {
			for !stop.Load() {
				id := fmt.Sprintf("w%d", nextID.Add(1))
				sentAfterHold := holdDone.Load()
				start := time.Now()
				resp, err := hc.Post(tg.proxy.URL+"/orders", "text/plain", strings.NewReader(id))
				if err != nil {
					fail("write %s: %v", id, err)
					continue
				}
				body, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				r := writeResult{status: resp.StatusCode, retryAfter: resp.Header.Get("Retry-After"), body: string(body)}
				if wantTimeouts && r.isRetryLater("hold_timeout") {
					mu.Lock()
					timedOut = append(timedOut, id)
					mu.Unlock()
					continue
				}
				if resp.StatusCode != http.StatusOK {
					fail("write %s: status %d %s", id, resp.StatusCode, body)
					continue
				}
				for d := int64(time.Since(start)); ; {
					if cur := maxWrite.Load(); d <= cur || maxWrite.CompareAndSwap(cur, d) {
						break
					}
				}
				mu.Lock()
				acked[id] = true
				if sentAfterHold {
					afterHold = append(afterHold, id)
				}
				mu.Unlock()
			}
		})
	}
	for range readers {
		wg.Go(func() {
			for !stop.Load() {
				p := phase.Load()
				resp, err := hc.Get(tg.proxy.URL + "/items")
				if err != nil {
					fail("read: %v", err)
					continue
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					fail("read: status %d", resp.StatusCode)
					continue
				}
				if p%2 == 0 && phase.Load() == p { // skip reads that overlap a change
					mu.Lock()
					reads[p][resp.Header.Get(headerBackend)]++
					mu.Unlock()
				}
			}
		})
	}

	time.Sleep(500 * time.Millisecond)
	phase.Store(phaseBefore + 1)
	if err := tg.client.SetReadWeights(ctx, map[string]int{"gke": 50, "eks": 50}); err != nil {
		t.Fatal(err)
	}
	phase.Store(phaseHalf)
	time.Sleep(time.Second)

	// fence
	fenceStart := time.Now()
	if err := eng.HoldWrites(ctx); err != nil {
		t.Fatalf("HoldWrites: %v", err)
	}
	holdTook := time.Since(fenceStart)
	holdDone.Store(true)
	gke.marked.Store(true)
	if n := gke.activeWrites.Load(); n != 0 {
		t.Errorf("HoldWrites returned with %d writes still running on gke", n)
	}
	time.Sleep(fence) // the engine's fence + switchover
	st, err := tg.client.State(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}

	// cutover
	phase.Store(phaseHalf + 1)
	if err := eng.Route(ctx, "eks"); err != nil {
		t.Fatalf("Route: %v", err)
	}
	phase.Store(phaseRouted)
	releaseStart := time.Now()
	if err := eng.ReleaseWrites(ctx); err != nil {
		t.Fatalf("ReleaseWrites: %v", err)
	}
	releaseTook := time.Since(releaseStart)
	writeDowntime := time.Since(fenceStart)
	time.Sleep(time.Second)
	stop.Store(true)
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if n := failures.Load(); n != 0 {
		t.Errorf("%d client requests failed, e.g. %v", n, failMsgs)
	}
	if st.HeldWrites == 0 {
		t.Error("no write was held during the fence; the test did not exercise the hold")
	}
	if n := gke.markedWrites.Load(); n != 0 {
		t.Errorf("gke received %d writes after HoldWrites returned", n)
	}
	onGke, onEks := gke.writeCounts(), eks.writeCounts()
	for id, n := range onGke {
		if n+onEks[id] != 1 {
			t.Errorf("write %s received %d times", id, n+onEks[id])
		}
	}
	for id, n := range onEks {
		if n+onGke[id] != 1 {
			t.Errorf("write %s received %d times", id, n+onGke[id])
		}
	}
	if len(onGke)+len(onEks) != len(acked) {
		t.Errorf("backends received %d distinct writes, clients got %d acks", len(onGke)+len(onEks), len(acked))
	}
	for id := range acked {
		if onGke[id]+onEks[id] != 1 {
			t.Errorf("acked write %s received %d times", id, onGke[id]+onEks[id])
		}
	}
	if len(afterHold) == 0 {
		t.Error("no write was sent after the hold")
	}
	for _, id := range afterHold {
		if onGke[id] != 0 || onEks[id] != 1 {
			t.Errorf("write %s sent after hold: gke=%d eks=%d", id, onGke[id], onEks[id])
		}
	}
	if wantTimeouts && len(timedOut) == 0 {
		t.Error("no write outlived max-hold; the test did not exercise it")
	}
	for _, id := range timedOut {
		if onGke[id]+onEks[id] != 0 {
			t.Errorf("write %s got hold_timeout but was forwarded: gke=%d eks=%d", id, onGke[id], onEks[id])
		}
	}
	if n := testutil.ToFloat64(tg.g.metrics.holdExpired); int(n) != len(timedOut) {
		t.Errorf("hold_expired_total = %v, clients saw %d hold_timeout", n, len(timedOut))
	}
	if reads[phaseBefore]["eks"] != 0 {
		t.Errorf("eks got %d reads before weights were set", reads[phaseBefore]["eks"])
	}
	half := reads[phaseHalf]
	share := float64(half["gke"]) / float64(half["gke"]+half["eks"])
	if half["gke"]+half["eks"] < 200 || share < 0.4 || share > 0.6 {
		t.Errorf("50/50 weights: gke=%d eks=%d (share %.2f)", half["gke"], half["eks"], share)
	}
	if reads[phaseRouted]["gke"] != 0 {
		t.Errorf("gke got %d reads after route to eks", reads[phaseRouted]["gke"])
	}

	t.Logf("writes: %d acked (gke %d, eks %d), %d sent after hold, %d held at release, %d hold_timeout",
		len(acked), len(onGke), len(onEks), len(afterHold), st.HeldWrites, len(timedOut))
	t.Logf("reads: before weights gke=%d; 50/50 gke=%d eks=%d (gke share %.3f); after route eks=%d",
		reads[phaseBefore]["gke"], half["gke"], half["eks"], share, reads[phaseRouted]["eks"])
	t.Logf("hold returned in %s, release in %s, write downtime %s, slowest client write %s, failed requests %d",
		holdTook.Round(time.Microsecond), releaseTook.Round(time.Microsecond),
		writeDowntime.Round(time.Millisecond), time.Duration(maxWrite.Load()).Round(time.Millisecond), failures.Load())
}
