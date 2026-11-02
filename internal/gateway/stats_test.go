package gateway

import (
	"math"
	"testing"
	"time"
)

func near(a, b float32) bool { return math.Abs(float64(a-b)) < 0.01 }

func TestStatsPercentiles(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	var s backendStats
	// 90 requests at 10ms land in (7,10]; 10 at 100ms land in (75,100].
	for i := range 90 {
		s.record(now, 10*time.Millisecond, i < 5)
	}
	for range 10 {
		s.record(now.Add(-3*time.Second), 100*time.Millisecond, false)
	}
	st := s.window(now, 10)
	if st.Requests != 100 || st.Errors != 5 || !near(st.ErrorRate, 0.05) || !near(st.Rps, 10) || st.WindowSeconds != 10 {
		t.Fatalf("window: %+v", st)
	}
	// p50: rank 50 of 90 in (7,10] -> 7 + 3*50/90.
	// p95: rank 95, 5 into the 10 in (75,100] -> 87.5; p99 -> 97.5.
	if !near(st.P50Millis, 7+3*50.0/90) || !near(st.P95Millis, 87.5) || !near(st.P99Millis, 97.5) {
		t.Fatalf("percentiles: p50=%v p95=%v p99=%v", st.P50Millis, st.P95Millis, st.P99Millis)
	}

	// The 100ms requests are 3s old: outside a 2s window.
	if st := s.window(now, 2); st.Requests != 90 || !near(st.P99Millis, 7+3*89.1/90) {
		t.Fatalf("2s window: %+v", st)
	}
	if tot := s.totals(); tot.Requests != 100 || tot.Errors != 5 {
		t.Fatalf("totals: %+v", tot)
	}
}

func TestStatsRingAndEdges(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	var s backendStats
	if st := s.window(now, 10); st.Requests != 0 || st.P99Millis != 0 || st.ErrorRate != 0 {
		t.Fatalf("empty: %+v", st)
	}
	// Same ring slot, 300s apart: the old second must not leak into the new one.
	s.record(now.Add(-statsKeepSeconds*time.Second), time.Millisecond, true)
	s.record(now, time.Millisecond, false)
	if st := s.window(now, statsKeepSeconds); st.Requests != 1 || st.Errors != 0 {
		t.Fatalf("stale bucket leaked: %+v", st)
	}
	// Above the largest bound there is nothing to interpolate towards.
	s.record(now, time.Minute, false)
	if st := s.window(now, 1); st.P99Millis != 30000 {
		t.Fatalf("overflow p99 = %v", st.P99Millis)
	}
	// Exactly on a bound belongs to that bound's bucket.
	if latencyBucket(10) != 5 || latencyBucket(10.01) != 6 || latencyBucket(0) != 0 {
		t.Fatal("bucket boundaries")
	}
	if s.totals().Requests != 3 {
		t.Fatal("totals keep everything since start")
	}
}
