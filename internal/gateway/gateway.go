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
//	POST /v1/writes/hold              hold new writes; 200 once in-flight writes drained,
//	                                  504 (hold reverted) when they do not drain in time,
//	                                  409 when released meanwhile
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
	"maps"
	"net/http"
	"net/http/httputil"
	"net/url"
	"slices"
	"strconv"
	"strings"
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
	holdSeq        uint64   // id of the last hold started
	buffering      int      // held writes still reading their body; count towards MaxHeld
	inflightWrites int      // includes released writes not yet dispatched
	writesIdle     chan struct{}
	shuttingDown   bool
	shutdown       chan struct{} // closed by BeginShutdown
	seq            uint64

	saveMu   sync.Mutex
	savedSeq uint64
}

// New builds a gateway and restores routing from the state file, if any. A
// hold recorded there continues until release.
func New(cfg Config) (*Gateway, error) {
	cfg.setDefaults()
	if len(cfg.Backends) == 0 {
		return nil, errors.New("no backends configured")
	}
	g := &Gateway{cfg: cfg, log: cfg.Log, byName: map[string]*backend{},
		writesIdle: make(chan struct{}), shutdown: make(chan struct{})}
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
		weights, err := g.restoredWeights(st)
		if err != nil {
			g.mu.Unlock()
			return fmt.Errorf("state file %s: %w", g.cfg.StateFile, err)
		}
		g.writeBackend = st.WriteBackend
		g.readWeights = weights
		if st.WritesHeld {
			since := time.Now()
			if st.HeldSince != nil {
				since = *st.HeldSince
			}
			g.startHoldLocked(since)
			g.log.Warn("resuming write hold from state file; it lasts until release", "heldSince", since.UTC())
		}
	}
	snap := g.snapshotLocked()
	g.mu.Unlock()
	// Fail at start, not mid-cutover, when the state file cannot be written.
	if err := g.trySave(snap); err != nil {
		return fmt.Errorf("write state file: %w", err)
	}
	return nil
}

// restoredWeights checks the state file against the configured backends. A
// backend removed from the config is dropped if it got no reads; anything
// else is refused rather than guessed, since routing differently from what
// was recorded could split writes mid-migration.
func (g *Gateway) restoredWeights(st *persistedState) (map[string]int, error) {
	configured := strings.Join(g.names(), ", ")
	if _, ok := g.byName[st.WriteBackend]; !ok {
		return nil, fmt.Errorf("write backend %q is not configured (configured: %s); add it back to the backends or fix the state file",
			st.WriteBackend, configured)
	}
	weights := make(map[string]int, len(st.ReadWeights))
	for _, name := range slices.Sorted(maps.Keys(st.ReadWeights)) {
		w := st.ReadWeights[name]
		if _, ok := g.byName[name]; !ok {
			if w == 0 {
				g.log.Warn("dropping unconfigured backend with read weight 0 from state file", "backend", name)
				continue
			}
			return nil, fmt.Errorf("backend %q has read weight %d but is not configured (configured: %s); add it back to the backends or fix the state file",
				name, w, configured)
		}
		weights[name] = w
	}
	if err := g.validateWeights(weights); err != nil {
		return nil, err
	}
	return g.fullWeights(weights), nil
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
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.shuttingDown {
		g.shuttingDown = true
		close(g.shutdown)
	}
}

// Close persists the final state. Call it after the servers have stopped; a
// hold in progress stays recorded as held.
func (g *Gateway) Close() {
	g.mu.Lock()
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
	arrived := time.Now()
	buffered := false
	g.mu.Lock()
	for g.held {
		if g.shuttingDown {
			g.mu.Unlock()
			retryLater(w, "shutting_down", "the gateway is restarting; retry shortly")
			return
		}
		if !buffered {
			if g.waitingLocked() >= g.cfg.MaxHeld {
				g.mu.Unlock()
				retryLater(w, "too_many_held_writes", "writes are paused for a migration; retry shortly")
				return
			}
			// Take the slot before reading the body, so MaxHeld bounds the
			// memory held writes use.
			g.buffering++
			g.mu.Unlock()
			err := bufferBody(r)
			g.mu.Lock()
			g.buffering--
			if err != nil {
				g.mu.Unlock()
				g.metrics.heldDropped.Inc()
				writeError(w, http.StatusBadRequest, "bad_request", "could not read the request body: "+err.Error())
				return
			}
			buffered = true
			continue // the hold may have ended while the body was read
		}
		gen := g.gen
		gen.waiting++
		g.mu.Unlock()
		b := g.awaitRelease(w, r, gen, arrived)
		if b == nil {
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

func retryLater(w http.ResponseWriter, code, msg string) {
	w.Header().Set("Retry-After", "1")
	writeError(w, http.StatusServiceUnavailable, code, msg)
}

// awaitRelease blocks until gen is released and returns the backend to send
// the write to; a held write is never forwarded before that. A write that
// cannot wait any longer (MaxHold since it arrived, or the gateway is shutting
// down) is answered with 503, and one whose client left is dropped. Then
// awaitRelease returns nil.
func (g *Gateway) awaitRelease(w http.ResponseWriter, r *http.Request, gen *holdGen, arrived time.Time) *backend {
	timeout := time.NewTimer(max(g.cfg.MaxHold-time.Since(arrived), 0))
	defer timeout.Stop()
	var gone, expired bool
	select {
	case <-gen.release:
	case <-r.Context().Done():
		gone = true
	case <-timeout.C:
		expired = true
	case <-g.shutdown:
	}
	g.mu.Lock()
	if gen.released && !gone {
		// Released, perhaps at the same moment as a timeout or shutdown:
		// forwarding is safe now, so nobody gets an error.
		gen.dispatchedOneLocked()
		g.mu.Unlock()
		return gen.backend
	}
	if gen.released {
		// Release won the race and already counted this write as in flight.
		gen.dispatchedOneLocked()
		g.writeDoneLocked()
	} else {
		gen.waiting--
	}
	g.mu.Unlock()
	switch {
	case gone:
		g.metrics.heldDropped.Inc()
	case expired:
		g.metrics.holdExpired.Inc()
		retryLater(w, "hold_timeout", fmt.Sprintf("writes are paused for a migration and this one waited %s; retry shortly", g.cfg.MaxHold))
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

// waitingLocked counts writes waiting for release, including those still
// reading their body.
func (g *Gateway) waitingLocked() int {
	if g.gen == nil {
		return 0
	}
	return g.gen.waiting + g.buffering
}

// startHoldLocked starts a hold period. It lasts until releaseLocked.
func (g *Gateway) startHoldLocked(since time.Time) {
	g.holdSeq++
	gen := &holdGen{id: g.holdSeq, release: make(chan struct{}), dispatched: make(chan struct{})}
	g.held, g.heldSince, g.gen = true, since, gen
}

// releaseLocked ends the hold and hands every waiting write to the current
// write backend. They count as in flight from here, so a hold that follows
// right away still waits for them.
func (g *Gateway) releaseLocked() *holdGen {
	gen := g.gen
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
			// chain instead of replacing it with cloudflared's own address,
			// and its X-Forwarded-Proto (https at the edge) instead of the
			// plain http of the tunnel hop.
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
				g.log.Warn("write timed out", "backend", b.name, "method", r.Method, "path", r.URL.Path,
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
				g.log.Warn("proxy error", "backend", b.name, "method", r.Method, "path", r.URL.Path, "err", err)
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

func (g *Gateway) forward(w http.ResponseWriter, r *http.Request, b *backend, class string) {
	if class == classWrite {
		// A write stays in flight until the backend answers, even when its
		// client leaves: the backend may still commit it, and hold must wait
		// for that. The timeout also gives ReverseProxy a context it can end.
		ctx, cancel := context.WithTimeoutCause(context.WithoutCancel(r.Context()), g.cfg.WriteTimeout, errWriteTimeout)
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
