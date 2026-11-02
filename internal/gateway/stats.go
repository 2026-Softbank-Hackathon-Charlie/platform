package gateway

import (
	"sort"
	"sync"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

const (
	statsKeepSeconds = 300 // 1-second buckets kept; also the largest window
	defaultWindow    = 10 * time.Second
)

// latencyBoundsMs are the upper bounds of the latency histogram buckets. The
// same bounds back the Prometheus histogram so both views agree.
var latencyBoundsMs = [...]float64{
	1, 2, 3, 5, 7, 10, 15, 20, 30, 50, 75, 100, 150, 200, 300, 500, 750,
	1000, 1500, 2000, 3000, 5000, 10000, 30000,
}

type statsBucket struct {
	sec      int64 // unix second this bucket holds; stale buckets are reset
	requests int64
	errors   int64
	latency  [len(latencyBoundsMs) + 1]int64 // last slot: above the largest bound
}

// backendStats keeps per-second counters for one backend.
type backendStats struct {
	mu            sync.Mutex
	ring          [statsKeepSeconds]statsBucket
	totalRequests int64
	totalErrors   int64
}

func latencyBucket(ms float64) int {
	return sort.SearchFloat64s(latencyBoundsMs[:], ms) // first bound >= ms
}

func (s *backendStats) record(now time.Time, d time.Duration, isErr bool) {
	sec := now.Unix()
	ms := float64(d) / float64(time.Millisecond)
	s.mu.Lock()
	defer s.mu.Unlock()
	b := &s.ring[sec%statsKeepSeconds]
	if b.sec != sec {
		*b = statsBucket{sec: sec}
	}
	b.requests++
	b.latency[latencyBucket(ms)]++
	s.totalRequests++
	if isErr {
		b.errors++
		s.totalErrors++
	}
}

// window sums the last `seconds` buckets, including the current one.
func (s *backendStats) window(now time.Time, seconds int) apiv1.BackendStats {
	var sum statsBucket
	end := now.Unix()
	s.mu.Lock()
	for sec := end - int64(seconds) + 1; sec <= end; sec++ {
		b := &s.ring[sec%statsKeepSeconds]
		if b.sec != sec {
			continue
		}
		sum.requests += b.requests
		sum.errors += b.errors
		for i, c := range b.latency {
			sum.latency[i] += c
		}
	}
	s.mu.Unlock()

	out := apiv1.BackendStats{
		WindowSeconds: seconds,
		Requests:      sum.requests,
		Errors:        sum.errors,
		Rps:           float32(float64(sum.requests) / float64(seconds)),
		P50Millis:     float32(percentile(sum.latency[:], sum.requests, 0.50)),
		P95Millis:     float32(percentile(sum.latency[:], sum.requests, 0.95)),
		P99Millis:     float32(percentile(sum.latency[:], sum.requests, 0.99)),
	}
	if sum.requests > 0 {
		out.ErrorRate = float32(float64(sum.errors) / float64(sum.requests))
	}
	return out
}

func (s *backendStats) totals() apiv1.BackendTotals {
	s.mu.Lock()
	defer s.mu.Unlock()
	return apiv1.BackendTotals{Requests: s.totalRequests, Errors: s.totalErrors}
}

// percentile estimates the q-quantile in ms, interpolating linearly inside
// the bucket that holds it (the same way Prometheus histogram_quantile does).
func percentile(counts []int64, total int64, q float64) float64 {
	if total == 0 {
		return 0
	}
	rank := q * float64(total)
	var cum int64
	for i, c := range counts {
		if c == 0 {
			continue
		}
		if float64(cum+c) >= rank {
			if i == len(latencyBoundsMs) {
				// No upper bound to interpolate towards.
				return latencyBoundsMs[len(latencyBoundsMs)-1]
			}
			lo := 0.0
			if i > 0 {
				lo = latencyBoundsMs[i-1]
			}
			hi := latencyBoundsMs[i]
			return lo + (hi-lo)*(rank-float64(cum))/float64(c)
		}
		cum += c
	}
	return latencyBoundsMs[len(latencyBoundsMs)-1]
}
