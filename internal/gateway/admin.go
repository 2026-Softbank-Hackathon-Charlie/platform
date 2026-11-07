package gateway

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	mux.Handle("GET /metrics", promhttp.HandlerFor(g.prom.reg, promhttp.HandlerOpts{}))
	// Liveness remains independent: removing a serving gateway from traffic
	// cannot repair its disk. This diagnostic reports control-plane durability.
	mux.HandleFunc("GET /readyz", g.durabilityHealth)
	// The default app's API, and the same API for any app under
	// /apps/{app}, which therefore works as a gateway base URL (engine,
	// Client.App).
	perApp := []struct {
		pattern string
		h       func(*router, http.ResponseWriter, *http.Request)
	}{
		{"GET /v1/state", (*router).handleState},
		{"GET /v1/ai/requests", (*router).handleAIRequests},
		{"POST /v1/writes/hold", (*router).handleHold},
		{"POST /v1/writes/release", (*router).handleRelease},
		{"POST /v1/route", (*router).handleRoute},
		{"POST /v1/weights", (*router).handleWeights},
		{"POST /v1/reads/exclude", (*router).handleExcludeReads},
		{"POST /v1/reads/include", (*router).handleIncludeReads},
	}
	for _, e := range perApp {
		method, path, _ := strings.Cut(e.pattern, " ")
		mux.HandleFunc(e.pattern, g.auth(func(w http.ResponseWriter, r *http.Request) { e.h(g.router, w, r) }))
		mux.HandleFunc(method+" /apps/{app}"+path, g.auth(g.scoped(e.h)))
	}
	mux.HandleFunc("GET /apps/{app}/healthz", g.auth(g.scoped(func(_ *router, w http.ResponseWriter, _ *http.Request) {
		g.durabilityHealth(w, nil)
	})))
	mux.HandleFunc("GET /apps", g.auth(g.handleListApps))
	mux.HandleFunc("PUT /apps/{app}", g.auth(g.handlePutApp))
	mux.HandleFunc("DELETE /apps/{app}", g.auth(g.handleDeleteApp))
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

// handleHold is idempotent. It answers 200 only once no write is in flight
// and the same hold is still on, so the engine can read the source's final
// LSN right after. The hold is all or nothing: the engine treats writes as
// held only after a 200 (and releases nothing otherwise), so a hold that does
// not drain in time is reverted before answering.
func (rt *router) handleHold(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	rt.g.controlMu.Lock()
	rt.mu.Lock()
	changed := !rt.held
	if changed {
		rt.startHoldLocked(start)
	}
	id := rt.gen.id
	inFlight := rt.inflightWrites
	rt.mu.Unlock()
	// No successful hold may be acknowledged before it survives a restart.
	// Other administrative changes cannot race this first persistence attempt.
	if err := rt.g.trySave(); err != nil {
		rt.log.Error("persist gateway hold state", "err", err, "newHoldReverted", changed)
		if changed {
			rt.mu.Lock()
			rt.releaseLocked()
			rt.mu.Unlock()
			rt.metrics.writePause.WithLabelValues("persistence_reverted").Observe(time.Since(start).Seconds())
		}
		rt.g.controlMu.Unlock()
		persistenceUnavailable(w)
		return
	}
	rt.g.controlMu.Unlock()

	ctx, cancel := context.WithTimeout(r.Context(), rt.g.cfg.DrainTimeout)
	defer cancel()
	drainErr := rt.waitWritesDrained(ctx)

	rt.mu.Lock()
	if rt.gen == nil || rt.gen.id != id {
		// Released (or released and held again) while draining: this call's
		// hold is over, so it must not report writes as held.
		rt.mu.Unlock()
		rt.log.Warn("admin action", "action", "hold", "changed", changed, "hold", id, "result", "hold_cancelled",
			"waitedMillis", time.Since(start).Milliseconds())
		writeError(w, http.StatusConflict, "hold_cancelled", "the hold was released before in-flight writes drained")
		return
	}
	if rt.inflightWrites == 0 {
		// While a hold is on nothing new is forwarded, so this stays true
		// until release.
		rt.mu.Unlock()
		rt.log.Info("admin action", "action", "hold", "changed", changed, "hold", id, "result", "ok",
			"inFlightAtHold", inFlight, "drainMillis", time.Since(start).Milliseconds())
		writeJSON(w, http.StatusOK, rt.state(defaultWindow))
		return
	}
	// Drain timeout, or the caller left. Revert. The engine routes only after
	// a successful hold, so the write backend is still the one the in-flight
	// writes went to, and the waiting writes can safely follow them.
	left := rt.inflightWrites
	heldFor := time.Since(rt.heldSince)
	// Do not race a route/release while reverting this hold.
	// Release already takes rt.mu; the generation check above protects it.
	gen := rt.releaseLocked()
	// Read under the lock: released writes decrement gen.pending as they go.
	released, backend := gen.pending, gen.backend.name
	rt.mu.Unlock()
	rt.g.save()
	rt.metrics.writePause.WithLabelValues("drain_reverted").Observe(heldFor.Seconds())
	rt.log.Warn("admin action", "action", "hold", "changed", changed, "hold", id, "result", "drain_timeout",
		"reverted", true, "err", drainErr, "inFlightAtHold", inFlight, "inFlightLeft", left,
		"released", released, "backend", backend, "waitedMillis", time.Since(start).Milliseconds())
	writeError(w, http.StatusGatewayTimeout, "drain_timeout",
		fmt.Sprintf("%d in-flight writes did not finish within %s; the hold was reverted and writes flow to %s again",
			left, rt.g.cfg.DrainTimeout, backend))
}

// handleRelease is idempotent. Held writes go to the write backend as it is
// now, so route-then-release sends them to the new backend.
func (rt *router) handleRelease(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	rt.g.controlMu.Lock()
	rt.mu.Lock()
	excludedWriter := rt.readExclusions[rt.writeBackend] != ""
	rt.mu.Unlock()
	if excludedWriter {
		rt.g.controlMu.Unlock()
		readsExcluded(w)
		return
	}
	// Save the current route while the hold is still on. If release's later
	// save fails, restart conservatively restores this route with writes held.
	if err := rt.g.trySave(); err != nil {
		rt.log.Error("persist gateway route before release", "err", err)
		rt.g.controlMu.Unlock()
		persistenceUnavailable(w)
		return
	}
	rt.mu.Lock()
	if !rt.held {
		rt.mu.Unlock()
		rt.g.controlMu.Unlock()
		rt.log.Info("admin action", "action", "release", "changed", false)
		writeJSON(w, http.StatusOK, rt.state(defaultWindow))
		return
	}
	heldFor := time.Since(rt.heldSince)
	rt.metrics.writePause.WithLabelValues("released").Observe(heldFor.Seconds())
	gen := rt.releaseLocked()
	n := gen.pending
	rt.mu.Unlock()
	rt.g.save()
	rt.g.controlMu.Unlock()

	select {
	case <-gen.dispatched:
	case <-r.Context().Done():
		// The caller left; the writes are dispatched regardless.
	}
	rt.log.Info("admin action", "action", "release", "changed", true, "backend", gen.backend.name,
		"released", n, "heldMillis", heldFor.Milliseconds(), "dispatchMillis", time.Since(start).Milliseconds())
	writeJSON(w, http.StatusOK, rt.state(defaultWindow))
}

func (rt *router) handleRoute(w http.ResponseWriter, r *http.Request) {
	var req apiv1.RouteRequest
	if !rt.g.decodeJSON(w, r, &req) {
		return
	}
	rt.g.controlMu.Lock()
	defer rt.g.controlMu.Unlock()
	rt.mu.Lock()
	if _, ok := rt.byName[req.Backend]; !ok {
		names := rt.namesLocked()
		rt.mu.Unlock()
		writeError(w, http.StatusBadRequest, "unknown_backend",
			fmt.Sprintf("unknown backend %q (configured: %s)", req.Backend, strings.Join(names, ", ")))
		return
	}
	if rt.readExclusions[req.Backend] != "" {
		rt.mu.Unlock()
		readsExcluded(w)
		return
	}
	prev, held := rt.writeBackend, rt.held
	if held && rt.inflightWrites > 0 && prev != req.Backend {
		rt.mu.Unlock()
		writeError(w, http.StatusConflict, "writes_draining", "wait for existing writes to drain before changing the write backend")
		return
	}
	rt.writeBackend = req.Backend
	rt.readWeights = rt.weightsAllToLocked(req.Backend)
	rt.mu.Unlock()
	rt.g.save()
	rt.log.Info("admin action", "action", "route", "backend", req.Backend, "previous", prev, "writesHeld", held)
	writeJSON(w, http.StatusOK, rt.state(defaultWindow))
}

func (rt *router) handleWeights(w http.ResponseWriter, r *http.Request) {
	var req apiv1.WeightsRequest
	if !rt.g.decodeJSON(w, r, &req) {
		return
	}
	rt.g.controlMu.Lock()
	defer rt.g.controlMu.Unlock()
	rt.mu.Lock()
	if err := validateWeights(req.ReadWeights, rt.namesLocked()); err != nil {
		rt.mu.Unlock()
		writeError(w, http.StatusBadRequest, "invalid_weights", err.Error())
		return
	}
	for backend, weight := range req.ReadWeights {
		if weight > 0 && rt.readExclusions[backend] != "" {
			rt.mu.Unlock()
			readsExcluded(w)
			return
		}
	}
	prev := rt.readWeights
	rt.readWeights = rt.fullWeightsLocked(req.ReadWeights)
	next := rt.readWeights
	rt.mu.Unlock()
	rt.g.save()
	rt.log.Info("admin action", "action", "weights", "readWeights", next, "previous", prev)
	writeJSON(w, http.StatusOK, rt.state(defaultWindow))
}

func persistenceUnavailable(w http.ResponseWriter) {
	writeError(w, http.StatusServiceUnavailable, "persistence_unavailable", "gateway state could not be saved; check routing and hold state before retrying the migration")
}

func (g *Gateway) durabilityHealth(w http.ResponseWriter, _ *http.Request) {
	if g.persistenceFailed.Load() {
		persistenceUnavailable(w)
		return
	}
	writeJSON(w, http.StatusOK, apiv1.Health{Status: "ok"})
}

func (rt *router) handleState(w http.ResponseWriter, r *http.Request) {
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
	writeJSON(w, http.StatusOK, rt.state(window))
}

// state builds the TrafficState with stats over window (whole seconds).
func (rt *router) state(window time.Duration) apiv1.TrafficState {
	now := time.Now()
	secs := int(window / time.Second)
	rt.mu.Lock()
	backends := rt.backends
	st := apiv1.TrafficState{
		WriteBackend: rt.writeBackend,
		ReadWeights:  maps.Clone(rt.readWeights),
		WritesHeld:   rt.held,
		HeldWrites:   rt.waitingLocked(),
		MaxHold:      rt.g.cfg.MaxHold.String(),
		InFlight:     apiv1.InFlight{Writes: rt.inflightWrites},
		Backends:     make([]apiv1.BackendState, 0, len(backends)),
		Stats:        make(map[string]apiv1.BackendStats, len(backends)),
		Totals:       make(map[string]apiv1.BackendTotals, len(backends)),
		UpdatedAt:    now.UTC(),
	}
	supported := rt.g.cfg.StateFile != ""
	exclusions := maps.Clone(rt.readExclusions)
	inFlight := make(map[string]int, len(backends))
	for _, b := range backends {
		inFlight[b.name] = 0
		if activity := rt.readActivity[b.name]; activity != nil {
			inFlight[b.name] = activity.count
		}
	}
	st.SourceReadExclusionSupported = &supported
	st.ReadExclusions = &exclusions
	st.ReadInFlight = &inFlight
	if rt.held {
		hs := rt.heldSince.UTC()
		st.HeldSince = &hs
	}
	rt.mu.Unlock()
	st.InFlight.Reads = int(rt.inflightReads.Load())

	for _, b := range backends {
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

// validateWeights checks read weights against the backend names.
func validateWeights(w map[string]int, names []string) error {
	sum := 0
	for _, name := range slices.Sorted(maps.Keys(w)) {
		if !slices.Contains(names, name) {
			return fmt.Errorf("unknown backend %q (configured: %s)", name, strings.Join(names, ", "))
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

// fullWeightsLocked lists every configured backend so the state and metrics
// show zeros explicitly.
func (rt *router) fullWeightsLocked(w map[string]int) map[string]int {
	out := make(map[string]int, len(rt.backends))
	for _, b := range rt.backends {
		out[b.name] = w[b.name]
	}
	return out
}

func (rt *router) weightsAllToLocked(name string) map[string]int {
	return rt.fullWeightsLocked(map[string]int{name: 100})
}

func (rt *router) namesLocked() []string {
	out := make([]string, len(rt.backends))
	for i, b := range rt.backends {
		out[i] = b.name
	}
	return out
}

func (g *Gateway) decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	// Bound the entire body, not only the prefix the first Decode consumes.
	// Only receive time is bounded: Hold's drain and Release's dispatch waits
	// are independent and may legitimately exceed this duration.
	deadline := time.Now().Add(g.cfg.AdminBodyReadTimeout)
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(deadline)
	body := http.MaxBytesReader(w, r.Body, maxAdminBody)
	done := make(chan struct{})
	timer := time.AfterFunc(time.Until(deadline), func() { _ = body.Close(); close(done) })
	raw, err := io.ReadAll(body)
	if !timer.Stop() {
		<-done
	}
	if !time.Now().Before(deadline) {
		// Keep the expired network read deadline until this connection closes:
		// net/http must not wait for the rest of a stalled body after the 408.
		w.Header().Set("Connection", "close")
		writeError(w, http.StatusRequestTimeout, "body_timeout", "admin request body did not arrive in time; no control change was applied")
		return false
	}
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "body_too_large", "admin request body exceeds 64 KiB")
		} else {
			writeError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body: "+err.Error())
		}
		return false
	}
	_ = controller.SetReadDeadline(time.Time{})
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body: "+err.Error())
		return false
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid_request", "JSON body must contain exactly one value")
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
