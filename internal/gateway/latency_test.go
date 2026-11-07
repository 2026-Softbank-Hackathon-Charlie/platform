package gateway

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestEndToEndIncludesHeldTime(t *testing.T) {
	tg := newTestGW(t, Config{}, "src")
	if err := tg.client.HoldWrites(context.Background()); err != nil {
		t.Fatal(err)
	}
	result := make(chan writeResult, 1)
	go func() {
		resp, _, err := tg.write(context.Background(), "latency")
		if err != nil {
			result <- writeResult{err: err}
			return
		}
		result <- writeResult{status: resp.StatusCode}
	}()
	waitFor(t, "waiting write", func() bool { return tg.heldWrites() == 1 })
	time.Sleep(70 * time.Millisecond)
	if err := tg.client.ReleaseWrites(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r := recv(t, result); r.err != nil || r.status != 200 {
		t.Fatalf("%+v", r)
	}
	metrics := tg.metrics(t)
	if len(metrics) == 0 {
		t.Fatal("no metrics")
	}
	if n := testutil.CollectAndCount(tg.g.prom.def.endToEnd); n != 1 {
		t.Fatalf("end-to-end families: %d", n)
	}
	fs, err := tg.g.prom.reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var total, hold, backend float64
	for _, f := range fs {
		for _, m := range f.Metric {
			if m.Histogram == nil {
				continue
			}
			switch f.GetName() {
			case "charlie_gateway_end_to_end_duration_seconds":
				total += m.Histogram.GetSampleSum()
			case "charlie_gateway_hold_wait_duration_seconds":
				hold += m.Histogram.GetSampleSum()
			case "charlie_gateway_request_duration_seconds":
				backend += m.Histogram.GetSampleSum()
			}
		}
	}
	if hold < 0.06 || total < hold || total < backend {
		t.Fatalf("total=%f hold=%f backend=%f", total, hold, backend)
	}
}

func TestLongLatencyDoesNotClampAtThirtySeconds(t *testing.T) {
	s := backendStats{}
	now := time.Now()
	s.record(now, 55*time.Second, false, false)
	if got := s.window(now, 10).P99Millis; got <= 30000 {
		t.Fatalf("long tail hidden: %f", got)
	}
}

func TestPauseMetricIncludesDrainBeforeHoldAcknowledgement(t *testing.T) {
	tg := newTestGW(t, Config{}, "src")
	unblock := tg.backends["src"].blockWrites()
	defer unblock()
	first := tg.asyncWrite(context.Background(), "draining")
	waitFor(t, "inflight write", func() bool { return tg.backends["src"].activeWrites.Load() == 1 })
	held := make(chan error, 1)
	go func() { held <- tg.client.HoldWrites(context.Background()) }()
	waitFor(t, "hold started before drain", func() bool { tg.g.mu.Lock(); defer tg.g.mu.Unlock(); return tg.g.held })
	time.Sleep(70 * time.Millisecond)
	unblock()
	if r := recv(t, first); r.err != nil || r.status != 200 {
		t.Fatalf("%+v", r)
	}
	if err := <-held; err != nil {
		t.Fatal(err)
	}
	if err := tg.client.ReleaseWrites(context.Background()); err != nil {
		t.Fatal(err)
	}
	fs, err := tg.g.prom.reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fs {
		if f.GetName() == "charlie_gateway_write_pause_duration_seconds" {
			if f.Metric[0].Histogram.GetSampleSum() < 0.06 {
				t.Fatal("drain excluded from pause")
			}
			return
		}
	}
	t.Fatal("no pause measurement")
}
