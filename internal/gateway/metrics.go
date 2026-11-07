package gateway

import (
	"maps"
	"slices"

	"github.com/prometheus/client_golang/prometheus"
)

// metrics keeps two sets of the same metrics. The default app has the
// original names and labels (charlie_gateway_*), so existing dashboards and
// alerts keep meaning that app alone. Registered apps share
// charlie_gateway_app_*, which carry an app label first.
type metrics struct {
	reg  *prometheus.Registry
	def  *families
	apps *families
}

// families are the request metrics of one set; routers write to them through
// a routerMetrics view.
type families struct {
	requests    *prometheus.CounterVec
	duration    *prometheus.HistogramVec
	endToEnd    *prometheus.HistogramVec
	holdWait    *prometheus.HistogramVec
	writeQueue  *prometheus.HistogramVec
	writePause  *prometheus.HistogramVec
	holdExpired *prometheus.CounterVec
	heldDropped *prometheus.CounterVec
	readRetries *prometheus.CounterVec
	gauges      gaugeDescs
}

// routerMetrics are one app's request metrics.
type routerMetrics struct {
	requests    *prometheus.CounterVec // backend, class, code
	duration    prometheus.ObserverVec // backend, class
	endToEnd    prometheus.ObserverVec // class, code
	holdWait    prometheus.ObserverVec // outcome
	writeQueue  prometheus.ObserverVec // outcome
	writePause  prometheus.ObserverVec // outcome
	holdExpired prometheus.Counter
	heldDropped prometheus.Counter
	readRetries *prometheus.CounterVec // backend
}

func newMetrics(g *Gateway) *metrics {
	m := &metrics{
		reg:  prometheus.NewRegistry(),
		def:  newFamilies("charlie_gateway_", nil),
		apps: newFamilies("charlie_gateway_app_", []string{"app"}),
	}
	for _, f := range []*families{m.def, m.apps} {
		m.reg.MustRegister(f.requests, f.duration, f.endToEnd, f.holdWait, f.writeQueue, f.writePause, f.holdExpired, f.heldDropped, f.readRetries)
	}
	m.reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "charlie_gateway_persistence_healthy", Help: "1 when the last state save succeeded; see persistence_enabled for durable mode."}, func() float64 {
		if g.persistenceFailed.Load() {
			return 0
		}
		return 1
	}), prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "charlie_gateway_persistence_enabled", Help: "1 when a state file is configured."}, func() float64 {
		if g.cfg.StateFile != "" {
			return 1
		}
		return 0
	}))
	m.reg.MustRegister(stateCollector{g, m})
	m.reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "charlie_gateway_buffered_bytes", Help: "Conservative held-body memory reservations across all apps."}, func() float64 { return float64(g.bufferedBytes.Load()) }), prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "charlie_gateway_buffer_budget_bytes", Help: "Global held-body reservation limit."}, func() float64 { return float64(g.cfg.MaxBufferedBytes) }))
	return m
}

// newFamilies builds one set; app are the labels in front of the usual ones.
func newFamilies(prefix string, app []string) *families {
	help := ""
	if len(app) > 0 {
		help = "Registered apps, by app. "
	}
	labels := func(more ...string) []string { return append(slices.Clone(app), more...) }
	buckets := make([]float64, len(latencyBoundsMs))
	for i, ms := range latencyBoundsMs {
		buckets[i] = ms / 1000
	}
	return &families{
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: prefix + "requests_total",
			Help: help + "Proxied requests by backend, class (read or write) and status code.",
		}, labels("backend", "class", "code")),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    prefix + "request_duration_seconds",
			Help:    help + "Time from dispatch to the backend until the response finished. Excludes time spent held.",
			Buckets: buckets,
		}, labels("backend", "class")),
		endToEnd:   prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: prefix + "end_to_end_duration_seconds", Help: help + "Request arrival through final response, including body reads, hold and dispatch queue; one observation per client request.", Buckets: buckets}, labels("class", "code")),
		holdWait:   prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: prefix + "hold_wait_duration_seconds", Help: help + "Time from arrival to leaving a write hold, including initial body read.", Buckets: buckets}, labels("outcome")),
		writeQueue: prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: prefix + "write_queue_duration_seconds", Help: help + "Wait for a write dispatch slot; excludes backend execution.", Buckets: buckets}, labels("outcome")),
		writePause: prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: prefix + "write_pause_duration_seconds", Help: help + "Entire write hold from admission through release, including draining; recorded when the hold ends.", Buckets: buckets}, labels("outcome")),
		holdExpired: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: prefix + "hold_expired_total",
			Help: help + "Held writes answered 503 hold_timeout, never forwarded, because they waited max-hold.",
		}, labels()),
		heldDropped: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: prefix + "held_dropped_total",
			Help: help + "Held writes dropped because the client went away before release.",
		}, labels()),
		readRetries: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: prefix + "read_retries_total",
			Help: help + "Reads that failed on a backend (5xx or no answer) and were sent once more to another backend, by the backend that failed.",
		}, labels("backend")),
		gauges: gaugeDescs{
			inflight:     prometheus.NewDesc(prefix+"inflight", help+"Requests being proxied right now.", labels("class"), nil),
			heldWrites:   prometheus.NewDesc(prefix+"held_writes", help+"Write requests waiting for release.", labels(), nil),
			writesHeld:   prometheus.NewDesc(prefix+"writes_held", help+"1 while new writes are held.", labels(), nil),
			readWeight:   prometheus.NewDesc(prefix+"read_weight", help+"Share of reads (0-100) per backend.", labels("backend"), nil),
			writeBackend: prometheus.NewDesc(prefix+"write_backend", help+"1 for the backend that receives writes.", labels("backend"), nil),
			healthy:      prometheus.NewDesc(prefix+"backend_healthy", help+"1 when the last health check passed.", labels("backend"), nil),
		},
	}
}

// view is one app's metrics: l is empty for the default app and
// {app: name} for a registered one.
func (f *families) view(l prometheus.Labels) *routerMetrics {
	return &routerMetrics{
		requests:    f.requests.MustCurryWith(l),
		duration:    f.duration.MustCurryWith(l),
		endToEnd:    f.endToEnd.MustCurryWith(l),
		holdWait:    f.holdWait.MustCurryWith(l),
		writeQueue:  f.writeQueue.MustCurryWith(l),
		writePause:  f.writePause.MustCurryWith(l),
		holdExpired: f.holdExpired.With(l),
		heldDropped: f.heldDropped.With(l),
		readRetries: f.readRetries.MustCurryWith(l),
	}
}

// forget drops a deleted app's series.
func (f *families) forget(l prometheus.Labels) {
	f.requests.DeletePartialMatch(l)
	f.duration.DeletePartialMatch(l)
	f.endToEnd.DeletePartialMatch(l)
	f.holdWait.DeletePartialMatch(l)
	f.writeQueue.DeletePartialMatch(l)
	f.writePause.DeletePartialMatch(l)
	f.holdExpired.DeletePartialMatch(l)
	f.heldDropped.DeletePartialMatch(l)
	f.readRetries.DeletePartialMatch(l)
}

type gaugeDescs struct {
	inflight, heldWrites, writesHeld, readWeight, writeBackend, healthy *prometheus.Desc
}

func (d gaugeDescs) all() []*prometheus.Desc {
	return []*prometheus.Desc{d.inflight, d.heldWrites, d.writesHeld, d.readWeight, d.writeBackend, d.healthy}
}

// stateCollector reads routing state at scrape time, so the gauges can never
// drift from what the gateway actually does.
type stateCollector struct {
	g *Gateway
	m *metrics
}

func (c stateCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range append(c.m.def.gauges.all(), c.m.apps.gauges.all()...) {
		ch <- d
	}
}

func (c stateCollector) Collect(ch chan<- prometheus.Metric) {
	g := c.g
	g.router.collect(ch, c.m.def.gauges)
	g.appsMu.RLock()
	apps := slices.Collect(maps.Values(g.apps))
	g.appsMu.RUnlock()
	for _, rt := range apps {
		rt.collect(ch, c.m.apps.gauges, rt.name)
	}
}

// collect sends one app's gauges; app is the app label value, if the descs
// have one.
func (rt *router) collect(ch chan<- prometheus.Metric, d gaugeDescs, app ...string) {
	rt.mu.Lock()
	writes, held, waiting := rt.inflightWrites, rt.held, rt.waitingLocked()
	weights := maps.Clone(rt.readWeights)
	writeBackend := rt.writeBackend
	backends := rt.backends
	rt.mu.Unlock()

	gauge := func(desc *prometheus.Desc, v float64, labels ...string) {
		ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, v, append(slices.Clone(app), labels...)...)
	}
	gauge(d.inflight, float64(rt.inflightReads.Load()), classRead)
	gauge(d.inflight, float64(writes), classWrite)
	gauge(d.heldWrites, float64(waiting))
	gauge(d.writesHeld, b2f(held))
	for _, b := range backends {
		gauge(d.readWeight, float64(weights[b.name]), b.name)
		gauge(d.writeBackend, b2f(b.name == writeBackend), b.name)
		gauge(d.healthy, b2f(b.healthy.Load()), b.name)
	}
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
