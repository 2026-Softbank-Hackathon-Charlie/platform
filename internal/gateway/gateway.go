// Package gateway is the HopCharlie traffic gateway. It sits outside every
// cluster (behind Cloudflare Tunnel) and proxies user traffic to one backend
// environment, and the migration engine drives it during cutover: hold writes,
// switch backend, release writes.
//
// Reads (GET, HEAD, OPTIONS) are spread over backends by weight; every other
// request is a write and goes to the single write backend. Admin API:
//
//	GET  /healthz                     200 once the config is loaded (no auth)
//	GET  /metrics                     Prometheus (no auth)
//	GET  /v1/state[?window=20s]       TrafficState
//	POST /v1/writes/hold              hold new writes; 200 once in-flight writes drained
//	POST /v1/writes/release           dispatch held writes to the current write backend
//	POST /v1/route {"backend":"eks"}  writes and 100% of reads to that backend
//	POST /v1/weights {"readWeights":{"gke":50,"eks":50}}
package gateway

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
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
// release decides which backend they go to.
type holdGen struct {
	release    chan struct{} // closed on release
	dispatched chan struct{} // closed once every released write left the queue
	timer      *time.Timer   // max-hold auto release
	waiting    int           // writes waiting for release
	pending    int           // released writes not yet dispatched
	released   bool
	backend    *backend // write backend at release time
}

// Gateway is the proxy plus its admin API.
type Gateway struct {
	cfg      Config
	log      *slog.Logger
	backends []*backend
	byName   map[string]*backend
	metrics  *metrics
	admin    http.Handler

	inflightReads atomic.Int64

	mu             sync.Mutex
	writeBackend   string
	readWeights    map[string]int // every configured backend, summing to 100
	held           bool
	heldSince      time.Time
	gen            *holdGen // non-nil while held
	inflightWrites int      // includes released writes not yet dispatched
	writesIdle     chan struct{}
	closed         bool
	seq            uint64

	saveMu   sync.Mutex
	savedSeq uint64
}

// New builds a gateway and restores routing from the state file, if any. A
// hold recorded there continues with the max-hold left since heldSince.
func New(cfg Config) (*Gateway, error) {
	cfg.setDefaults()
	if len(cfg.Backends) == 0 {
		return nil, errors.New("no backends configured")
	}
	g := &Gateway{cfg: cfg, log: cfg.Log, byName: map[string]*backend{}, writesIdle: make(chan struct{})}
	for _, bc := range cfg.Backends {
		if _, dup := g.byName[bc.Name]; dup {
			return nil, fmt.Errorf("backend %q listed twice", bc.Name)
		}
		b := &backend{name: bc.Name, url: bc.URL}
		// Unknown until the first check; assume healthy so start-up does not
		// push every read onto the write backend.
		b.healthy.Store(true)
		b.proxy = g.newProxy(b)
		g.backends = append(g.backends, b)
		g.byName[b.name] = b
	}
	g.metrics = newMetrics(g)
	g.admin = g.adminMux()
	if err := g.restore(); err != nil {
		return nil, err
	}
	return g, nil
}

func (g *Gateway) restore() error {
	initial := g.cfg.InitialBackend
	if initial == "" {
		initial = g.backends[0].name
	}
	if _, ok := g.byName[initial]; !ok {
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

	g.mu.Lock()
	if st == nil {
		g.writeBackend = initial
		g.readWeights = g.weightsAllTo(initial)
	} else {
		// Refuse rather than guess: routing to a different backend than the
		// one recorded could split writes mid-migration.
		if _, ok := g.byName[st.WriteBackend]; !ok {
			g.mu.Unlock()
			return fmt.Errorf("state file %s: write backend %q is not configured", g.cfg.StateFile, st.WriteBackend)
		}
		if err := g.validateWeights(st.ReadWeights); err != nil {
			g.mu.Unlock()
			return fmt.Errorf("state file %s: %w", g.cfg.StateFile, err)
		}
		g.writeBackend = st.WriteBackend
		g.readWeights = g.fullWeights(st.ReadWeights)
		if st.WritesHeld {
			since := time.Now()
			if st.HeldSince != nil {
				since = *st.HeldSince
			}
			g.startHoldLocked(since)
			g.log.Warn("resuming write hold from state file", "heldSince", since.UTC(),
				"maxHoldLeft", max(g.cfg.MaxHold-time.Since(since), 0).Round(time.Millisecond).String())
		}
	}
	snap := g.snapshotLocked()
	g.mu.Unlock()
	// Fail at start, not mid-cutover, when the state file cannot be written.
	if err := g.trySave(snap); err != nil {
		g.mu.Lock()
		g.closed = true // a resumed hold's timer must not fire on a dead gateway
		if g.gen != nil {
			g.gen.timer.Stop()
		}
		g.mu.Unlock()
		return fmt.Errorf("write state file: %w", err)
	}
	return nil
}

// Proxy returns the public handler for user traffic.
func (g *Gateway) Proxy() http.Handler { return http.HandlerFunc(g.serveProxy) }

// Admin returns the admin API handler.
func (g *Gateway) Admin() http.Handler { return g.admin }

// Close stops the max-hold timer and persists the final state. Call it after
// the servers have stopped; a hold in progress stays recorded as held.
func (g *Gateway) Close() {
	g.mu.Lock()
	g.closed = true
	if g.gen != nil {
		g.gen.timer.Stop()
	}
	snap := g.snapshotLocked()
	g.mu.Unlock()
	g.save(snap)
}

func (g *Gateway) serveProxy(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		g.serveRead(w, r)
	default:
		g.serveWrite(w, r)
	}
}

func (g *Gateway) serveRead(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	b := g.pickReadLocked()
	g.mu.Unlock()
	g.inflightReads.Add(1)
	defer g.inflightReads.Add(-1)
	g.forward(w, r, b, classRead)
}

// pickReadLocked picks a read backend by weight among healthy backends and
// falls back to the write backend when none is left.
func (g *Gateway) pickReadLocked() *backend {
	total := 0
	for _, b := range g.backends {
		if w := g.readWeights[b.name]; w > 0 && b.healthy.Load() {
			total += w
		}
	}
	if total > 0 {
		n := g.cfg.Rand(total)
		for _, b := range g.backends {
			w := g.readWeights[b.name]
			if w <= 0 || !b.healthy.Load() {
				continue
			}
			if n < w {
				return b
			}
			n -= w
		}
	}
	return g.byName[g.writeBackend]
}

// serveWrite sends a write to the write backend, or parks it while writes are
// held. Writes never fail over on a bad health check: moving them is a
// migration decision, not the gateway's.
func (g *Gateway) serveWrite(w http.ResponseWriter, r *http.Request) {
	buffered := false
	g.mu.Lock()
	for g.held {
		gen := g.gen
		if gen.waiting >= g.cfg.MaxHeld {
			g.mu.Unlock()
			w.Header().Set("Retry-After", "1")
			writeError(w, http.StatusServiceUnavailable, "too_many_held_writes", "writes are paused for a migration; retry shortly")
			return
		}
		if !buffered {
			g.mu.Unlock()
			if err := bufferBody(r); err != nil {
				g.metrics.heldDropped.Inc()
				return
			}
			buffered = true
			g.mu.Lock()
			continue // the hold may have ended while the body was read
		}
		gen.waiting++
		g.mu.Unlock()
		b, ok := g.awaitRelease(r.Context(), gen)
		if !ok {
			return
		}
		defer g.writeDone()
		g.forward(w, r, b, classWrite)
		return
	}
	b := g.byName[g.writeBackend]
	g.inflightWrites++
	g.mu.Unlock()
	defer g.writeDone()
	g.forward(w, r, b, classWrite)
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

// awaitRelease blocks until the hold ends. A write whose client leaves first
// is dropped.
func (g *Gateway) awaitRelease(ctx context.Context, gen *holdGen) (*backend, bool) {
	select {
	case <-gen.release:
		g.mu.Lock()
		gen.dispatchedOneLocked()
		g.mu.Unlock()
		return gen.backend, true
	case <-ctx.Done():
		g.mu.Lock()
		if gen.released {
			// Release won the race and already counted this write as in flight.
			gen.dispatchedOneLocked()
			g.writeDoneLocked()
		} else {
			gen.waiting--
		}
		g.mu.Unlock()
		g.metrics.heldDropped.Inc()
		return nil, false
	}
}

func (gen *holdGen) dispatchedOneLocked() {
	gen.pending--
	if gen.pending == 0 {
		close(gen.dispatched)
	}
}

func (g *Gateway) writeDone() {
	g.mu.Lock()
	g.writeDoneLocked()
	g.mu.Unlock()
}

func (g *Gateway) writeDoneLocked() {
	g.inflightWrites--
	if g.inflightWrites == 0 {
		close(g.writesIdle)
		g.writesIdle = make(chan struct{})
	}
}

func (g *Gateway) waitingLocked() int {
	if g.gen == nil {
		return 0
	}
	return g.gen.waiting
}

// startHoldLocked starts a hold period that auto-releases MaxHold after since.
func (g *Gateway) startHoldLocked(since time.Time) {
	gen := &holdGen{release: make(chan struct{}), dispatched: make(chan struct{})}
	gen.timer = time.AfterFunc(max(g.cfg.MaxHold-time.Since(since), 0), func() { g.expireHold(gen) })
	g.held, g.heldSince, g.gen = true, since, gen
}

// releaseLocked ends the hold and hands every waiting write to the current
// write backend. They count as in flight from here, so a hold that follows
// right away still waits for them.
func (g *Gateway) releaseLocked() *holdGen {
	gen := g.gen
	gen.timer.Stop()
	gen.released = true
	gen.backend = g.byName[g.writeBackend]
	gen.pending = gen.waiting
	g.inflightWrites += gen.waiting
	gen.waiting = 0
	if gen.pending == 0 {
		close(gen.dispatched)
	}
	close(gen.release)
	g.held, g.gen = false, nil
	return gen
}

// expireHold releases a hold that outlived MaxHold, so a crashed engine
// cannot freeze writes forever.
func (g *Gateway) expireHold(gen *holdGen) {
	g.mu.Lock()
	if g.closed || g.gen != gen {
		g.mu.Unlock()
		return
	}
	heldSince := g.heldSince
	g.releaseLocked()
	g.metrics.holdExpired.Inc() // under g.mu: nobody sees the release without the count
	n := gen.pending
	snap := g.snapshotLocked()
	g.mu.Unlock()
	g.save(snap)
	g.log.Warn("max hold elapsed; released held writes", "action", "auto_release", "backend", gen.backend.name,
		"released", n, "heldSince", heldSince.UTC(), "maxHold", g.cfg.MaxHold.String())
}

func (g *Gateway) waitWritesDrained(ctx context.Context) error {
	for {
		g.mu.Lock()
		n, idle := g.inflightWrites, g.writesIdle
		g.mu.Unlock()
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

func (g *Gateway) newProxy(b *backend) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			// cloudflared is the only client, so keep its X-Forwarded-For
			// chain instead of replacing it with cloudflared's own address.
			if xff := pr.In.Header["X-Forwarded-For"]; len(xff) > 0 {
				pr.Out.Header["X-Forwarded-For"] = xff
			}
			pr.SetURL(b.url)
			pr.SetXForwarded()
		},
		Transport:     g.cfg.Transport,
		FlushInterval: -1, // stream SSE and chunked responses as they come
		ModifyResponse: func(resp *http.Response) error {
			resp.Header.Set(headerBackend, b.name)
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if r.Context().Err() != nil {
				// The client left; nobody to answer and not the backend's fault.
				if sw, ok := w.(*statusWriter); ok {
					sw.code = statusClientClosed
				}
				return
			}
			g.log.Warn("proxy error", "backend", b.name, "method", r.Method, "path", r.URL.Path, "err", err)
			w.Header().Set(headerBackend, b.name)
			writeError(w, http.StatusBadGateway, "bad_gateway", "backend "+b.name+" did not answer")
		},
	}
}

func (g *Gateway) forward(w http.ResponseWriter, r *http.Request, b *backend, class string) {
	sw := &statusWriter{ResponseWriter: w}
	start := time.Now()
	// Deferred because ReverseProxy panics with http.ErrAbortHandler when a
	// streamed body breaks, and that request must still be counted.
	defer func() {
		d := time.Since(start)
		code := sw.status()
		b.stats.record(time.Now(), d, code >= 500)
		g.metrics.requests.WithLabelValues(b.name, class, strconv.Itoa(code)).Inc()
		g.metrics.duration.WithLabelValues(b.name, class).Observe(d.Seconds())
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

// Run checks backend health every HealthInterval until ctx ends.
func (g *Gateway) Run(ctx context.Context) {
	client := &http.Client{Transport: g.cfg.Transport, Timeout: healthTimeout}
	t := time.NewTicker(g.cfg.HealthInterval)
	defer t.Stop()
	for {
		var wg sync.WaitGroup
		for _, b := range g.backends {
			wg.Go(func() { g.checkBackend(ctx, client, b) })
		}
		wg.Wait()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (g *Gateway) checkBackend(ctx context.Context, client *http.Client, b *backend) {
	err := probe(ctx, client, b.url.JoinPath(g.cfg.HealthPath).String())
	if ctx.Err() != nil {
		return // shutting down; keep the last real result
	}
	now := time.Now()
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	b.hmu.Lock()
	b.lastCheck, b.lastErr = now, msg
	b.hmu.Unlock()
	if was := b.healthy.Swap(err == nil); was != (err == nil) {
		if err != nil {
			g.log.Warn("backend unhealthy", "backend", b.name, "err", msg)
		} else {
			g.log.Info("backend healthy", "backend", b.name)
		}
	}
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
	if resp.StatusCode < 200 || resp.StatusCode > 399 {
		return fmt.Errorf("health check returned %s", resp.Status)
	}
	return nil
}
