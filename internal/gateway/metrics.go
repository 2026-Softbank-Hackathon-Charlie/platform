package gateway

import (
	"github.com/prometheus/client_golang/prometheus"
)

type metrics struct {
	reg         *prometheus.Registry
	requests    *prometheus.CounterVec
	duration    *prometheus.HistogramVec
	holdExpired prometheus.Counter
	heldDropped prometheus.Counter
	readRetries *prometheus.CounterVec
}

func newMetrics(g *Gateway) *metrics {
	buckets := make([]float64, len(latencyBoundsMs))
	for i, ms := range latencyBoundsMs {
		buckets[i] = ms / 1000
	}
	m := &metrics{
		reg: prometheus.NewRegistry(),
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "charlie_gateway_requests_total",
			Help: "Proxied requests by backend, class (read or write) and status code.",
		}, []string{"backend", "class", "code"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "charlie_gateway_request_duration_seconds",
			Help:    "Time from dispatch to the backend until the response finished. Excludes time spent held.",
			Buckets: buckets,
		}, []string{"backend", "class"}),
		holdExpired: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "charlie_gateway_hold_expired_total",
			Help: "Held writes answered 503 hold_timeout, never forwarded, because they waited max-hold.",
		}),
		heldDropped: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "charlie_gateway_held_dropped_total",
			Help: "Held writes dropped because the client went away before release.",
		}),
		readRetries: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "charlie_gateway_read_retries_total",
			Help: "Reads that failed on a backend (5xx or no answer) and were sent once more to another backend, by the backend that failed.",
		}, []string{"backend"}),
	}
	m.reg.MustRegister(m.requests, m.duration, m.holdExpired, m.heldDropped, m.readRetries, stateCollector{g})
	return m
}

var (
	descInflight     = prometheus.NewDesc("charlie_gateway_inflight", "Requests being proxied right now.", []string{"class"}, nil)
	descHeldWrites   = prometheus.NewDesc("charlie_gateway_held_writes", "Write requests waiting for release.", nil, nil)
	descWritesHeld   = prometheus.NewDesc("charlie_gateway_writes_held", "1 while new writes are held.", nil, nil)
	descReadWeight   = prometheus.NewDesc("charlie_gateway_read_weight", "Share of reads (0-100) per backend.", []string{"backend"}, nil)
	descWriteBackend = prometheus.NewDesc("charlie_gateway_write_backend", "1 for the backend that receives writes.", []string{"backend"}, nil)
	descHealthy      = prometheus.NewDesc("charlie_gateway_backend_healthy", "1 when the last health check passed.", []string{"backend"}, nil)
)

// stateCollector reads routing state at scrape time, so the gauges can never
// drift from what the gateway actually does.
type stateCollector struct{ g *Gateway }

func (c stateCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{descInflight, descHeldWrites, descWritesHeld, descReadWeight, descWriteBackend, descHealthy} {
		ch <- d
	}
}

func (c stateCollector) Collect(ch chan<- prometheus.Metric) {
	g := c.g
	g.mu.Lock()
	writes, held, waiting := g.inflightWrites, g.held, g.waitingLocked()
	weights := make(map[string]int, len(g.readWeights))
	for k, v := range g.readWeights {
		weights[k] = v
	}
	writeBackend := g.writeBackend
	g.mu.Unlock()

	gauge := func(d *prometheus.Desc, v float64, labels ...string) {
		ch <- prometheus.MustNewConstMetric(d, prometheus.GaugeValue, v, labels...)
	}
	gauge(descInflight, float64(g.inflightReads.Load()), classRead)
	gauge(descInflight, float64(writes), classWrite)
	gauge(descHeldWrites, float64(waiting))
	gauge(descWritesHeld, b2f(held))
	for _, b := range g.backends {
		gauge(descReadWeight, float64(weights[b.name]), b.name)
		gauge(descWriteBackend, b2f(b.name == writeBackend), b.name)
		gauge(descHealthy, b2f(b.healthy.Load()), b.name)
	}
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
