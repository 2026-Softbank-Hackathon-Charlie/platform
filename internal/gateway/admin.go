package gateway

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

const maxAdminBody = 64 << 10

func (g *Gateway) adminMux() http.Handler {
	mux := http.NewServeMux()
	// healthz and metrics stay open so probes and Prometheus need no token.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, apiv1.Health{Status: "ok"})
	})
	mux.Handle("GET /metrics", promhttp.HandlerFor(g.metrics.reg, promhttp.HandlerOpts{}))
	mux.HandleFunc("GET /v1/state", g.auth(g.handleState))
	mux.HandleFunc("POST /v1/writes/hold", g.auth(g.handleHold))
	mux.HandleFunc("POST /v1/writes/release", g.auth(g.handleRelease))
	mux.HandleFunc("POST /v1/route", g.auth(g.handleRoute))
	mux.HandleFunc("POST /v1/weights", g.auth(g.handleWeights))
	return mux
}

func (g *Gateway) auth(h http.HandlerFunc) http.HandlerFunc {
	if g.cfg.AdminToken == "" {
		return h
	}
	want := []byte(g.cfg.AdminToken)
	return func(w http.ResponseWriter, r *http.Request) {
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || subtle.ConstantTimeCompare([]byte(got), want) != 1 {
			g.log.Warn("admin request denied", "method", r.Method, "path", r.URL.Path, "remote", r.RemoteAddr)
			writeError(w, http.StatusUnauthorized, "unauthorized", "missing or invalid bearer token")
			return
		}
		h(w, r)
	}
}

// handleHold is idempotent. It answers only once no write is in flight, so
// the engine can read the source's final LSN right after.
func (g *Gateway) handleHold(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	g.mu.Lock()
	changed := !g.held
	if changed {
		g.startHoldLocked(start)
	}
	inFlight := g.inflightWrites
	snap := g.snapshotLocked()
	g.mu.Unlock()
	if changed {
		g.save(snap)
	}

	ctx, cancel := context.WithTimeout(r.Context(), g.cfg.DrainTimeout)
	defer cancel()
	if err := g.waitWritesDrained(ctx); err != nil {
		// Stay held: the engine rolls back with route + release.
		g.mu.Lock()
		left := g.inflightWrites
		g.mu.Unlock()
		g.log.Warn("admin action", "action", "hold", "changed", changed, "result", "drain_timeout",
			"inFlightAtHold", inFlight, "inFlightLeft", left, "waitedMillis", time.Since(start).Milliseconds())
		writeError(w, http.StatusGatewayTimeout, "drain_timeout",
			fmt.Sprintf("writes are held, but %d in-flight writes did not finish in time (drain timeout %s)", left, g.cfg.DrainTimeout))
		return
	}
	g.log.Info("admin action", "action", "hold", "changed", changed, "result", "ok",
		"inFlightAtHold", inFlight, "drainMillis", time.Since(start).Milliseconds())
	writeJSON(w, http.StatusOK, g.state(defaultWindow))
}

// handleRelease is idempotent. Held writes go to the write backend as it is
// now, so route-then-release sends them to the new backend.
func (g *Gateway) handleRelease(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	g.mu.Lock()
	if !g.held {
		g.mu.Unlock()
		g.log.Info("admin action", "action", "release", "changed", false)
		writeJSON(w, http.StatusOK, g.state(defaultWindow))
		return
	}
	heldFor := start.Sub(g.heldSince)
	gen := g.releaseLocked()
	n := gen.pending
	snap := g.snapshotLocked()
	g.mu.Unlock()
	g.save(snap)

	select {
	case <-gen.dispatched:
	case <-r.Context().Done():
		// The caller left; the writes are dispatched regardless.
	}
	g.log.Info("admin action", "action", "release", "changed", true, "backend", gen.backend.name,
		"released", n, "heldMillis", heldFor.Milliseconds(), "dispatchMillis", time.Since(start).Milliseconds())
	writeJSON(w, http.StatusOK, g.state(defaultWindow))
}

func (g *Gateway) handleRoute(w http.ResponseWriter, r *http.Request) {
	var req apiv1.RouteRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if _, ok := g.byName[req.Backend]; !ok {
		writeError(w, http.StatusBadRequest, "unknown_backend",
			fmt.Sprintf("unknown backend %q (configured: %s)", req.Backend, strings.Join(g.names(), ", ")))
		return
	}
	g.mu.Lock()
	prev, held := g.writeBackend, g.held
	g.writeBackend = req.Backend
	g.readWeights = g.weightsAllTo(req.Backend)
	snap := g.snapshotLocked()
	g.mu.Unlock()
	g.save(snap)
	g.log.Info("admin action", "action", "route", "backend", req.Backend, "previous", prev, "writesHeld", held)
	writeJSON(w, http.StatusOK, g.state(defaultWindow))
}

func (g *Gateway) handleWeights(w http.ResponseWriter, r *http.Request) {
	var req apiv1.WeightsRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := g.validateWeights(req.ReadWeights); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_weights", err.Error())
		return
	}
	g.mu.Lock()
	prev := g.readWeights
	g.readWeights = g.fullWeights(req.ReadWeights)
	next := g.readWeights
	snap := g.snapshotLocked()
	g.mu.Unlock()
	g.save(snap)
	g.log.Info("admin action", "action", "weights", "readWeights", next, "previous", prev)
	writeJSON(w, http.StatusOK, g.state(defaultWindow))
}

func (g *Gateway) handleState(w http.ResponseWriter, r *http.Request) {
	window := defaultWindow
	if v := r.URL.Query().Get("window"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < time.Second || d > statsKeepSeconds*time.Second {
			writeError(w, http.StatusBadRequest, "invalid_window",
				fmt.Sprintf("window must be a duration from 1s to %ds, e.g. 20s", statsKeepSeconds))
			return
		}
		window = d
	}
	writeJSON(w, http.StatusOK, g.state(window))
}

// state builds the TrafficState with stats over window (whole seconds).
func (g *Gateway) state(window time.Duration) apiv1.TrafficState {
	now := time.Now()
	secs := int(window / time.Second)
	g.mu.Lock()
	st := apiv1.TrafficState{
		WriteBackend: g.writeBackend,
		ReadWeights:  maps.Clone(g.readWeights),
		WritesHeld:   g.held,
		HeldWrites:   g.waitingLocked(),
		MaxHold:      g.cfg.MaxHold.String(),
		InFlight:     apiv1.InFlight{Writes: g.inflightWrites},
		Backends:     make([]apiv1.BackendState, 0, len(g.backends)),
		Stats:        make(map[string]apiv1.BackendStats, len(g.backends)),
		Totals:       make(map[string]apiv1.BackendTotals, len(g.backends)),
		UpdatedAt:    now.UTC(),
	}
	if g.held {
		hs := g.heldSince.UTC()
		st.HeldSince = &hs
	}
	g.mu.Unlock()
	st.InFlight.Reads = int(g.inflightReads.Load())

	for _, b := range g.backends {
		bs := apiv1.BackendState{Name: b.name, Url: b.url.Redacted(), Healthy: b.healthy.Load()}
		b.hmu.Lock()
		if !b.lastCheck.IsZero() {
			lc := b.lastCheck.UTC()
			bs.LastCheck = &lc
		}
		if b.lastErr != "" {
			e := b.lastErr
			bs.LastError = &e
		}
		b.hmu.Unlock()
		st.Backends = append(st.Backends, bs)
		st.Stats[b.name] = b.stats.window(now, secs)
		st.Totals[b.name] = b.stats.totals()
	}
	return st
}

func (g *Gateway) validateWeights(w map[string]int) error {
	sum := 0
	for _, name := range slices.Sorted(maps.Keys(w)) {
		if _, ok := g.byName[name]; !ok {
			return fmt.Errorf("unknown backend %q (configured: %s)", name, strings.Join(g.names(), ", "))
		}
		if w[name] < 0 {
			return fmt.Errorf("weight for %s is negative", name)
		}
		sum += w[name]
	}
	if sum != 100 {
		return fmt.Errorf("read weights must sum to 100, got %d", sum)
	}
	return nil
}

// fullWeights lists every configured backend so the state and metrics show
// zeros explicitly.
func (g *Gateway) fullWeights(w map[string]int) map[string]int {
	out := make(map[string]int, len(g.backends))
	for _, b := range g.backends {
		out[b.name] = w[b.name]
	}
	return out
}

func (g *Gateway) weightsAllTo(name string) map[string]int {
	return g.fullWeights(map[string]int{name: 100})
}

func (g *Gateway) names() []string {
	out := make([]string, len(g.backends))
	for i, b := range g.backends {
		out[i] = b.name
	}
	return out
}

func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxAdminBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, apiv1.Error{Error: apiv1.ErrorBody{Code: code, Message: msg}})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
