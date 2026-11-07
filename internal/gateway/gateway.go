// Package gateway is the HopCharlie traffic gateway. It sits in front of every
// environment, behind one public proxy (the shared ALB of the management
// cluster; earlier Cloudflare Tunnel), and proxies user traffic to one backend
// environment. The migration engine drives it during cutover: hold writes,
// switch backend, release writes. Switching never changes public DNS, so
// client DNS caches and TTLs play no part in a cutover.
//
// Reads (GET, HEAD, OPTIONS) are spread over backends by weight; every other
// request is a write and goes to the single write backend. Admin API:
//
//	GET  /healthz                     200 once the config is loaded (no auth)
//	GET  /metrics                     Prometheus (no auth)
//	GET  /v1/state[?window=20s]       TrafficState
//	POST /v1/writes/hold              hold new writes; 200 once in-flight writes drained,
//	                                  504 (hold reverted) when they do not drain in time,
//	                                  409 when released meanwhile
//	POST /v1/writes/release           dispatch held writes to the current write backend
//	POST /v1/route {"backend":"eks"}  writes and 100% of reads to that backend
//	POST /v1/weights {"readWeights":{"gke":50,"eks":50}}
//
// Each app has its own backends, routing, hold and stats. The backends in
// Config are the default app, which the /v1 API above acts on. Other apps are
// registered at run time, and with Config.AppDomain set, Host <app>.<domain>
// reaches them:
//
//	GET    /apps                      AppList
//	PUT    /apps/{app}                AppSpec; create (201) or replace (200)
//	DELETE /apps/{app}                204
//	*      /apps/{app}/healthz, /apps/{app}/v1/...
//	                                  the API above for that app, so
//	                                  <admin>/apps/{app} works as a gateway URL
package gateway

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

const (
	classRead  = "read"
	classWrite = "write"

	headerBackend = "X-Charlie-Backend"
	// statusClientClosed records requests whose client left before the
	// backend answered (nginx's 499). They are not backend errors.
	statusClientClosed = 499
	// maxBufferedBody bounds how much of a held write's body is read into
	// memory while it waits (see bufferBody).
	maxBufferedBody = 64 << 10
	healthTimeout   = time.Second
)

type backend struct {
	name  string
	url   *url.URL
	proxy *httputil.ReverseProxy
	stats backendStats

	healthy   atomic.Bool
	hmu       sync.Mutex
	lastCheck time.Time
	lastErr   string
}

// holdGen is one hold period. Writes that arrive during it wait on release;
// release decides which backend they go to. A hold only ends by release (or
// by a hold call reverting it); nothing ends it on a timer.
type holdGen struct {
	id         uint64        // from Gateway.holdSeq; tells holds apart across release and re-hold
	release    chan struct{} // closed on release
	dispatched chan struct{} // closed once every released write left the queue
	waiting    int           // writes waiting for release, body already read
	pending    int           // released writes not yet dispatched
	released   bool
	backend    *backend // write backend at release time
}

// router is one app: its backends, routing, hold and stats. Apps share
// nothing else, so holding one app's writes never delays another's.
type router struct {
	g       *Gateway
	name    string
	log     *slog.Logger
	metrics *routerMetrics

	inflightReads atomic.Int64

	mu sync.Mutex
	// backends and byName are replaced, never changed in place, when a PUT
	// replaces a registered app. Read them under mu.
	backends       []*backend
	byName         map[string]*backend
	writeBackend   string
	readWeights    map[string]int // every configured backend, summing to 100
	held           bool
	heldSince      time.Time
	gen            *holdGen // non-nil while held
	holdSeq        uint64   // id of the last hold started
	buffering      int      // held writes still reading their body; count towards MaxHeld
	inflightWrites int      // includes released writes not yet dispatched
	writesIdle     chan struct{}
	writeSlots     chan struct{}
	healthPath     string
}

// Gateway is the proxy plus its admin API.
type Gateway struct {
	// The embedded router is the default app: the backends in Config, the /v1
	// admin API, and every request whose Host names no registered app.
	*router

	cfg      Config
	log      *slog.Logger
	prom     *metrics
	admin    http.Handler
	shutdown chan struct{} // closed by BeginShutdown
	stopOnce sync.Once

	// Lock order: optional controlMu, saveMu, appsMu, router mu. Never save
	// while holding appsMu/router mu, and never hold controlMu while draining.
	appsMu sync.RWMutex
	apps   map[string]*router // registered through the admin API

	saveMu sync.Mutex
	// Serializes administrative state changes and their persistence. Hold
	// releases this lock before draining so an operator can still cancel it.
	controlMu         sync.Mutex
	persistenceFailed atomic.Bool
	bufferedBytes     atomic.Int64
}

// New builds a gateway and restores routing from the state file, if any. A
// hold recorded there continues until release.
func New(cfg Config) (*Gateway, error) {
	cfg.setDefaults()
	if len(cfg.Backends) == 0 {
		return nil, errors.New("no backends configured")
	}
	if err := cfg.checkApps(); err != nil {
		return nil, err
	}
	g := &Gateway{cfg: cfg, log: cfg.Log, shutdown: make(chan struct{}), apps: map[string]*router{}}
	g.prom = newMetrics(g)
	def, err := g.newRouter(cfg.DefaultApp, cfg.Backends, g.log, g.prom.def.view(prometheus.Labels{}))
	if err != nil {
		return nil, err
	}
	g.router = def
	g.admin = g.adminMux()
	if err := g.restore(); err != nil {
		return nil, err
	}
	return g, nil
}

func (g *Gateway) newRouter(name string, bcs []BackendConfig, log *slog.Logger, m *routerMetrics) (*router, error) {
	rt := &router{g: g, name: name, log: log, metrics: m, writesIdle: make(chan struct{}), writeSlots: make(chan struct{}, g.cfg.MaxConcurrentWrites), healthPath: g.cfg.HealthPath}
	var err error
	rt.backends, rt.byName, err = rt.newBackends(bcs, nil)
	if err != nil {
		return nil, err
	}
	return rt, nil
}

// newBackends builds the backends for bcs. Those of old whose name and URL
// did not change are kept, with their stats and health.
func (rt *router) newBackends(bcs []BackendConfig, old map[string]*backend) ([]*backend, map[string]*backend, error) {
	list := make([]*backend, 0, len(bcs))
	byName := make(map[string]*backend, len(bcs))
	for _, bc := range bcs {
		if _, dup := byName[bc.Name]; dup {
			return nil, nil, fmt.Errorf("backend %q listed twice", bc.Name)
		}
		b := old[bc.Name]
		if b == nil || b.url.String() != bc.URL.String() {
			b = &backend{name: bc.Name, url: bc.URL}
			// Unknown until the first check; assume healthy so start-up does
			// not push every read onto the write backend.
			b.healthy.Store(true)
			b.proxy = rt.newProxy(b)
		}
		list = append(list, b)
		byName[b.name] = b
	}
	return list, byName, nil
}

// restore restores routing from the state file: the default app from its
// top-level fields, as older gateways wrote it, and each registered app from
// "apps". The default app starts on the initial backend without a file.
func (g *Gateway) restore() error {
	def := g.router
	initial := g.cfg.InitialBackend
	if initial == "" {
		initial = def.backends[0].name
	}
	if _, ok := def.byName[initial]; !ok {
		return fmt.Errorf("initial backend %q is not configured", initial)
	}
	var st *persistedState
	if g.cfg.StateFile != "" {
		var err error
		st, err = loadState(g.cfg.StateFile)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}

	if st == nil {
		def.mu.Lock()
		def.writeBackend = initial
		def.readWeights = def.weightsAllToLocked(initial)
		def.mu.Unlock()
	} else {
		if err := def.restore(st); err != nil {
			return fmt.Errorf("state file %s: %w", g.cfg.StateFile, err)
		}
		for _, name := range slices.Sorted(maps.Keys(st.Apps)) {
			if err := g.restoreApp(name, st.Apps[name]); err != nil {
				return fmt.Errorf("state file %s: app %s: %w", g.cfg.StateFile, name, err)
			}
		}
	}
	// Fail at start, not mid-cutover, when the state file cannot be written.
	if err := g.trySave(); err != nil {
		return fmt.Errorf("write state file: %w", err)
	}
	return nil
}

func (g *Gateway) restoreApp(name string, pa persistedApp) error {
	if !appName.MatchString(name) {
		return fmt.Errorf("name must match %s", appName)
	}
	if name == g.router.name {
		return fmt.Errorf("the default app (-default-app) has the same name; rename one of them")
	}
	bcs, err := parseBackendMap(pa.Backends)
	if err != nil {
		return err
	}
	rt, err := g.newRouter(name, bcs, g.log.With("app", name), g.prom.apps.view(prometheus.Labels{"app": name}))
	if err != nil {
		return err
	}
	if err := rt.restore(&pa.persistedState); err != nil {
		return err
	}
	if pa.HealthPath != "" {
		if !validHealthPath(pa.HealthPath) {
			return errors.New("invalid persisted app health path")
		}
		rt.healthPath = pa.HealthPath
	}
	g.apps[name] = rt
	return nil
}

// restore applies one app's recorded routing. A hold recorded there
// continues until release.
func (rt *router) restore(st *persistedState) error {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	weights, err := rt.restoredWeightsLocked(st)
	if err != nil {
		return err
	}
	rt.writeBackend = st.WriteBackend
	rt.readWeights = weights
	if st.WritesHeld {
		since := time.Now()
		if st.HeldSince != nil {
			since = *st.HeldSince
		}
		rt.startHoldLocked(since)
		rt.log.Warn("resuming write hold from state file; it lasts until release", "heldSince", since.UTC())
	}
	return nil
}

// restoredWeightsLocked checks the state file against the configured
// backends. A backend removed from the config is dropped if it got no reads;
// anything else is refused rather than guessed, since routing differently
// from what was recorded could split writes mid-migration.
func (rt *router) restoredWeightsLocked(st *persistedState) (map[string]int, error) {
	configured := strings.Join(rt.namesLocked(), ", ")
	if _, ok := rt.byName[st.WriteBackend]; !ok {
		return nil, fmt.Errorf("write backend %q is not configured (configured: %s); add it back to the backends or fix the state file",
			st.WriteBackend, configured)
	}
	weights := make(map[string]int, len(st.ReadWeights))
	for _, name := range slices.Sorted(maps.Keys(st.ReadWeights)) {
		w := st.ReadWeights[name]
		if _, ok := rt.byName[name]; !ok {
			if w == 0 {
				rt.log.Warn("dropping unconfigured backend with read weight 0 from state file", "backend", name)
				continue
			}
			return nil, fmt.Errorf("backend %q has read weight %d but is not configured (configured: %s); add it back to the backends or fix the state file",
				name, w, configured)
		}
		weights[name] = w
	}
	if err := validateWeights(weights, rt.namesLocked()); err != nil {
		return nil, err
	}
	return rt.fullWeightsLocked(weights), nil
}

// Proxy returns the public handler for user traffic.
func (g *Gateway) Proxy() http.Handler { return http.HandlerFunc(g.serveProxy) }

// Admin returns the admin API handler.
func (g *Gateway) Admin() http.Handler { return g.admin }

// BeginShutdown answers every write waiting on a hold with 503 shutting_down,
// and every write that arrives while held from now on, without forwarding
// them. The hold and routing are unchanged, so a restart keeps holding. Call
// it before shutting the proxy server down: held writes would otherwise keep
// it waiting.
func (g *Gateway) BeginShutdown() {
	g.stopOnce.Do(func() { close(g.shutdown) })
}

func (g *Gateway) shuttingDown() bool {
	select {
	case <-g.shutdown:
		return true
	default:
		return false
	}
}

// Close persists the final state. Call it after the servers have stopped; a
// hold in progress stays recorded as held.
func (g *Gateway) Close() {
	g.save()
}

// serveProxy sends a request to the app its Host names (see routerFor).
func (g *Gateway) serveProxy(w http.ResponseWriter, r *http.Request) {
	rt, name := g.routerFor(r.Host)
	if rt == nil {
		writeError(w, http.StatusNotFound, "unknown_app",
			fmt.Sprintf("no app named %q is registered at this gateway (host %s)", name, r.Host))
		return
	}
	rt.serve(w, r)
}

func (rt *router) serve(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	class := classWrite
	if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
		class = classRead
	}
	sw := &statusWriter{ResponseWriter: w}
	defer func() {
		code := sw.status()
		if sw.code == 0 && r.Context().Err() != nil {
			code = statusClientClosed
		}
		rt.metrics.endToEnd.WithLabelValues(class, strconv.Itoa(code)).Observe(time.Since(start).Seconds())
	}()
	w = sw
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		rt.serveRead(w, r)
	default:
		rt.serveWrite(w, r)
	}
}

func (rt *router) serveRead(w http.ResponseWriter, r *http.Request) {
	rt.mu.Lock()
	b := rt.pickReadLocked()
	var alt *backend
	if retryableRead(r) {
		alt = rt.retryTargetLocked(b)
	}
	rt.mu.Unlock()
	rt.inflightReads.Add(1)
	defer rt.inflightReads.Add(-1)
	if alt == nil {
		rt.forward(w, r, b, classRead, nil)
		return
	}
	// A backend answers 5xx or not at all for a few seconds while its
	// database is demoted or restarts during a cutover. The read is sent once
	// more to alt, and the client only sees the second answer.
	rw := &retryWriter{w: w, header: http.Header{}}
	rt.forward(rw, r, b, classRead, rw)
	if !rw.failed || r.Context().Err() != nil {
		return
	}
	rt.metrics.readRetries.WithLabelValues(b.name).Inc()
	rt.forward(w, r, alt, classRead, nil)
}

// retryableRead: a read the gateway may send twice. Only GET and HEAD
// without a body, and never a protocol upgrade, whose connection is handed
// over to the backend.
func retryableRead(r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	if r.Header.Get("Upgrade") != "" {
		return false
	}
	return r.Body == nil || r.Body == http.NoBody || r.ContentLength == 0
}

// retryTargetLocked picks where a read that fails on `failed` goes once more,
// or nil when there is nowhere to send it (then the first answer reaches the
// client unchanged). First another backend that takes reads now, then the
// write backend. While writes are held for a cutover, also any other healthy
// backend: both environments hold the same data then, and one of them is
// being demoted or restarted. At other times a backend with no read weight
// may not have the data yet, so reads never fall back to it.
func (rt *router) retryTargetLocked(failed *backend) *backend {
	var best *backend
	for _, b := range rt.backends {
		if b == failed || !b.healthy.Load() {
			continue
		}
		if w := rt.readWeights[b.name]; w > 0 && (best == nil || w > rt.readWeights[best.name]) {
			best = b
		}
	}
	if best != nil {
		return best
	}
	if wb := rt.byName[rt.writeBackend]; wb != failed && wb.healthy.Load() {
		return wb
	}
	if rt.held {
		for _, b := range rt.backends {
			if b != failed && b.healthy.Load() {
				return b
			}
		}
	}
	return nil
}

// retryableStatus are the answers a read is retried after.
func retryableStatus(code int) bool {
	switch code {
	case http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

// retryWriter holds back a read's first answer until its status is known.
// A retryable failure is discarded (headers and body) so the read can be sent
// again; any other answer passes through as it comes, streaming included.
type retryWriter struct {
	w      http.ResponseWriter
	header http.Header // headers set before the status is known
	failed bool        // the answer was a retryable failure and was discarded
	passed bool        // the answer is going to the client
}

func (rw *retryWriter) Header() http.Header {
	if rw.passed {
		return rw.w.Header()
	}
	return rw.header
}

func (rw *retryWriter) WriteHeader(code int) {
	switch {
	case rw.failed:
		return
	case rw.passed:
		rw.w.WriteHeader(code)
		return
	case code < 200: // informational; the final status follows
		copyHeader(rw.w.Header(), rw.header)
		rw.w.WriteHeader(code)
		return
	case retryableStatus(code):
		rw.failed = true
		return
	}
	copyHeader(rw.w.Header(), rw.header)
	rw.passed = true
	rw.w.WriteHeader(code)
}

func (rw *retryWriter) Write(p []byte) (int, error) {
	if rw.failed {
		return len(p), nil
	}
	if !rw.passed {
		rw.WriteHeader(http.StatusOK)
	}
	return rw.w.Write(p)
}

// FlushError flushes only an answer that is going to the client; flushing a
// discarded one would commit an empty 200.
func (rw *retryWriter) FlushError() error {
	if !rw.passed {
		return nil
	}
	return http.NewResponseController(rw.w).Flush()
}

func (rw *retryWriter) Unwrap() http.ResponseWriter { return rw.w }

func copyHeader(dst, src http.Header) {
	for k, v := range src {
		dst[k] = v
	}
}

// pickReadLocked picks a read backend by weight among healthy backends and
// falls back to the write backend when none is left.
func (rt *router) pickReadLocked() *backend {
	total := 0
	for _, b := range rt.backends {
		if w := rt.readWeights[b.name]; w > 0 && b.healthy.Load() {
			total += w
		}
	}
	if total > 0 {
		n := rt.g.cfg.Rand(total)
		for _, b := range rt.backends {
			w := rt.readWeights[b.name]
			if w <= 0 || !b.healthy.Load() {
				continue
			}
			if n < w {
				return b
			}
			n -= w
		}
	}
	return rt.byName[rt.writeBackend]
}

// serveWrite sends a write to the write backend, or parks it while writes are
// held. Writes never fail over on a bad health check: moving them is a
// migration decision, not the gateway's.
func (rt *router) serveWrite(w http.ResponseWriter, r *http.Request) {
	arrived := time.Now()
	buffered := false
	rt.mu.Lock()
	for rt.held {
		if rt.g.shuttingDown() {
			rt.mu.Unlock()
			retryLater(w, "shutting_down", "the gateway is restarting; retry shortly")
			return
		}
		if !buffered {
			if rt.waitingLocked() >= rt.g.cfg.MaxHeld {
				rt.mu.Unlock()
				rt.metrics.holdWait.WithLabelValues("rejected").Observe(time.Since(arrived).Seconds())
				retryLater(w, "too_many_held_writes", "writes are paused for a migration; retry shortly")
				return
			}
			// Take the slot before reading the body, so MaxHeld bounds the
			// memory held writes use.
			rt.buffering++
			rt.mu.Unlock()
			if r.Body != nil && r.Body != http.NoBody {
				if !rt.g.reserveHeldBuffer() {
					rt.mu.Lock()
					rt.buffering--
					rt.mu.Unlock()
					rt.metrics.holdWait.WithLabelValues("rejected").Observe(time.Since(arrived).Seconds())
					retryLater(w, "buffer_budget_exhausted", "held request body memory budget is full; not forwarded")
					return
				}
				defer rt.g.bufferedBytes.Add(-heldBufferReservation)
			}
			bodyDeadline := arrived.Add(rt.g.cfg.MaxHold)
			if limit := time.Now().Add(rt.g.cfg.BodyReadTimeout); limit.Before(bodyDeadline) {
				bodyDeadline = limit
			}
			err := bufferBodyUntil(w, r, bodyDeadline)
			rt.mu.Lock()
			rt.buffering--
			if err != nil {
				rt.mu.Unlock()
				rt.metrics.holdWait.WithLabelValues("body_error").Observe(time.Since(arrived).Seconds())
				if errors.Is(err, errBodyTimeout) {
					if time.Since(arrived) >= rt.g.cfg.MaxHold {
						rt.metrics.holdExpired.Inc()
						retryLater(w, "hold_timeout", "write expired while receiving its body; not forwarded")
					} else {
						writeError(w, http.StatusRequestTimeout, "body_timeout", "request body did not arrive in time; not forwarded")
					}
					return
				}
				rt.metrics.heldDropped.Inc()
				writeError(w, http.StatusBadRequest, "bad_request", "could not read the request body: "+err.Error())
				return
			}
			buffered = true
			continue // the hold may have ended while the body was read
		}
		gen := rt.gen
		gen.waiting++
		rt.mu.Unlock()
		b := rt.awaitRelease(w, r, gen, arrived)
		if b == nil {
			return
		}
		defer rt.writeDone()
		rt.dispatchWrite(w, r, b, arrived)
		return
	}
	b := rt.byName[rt.writeBackend]
	rt.inflightWrites++
	rt.mu.Unlock()
	defer rt.writeDone()
	rt.dispatchWrite(w, r, b, arrived)
}

// Account conservatively for ReadAll growth and the retained streaming prefix.
const heldBufferReservation int64 = 2 * (maxBufferedBody + 1)

func (g *Gateway) reserveHeldBuffer() bool {
	for {
		used := g.bufferedBytes.Load()
		if used > g.cfg.MaxBufferedBytes-heldBufferReservation {
			return false
		}
		if g.bufferedBytes.CompareAndSwap(used, used+heldBufferReservation) {
			return true
		}
	}
}

// Queued writes remain in inflightWrites: a subsequent hold must drain them
// too. New requests share the limiter so they cannot bypass a release burst.
func (rt *router) dispatchWrite(w http.ResponseWriter, r *http.Request, b *backend, arrived time.Time) {
	queuedAt := time.Now()
	observe := func(outcome string) {
		rt.metrics.writeQueue.WithLabelValues(outcome).Observe(time.Since(queuedAt).Seconds())
	}
	ctx, cancel := context.WithDeadline(r.Context(), arrived.Add(rt.g.cfg.MaxHold))
	defer cancel()
	select {
	case rt.writeSlots <- struct{}{}:
		defer func() { <-rt.writeSlots }()
	case <-ctx.Done():
		observe("not_forwarded")
		if r.Context().Err() == nil {
			retryLater(w, "write_queue_timeout", "write expired in dispatch queue; not forwarded")
		}
		return
	}
	if ctx.Err() != nil {
		observe("not_forwarded")
		if r.Context().Err() == nil {
			retryLater(w, "write_queue_timeout", "write expired in dispatch queue; not forwarded")
		}
		return
	}
	observe("dispatched")
	rt.forward(w, r, b, classWrite, nil)
}

// bufferBody reads a held write's body into memory. Go notices a client
// disconnect (and cancels r.Context()) only after the request body has been
// read, so without this a held POST whose client gave up would still be
// forwarded on release. Bodies above maxBufferedBody keep streaming.
func bufferBody(r *http.Request) error {
	if r.Body == nil || r.Body == http.NoBody {
		return nil
	}
	buf, err := io.ReadAll(io.LimitReader(r.Body, maxBufferedBody+1))
	if err != nil {
		return err
	}
	if len(buf) <= maxBufferedBody {
		r.Body = io.NopCloser(bytes.NewReader(buf))
		return nil
	}
	r.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(buf), r.Body), r.Body}
	return nil
}

var errBodyTimeout = errors.New("held request body timed out")

func bufferBodyUntil(w http.ResponseWriter, r *http.Request, deadline time.Time) error {
	if r.Body == nil || r.Body == http.NoBody {
		return nil
	}
	controller := http.NewResponseController(w)
	// A network read deadline interrupts net/http's body read and Close.
	// Closing also supports non-network readers such as io.Pipe in tests.
	_ = controller.SetReadDeadline(deadline)
	body := r.Body
	done := make(chan struct{})
	timer := time.AfterFunc(time.Until(deadline), func() { _ = body.Close(); close(done) })
	err := bufferBody(r)
	if !timer.Stop() {
		<-done
	}
	_ = controller.SetReadDeadline(time.Time{})
	if !time.Now().Before(deadline) {
		return errBodyTimeout
	}
	return err
}

func retryLater(w http.ResponseWriter, code, msg string) {
	w.Header().Set("Retry-After", "1")
	writeError(w, http.StatusServiceUnavailable, code, msg)
}

// awaitRelease blocks until gen is released and returns the backend to send
// the write to; a held write is never forwarded before that. A write that
// cannot wait any longer (MaxHold since it arrived, or the gateway is shutting
// down) is answered with 503, and one whose client left is dropped. Then
// awaitRelease returns nil.
func (rt *router) awaitRelease(w http.ResponseWriter, r *http.Request, gen *holdGen, arrived time.Time) (selected *backend) {
	defer func() {
		outcome := "not_forwarded"
		if selected != nil {
			outcome = "released"
		}
		rt.metrics.holdWait.WithLabelValues(outcome).Observe(time.Since(arrived).Seconds())
	}()
	timeout := time.NewTimer(max(rt.g.cfg.MaxHold-time.Since(arrived), 0))
	defer timeout.Stop()
	var gone, expired bool
	select {
	case <-gen.release:
	case <-r.Context().Done():
		gone = true
	case <-timeout.C:
		expired = true
	case <-rt.g.shutdown:
	}
	rt.mu.Lock()
	if gen.released && !gone {
		// Released, perhaps at the same moment as a timeout or shutdown:
		// forwarding is safe now, so nobody gets an error.
		gen.dispatchedOneLocked()
		rt.mu.Unlock()
		return gen.backend
	}
	if gen.released {
		// Release won the race and already counted this write as in flight.
		gen.dispatchedOneLocked()
		rt.writeDoneLocked()
	} else {
		gen.waiting--
	}
	rt.mu.Unlock()
	switch {
	case gone:
		rt.metrics.heldDropped.Inc()
	case expired:
		rt.metrics.holdExpired.Inc()
		retryLater(w, "hold_timeout", fmt.Sprintf("writes are paused for a migration and this one waited %s; retry shortly", rt.g.cfg.MaxHold))
	default:
		retryLater(w, "shutting_down", "the gateway is restarting; retry shortly")
	}
	return nil
}

func (gen *holdGen) dispatchedOneLocked() {
	gen.pending--
	if gen.pending == 0 {
		close(gen.dispatched)
	}
}

func (rt *router) writeDone() {
	rt.mu.Lock()
	rt.writeDoneLocked()
	rt.mu.Unlock()
}

func (rt *router) writeDoneLocked() {
	rt.inflightWrites--
	if rt.inflightWrites == 0 {
		close(rt.writesIdle)
		rt.writesIdle = make(chan struct{})
	}
}

// waitingLocked counts writes waiting for release, including those still
// reading their body.
func (rt *router) waitingLocked() int {
	if rt.gen == nil {
		return 0
	}
	return rt.gen.waiting + rt.buffering
}

// startHoldLocked starts a hold period. It lasts until releaseLocked.
func (rt *router) startHoldLocked(since time.Time) {
	rt.holdSeq++
	gen := &holdGen{id: rt.holdSeq, release: make(chan struct{}), dispatched: make(chan struct{})}
	rt.held, rt.heldSince, rt.gen = true, since, gen
}

// releaseLocked ends the hold and hands every waiting write to the current
// write backend. They count as in flight from here, so a hold that follows
// right away still waits for them.
func (rt *router) releaseLocked() *holdGen {
	gen := rt.gen
	gen.released = true
	gen.backend = rt.byName[rt.writeBackend]
	gen.pending = gen.waiting
	rt.inflightWrites += gen.waiting
	gen.waiting = 0
	if gen.pending == 0 {
		close(gen.dispatched)
	}
	close(gen.release)
	rt.held, rt.gen = false, nil
	return gen
}

func (rt *router) waitWritesDrained(ctx context.Context) error {
	for {
		rt.mu.Lock()
		n, idle := rt.inflightWrites, rt.writesIdle
		rt.mu.Unlock()
		if n == 0 {
			return nil
		}
		select {
		case <-idle:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (rt *router) newProxy(b *backend) *httputil.ReverseProxy {
	g := rt.g
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			// The public proxy in front (ALB, or cloudflared) is the only
			// client, so keep its X-Forwarded-For chain, whose last entry is
			// the client address it saw, and append its own address after it
			// (SetXForwarded) instead of replacing the chain. Keep its
			// X-Forwarded-Proto (https at the edge) too, instead of the plain
			// http of the hop to us.
			if xff := pr.In.Header["X-Forwarded-For"]; len(xff) > 0 {
				pr.Out.Header["X-Forwarded-For"] = xff
			}
			pr.SetURL(b.url)
			pr.SetXForwarded()
			if proto := pr.In.Header.Get("X-Forwarded-Proto"); proto != "" {
				pr.Out.Header.Set("X-Forwarded-Proto", proto)
			}
		},
		Transport:     g.cfg.Transport,
		FlushInterval: -1, // stream SSE and chunked responses as they come
		ModifyResponse: func(resp *http.Response) error {
			resp.Header.Set(headerBackend, b.name)
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			client := r.Context()
			if c, ok := client.Value(clientCtxKey{}).(context.Context); ok {
				client = c // a write, whose r.Context() outlives the client
			}
			switch {
			case errors.Is(context.Cause(r.Context()), errWriteTimeout):
				rt.log.Warn("write timed out", "backend", b.name, "method", r.Method, "path", r.URL.Path,
					"writeTimeout", g.cfg.WriteTimeout.String())
				w.Header().Set(headerBackend, b.name)
				writeError(w, http.StatusGatewayTimeout, "backend_timeout",
					fmt.Sprintf("backend %s did not answer within %s", b.name, g.cfg.WriteTimeout))
			case client.Err() != nil:
				// The client left; nobody to answer and not the backend's fault.
				if sw, ok := w.(*statusWriter); ok {
					sw.code = statusClientClosed
				}
			default:
				rt.log.Warn("proxy error", "backend", b.name, "method", r.Method, "path", r.URL.Path, "err", err)
				w.Header().Set(headerBackend, b.name)
				writeError(w, http.StatusBadGateway, "bad_gateway", "backend "+b.name+" did not answer")
			}
		},
	}
}

// clientCtxKey holds a forwarded write's client context, so the error handler
// can still tell that the client left.
type clientCtxKey struct{}

var errWriteTimeout = errors.New("write timeout")

// forward proxies r to b. retry is the retryWriter of a first read attempt
// (nil otherwise): when it discarded the answer, the attempt is counted as
// retried, not as a failure the client saw.
func (rt *router) forward(w http.ResponseWriter, r *http.Request, b *backend, class string, retry *retryWriter) {
	if class == classWrite {
		// A write stays in flight until the backend answers, even when its
		// client leaves: the backend may still commit it, and hold must wait
		// for that. The timeout also gives ReverseProxy a context it can end.
		ctx, cancel := context.WithTimeoutCause(context.WithoutCancel(r.Context()), rt.g.cfg.WriteTimeout, errWriteTimeout)
		defer cancel()
		r = r.WithContext(context.WithValue(ctx, clientCtxKey{}, r.Context()))
	}
	sw := &statusWriter{ResponseWriter: w}
	start := time.Now()
	// Deferred because ReverseProxy panics with http.ErrAbortHandler when a
	// streamed body breaks, and that request must still be counted.
	defer func() {
		d := time.Since(start)
		code := sw.status()
		b.stats.record(time.Now(), d, code >= 500, retry != nil && retry.failed)
		rt.metrics.requests.WithLabelValues(b.name, class, strconv.Itoa(code)).Inc()
		rt.metrics.duration.WithLabelValues(b.name, class).Observe(d.Seconds())
	}()
	b.proxy.ServeHTTP(sw, r)
}

// statusWriter records the response status. Unwrap lets ReverseProxy reach
// the real writer for flushing and protocol upgrades.
type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.code == 0 && code >= 200 { // 1xx are informational
		w.code = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(p []byte) (int, error) {
	if w.code == 0 {
		w.code = http.StatusOK
	}
	return w.ResponseWriter.Write(p)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *statusWriter) status() int {
	if w.code == 0 {
		// Only a protocol upgrade writes no status through us: ReverseProxy
		// hijacks the connection and sends the 101 itself.
		return http.StatusSwitchingProtocols
	}
	return w.code
}

// Run checks backend health every HealthInterval, for every app registered at
// the time of each round, and closes idle proxy connections every
// ConnRecycle, until ctx ends.
func (g *Gateway) Run(ctx context.Context) {
	go func() {
		t := time.NewTicker(g.cfg.ConnRecycle)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				g.recycleConns()
			}
		}
	}()
	client := &http.Client{Transport: g.cfg.HealthTransport, Timeout: healthTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	t := time.NewTicker(g.cfg.HealthInterval)
	defer t.Stop()
	for {
		var wg sync.WaitGroup
		for _, rt := range g.routers() {
			rt.mu.Lock()
			backends := rt.backends
			rt.mu.Unlock()
			for _, b := range backends {
				wg.Go(func() { rt.checkBackend(ctx, client, b) })
			}
		}
		wg.Wait()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (rt *router) checkBackend(ctx context.Context, client *http.Client, b *backend) {
	rt.mu.Lock()
	path := rt.healthPath
	rt.mu.Unlock()
	parsed, _ := url.ParseRequestURI(path)
	u := b.url.JoinPath(parsed.EscapedPath())
	u.RawQuery = parsed.RawQuery
	err := probe(ctx, client, u.String())
	if ctx.Err() != nil {
		return // shutting down; keep the last real result
	}
	now := time.Now()
	msg := probeError(err)
	rt.mu.Lock()
	if rt.healthPath != path || rt.byName[b.name] != b {
		rt.mu.Unlock()
		return
	}
	b.hmu.Lock()
	b.lastCheck, b.lastErr = now, msg
	b.hmu.Unlock()
	was := b.healthy.Swap(err == nil)
	rt.mu.Unlock()
	if was != (err == nil) {
		if err != nil {
			rt.log.Warn("backend unhealthy", "backend", b.name, "err", msg)
		} else {
			rt.log.Info("backend healthy", "backend", b.name)
		}
		// The backend may have moved (it failed) or come back at a new
		// address: drop pooled connections so requests dial again.
		rt.g.recycleConns()
	}
}

// recycleConns closes idle proxy connections; the next request to a backend
// dials and resolves its name again.
func (g *Gateway) recycleConns() {
	if t, ok := g.cfg.Transport.(interface{ CloseIdleConnections() }); ok {
		t.CloseIdleConnections()
	}
}

// probeError describes a failed health check. A name that does not resolve
// is called out, since the fix is in DNS, not in the backend.
func probeError(err error) string {
	if err == nil {
		return ""
	}
	var requestError *url.Error
	if errors.As(err, &requestError) {
		return probeError(requestError.Err)
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return "dns: " + dns.Error()
	}
	return err.Error()
}

func probe(ctx context.Context, client *http.Client, u string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10)) // lets the connection be reused
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("health check returned %s", resp.Status)
	}
	return nil
}
