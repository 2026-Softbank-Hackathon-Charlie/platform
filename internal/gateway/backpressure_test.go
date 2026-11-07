package gateway

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestReleaseRespectsWriteConcurrencyAndNextDrain(t *testing.T) {
	tg := newTestGW(t, Config{MaxConcurrentWrites: 1, MaxHold: 3 * time.Second}, "src")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := tg.client.HoldWrites(ctx); err != nil {
		t.Fatal(err)
	}
	unblock := tg.backends["src"].blockWrites()
	defer unblock()
	results := make(chan writeResult, 3)
	for i := range 3 {
		go func() {
			resp, _, err := tg.write(ctx, fmt.Sprintf("write-%d", i))
			if err != nil {
				results <- writeResult{err: err}
			} else {
				results <- writeResult{status: resp.StatusCode}
			}
		}()
	}
	waitFor(t, "three queued held writes", func() bool { tg.g.mu.Lock(); defer tg.g.mu.Unlock(); return tg.g.gen != nil && tg.g.gen.waiting == 3 })
	if err := tg.client.ReleaseWrites(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "one active write", func() bool {
		return tg.backends["src"].activeWrites.Load() == 1 && len(tg.backends["src"].writeCounts()) == 1
	})
	if got := len(tg.backends["src"].writeCounts()); got != 1 {
		t.Fatalf("release burst bypassed limit: %d", got)
	}
	held := make(chan error, 1)
	go func() { held <- tg.client.HoldWrites(ctx) }()
	select {
	case err := <-held:
		t.Fatalf("hold missed dispatch queue: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	unblock()
	for range 3 {
		if r := recv(t, results); r.err != nil || r.status != 200 {
			t.Fatalf("%+v", r)
		}
	}
	if err := <-held; err != nil {
		t.Fatal(err)
	}
	if err := tg.client.ReleaseWrites(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestHeldBufferBudgetRejectsBeforeReadingBody(t *testing.T) {
	tg := newTestGW(t, Config{MaxBufferedBytes: heldBufferReservation - 1}, "src")
	if err := tg.client.HoldWrites(context.Background()); err != nil {
		t.Fatal(err)
	}
	resp, body, err := tg.write(context.Background(), "not-forwarded")
	if err != nil || resp.StatusCode != 503 {
		t.Fatalf("%v %s", err, body)
	}
	if tg.g.bufferedBytes.Load() != 0 || tg.heldWrites() != 0 || tg.backends["src"].got("not-forwarded") != 0 {
		t.Fatal("buffer reservation or write leaked")
	}
	if err := tg.client.ReleaseWrites(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestDispatchQueueTimeoutNeverForwards(t *testing.T) {
	tg := newTestGW(t, Config{MaxConcurrentWrites: 1, MaxHold: 100 * time.Millisecond}, "src")
	unblock := tg.backends["src"].blockWrites()
	defer unblock()
	first := tg.asyncWrite(context.Background(), "first")
	waitFor(t, "first running", func() bool { return tg.backends["src"].activeWrites.Load() == 1 })
	resp, body, err := tg.write(context.Background(), "queued")
	if err != nil || resp.StatusCode != 503 {
		t.Fatalf("%v %s", err, body)
	}
	if tg.backends["src"].got("queued") != 0 {
		t.Fatal("expired queue request was forwarded")
	}
	unblock()
	if r := recv(t, first); r.err != nil || r.status != 200 {
		t.Fatalf("%+v", r)
	}
}

func TestHeldBufferBudgetIsGlobalAcrossApps(t *testing.T) {
	tg := newTestGW(t, Config{AppDomain: testDomain, MaxBufferedBytes: heldBufferReservation}, "src")
	ctx := context.Background()
	for _, name := range []string{"shop", "blog"} {
		spec, _ := appBackends(t, name, "src")
		if err := tg.client.PutApp(ctx, name, spec); err != nil {
			t.Fatal(err)
		}
		if err := tg.client.App(name).HoldWrites(ctx); err != nil {
			t.Fatal(err)
		}
	}
	first := tg.asyncHostDo(ctx, "POST", "shop."+testDomain, "/orders", "first")
	waitFor(t, "first app holds budget", func() bool { return tg.g.bufferedBytes.Load() == heldBufferReservation })
	other := tg.hostDo(ctx, "POST", "blog."+testDomain, "/orders", "second")
	if other.err != nil || other.status != 503 {
		t.Fatalf("global budget bypassed: %+v", other)
	}
	if err := tg.client.App("shop").ReleaseWrites(ctx); err != nil {
		t.Fatal(err)
	}
	if r := recv(t, first); r.err != nil || r.status != 200 {
		t.Fatalf("%+v", r)
	}
	waitFor(t, "budget returned", func() bool { return tg.g.bufferedBytes.Load() == 0 })
	if err := tg.client.App("blog").ReleaseWrites(ctx); err != nil {
		t.Fatal(err)
	}
}
