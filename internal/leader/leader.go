// Package leader lets two API processes overlap during a rolling update
// (ADR 0065). One process at a time is the leader, which runs the migration
// runs, deployments, onboardings and reconcile loops; every process serves
// reads. A request only the leader may serve is forwarded to it, and while
// leadership moves from the old process to the new one it waits.
package leader

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

// ForwardedHeader marks a request one process forwarded to another. A
// process that is not the leader answers it 503 instead of forwarding it
// again, so a request never travels in a loop.
const ForwardedHeader = "X-Hopcharlie-Forwarded"

// Campaign takes leadership with try, every poll, until it gets it or ctx ends.
func Campaign(ctx context.Context, try func(context.Context) (store.Leadership, error), poll time.Duration, log *slog.Logger) (store.Leadership, error) {
	logged := false
	for {
		l, err := try(ctx)
		if err == nil {
			return l, nil
		}
		if !errors.Is(err, store.ErrConflict) {
			log.Warn("leadership attempt failed", "err", err)
		} else if !logged {
			log.Info("another process leads; serving reads and forwarding the rest until it hands over")
			logged = true
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(poll):
		}
	}
}

// Watch checks l every interval and calls lost once a check fails. It
// returns when ctx ends.
func Watch(ctx context.Context, l store.Leadership, interval time.Duration, lost func(error)) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		cctx, cancel := context.WithTimeout(ctx, interval)
		err := l.Check(cctx)
		cancel()
		if err != nil && ctx.Err() == nil {
			lost(err)
			return
		}
	}
}

// LeaderOnly reports whether r needs the leader: everything but reads, and
// the event streams, which follow the leader's live runs and deployments.
func LeaderOnly(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		return strings.HasSuffix(r.URL.Path, "/events")
	case http.MethodOptions:
		return false
	}
	return true
}

// Gate serves the requests LeaderOnly picks locally while this process is
// the active leader, and otherwise forwards them to the leader that
// LeaderAddr names, waiting up to Wait for one.
type Gate struct {
	Self       string                                    // this process's own address, never forwarded to
	LeaderAddr func(ctx context.Context) (string, error) // the leader's address, "" when there is none
	Wait       time.Duration                             // how long a request waits for a leader (default 30s)
	Poll       time.Duration                             // how often it looks again (default 250ms)
	Log        *slog.Logger

	// mu: a local leader request holds it shared while it runs, so
	// Deactivate waits for those already running.
	mu     sync.RWMutex
	active bool
}

// Activate makes this process serve leader requests itself.
func (g *Gate) Activate() {
	g.mu.Lock()
	g.active = true
	g.mu.Unlock()
}

// Deactivate stops serving leader requests here, after the ones running
// have finished; the ones that come later are forwarded.
func (g *Gate) Deactivate() {
	g.mu.Lock()
	g.active = false
	g.mu.Unlock()
}

// Wrap returns next behind the gate.
func (g *Gate) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !LeaderOnly(r) {
			next.ServeHTTP(w, r)
			return
		}
		g.forward(next, w, r)
	})
}

func (g *Gate) serveLocal(next http.Handler, w http.ResponseWriter, r *http.Request) bool {
	// Streams are reads: they need not hold up a deactivation.
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		g.mu.RLock()
		active := g.active
		g.mu.RUnlock()
		if active {
			next.ServeHTTP(w, r)
		}
		return active
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	if !g.active {
		return false
	}
	next.ServeHTTP(w, r)
	return true
}

func (g *Gate) forward(next http.Handler, w http.ResponseWriter, r *http.Request) {
	if g.serveLocal(next, w, r) {
		return
	}
	if r.Header.Get(ForwardedHeader) != "" {
		unavailable(w, "this server is not the leader")
		return
	}
	wait, poll := g.Wait, g.Poll
	if wait <= 0 {
		wait = 30 * time.Second
	}
	if poll <= 0 {
		poll = 250 * time.Millisecond
	}
	deadline := time.Now().Add(wait)
	for {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		addr, err := g.LeaderAddr(ctx)
		cancel()
		if err != nil && g.Log != nil {
			g.Log.Warn("looking up the leader failed", "err", err)
		}
		if err == nil && addr != "" && addr != g.Self {
			g.proxy(addr).ServeHTTP(w, r)
			return
		}
		if time.Now().Add(poll).After(deadline) {
			unavailable(w, "leadership is moving to another server")
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(poll):
		}
		// It may have become the leader while the request waited.
		if g.serveLocal(next, w, r) {
			return
		}
	}
}

func (g *Gate) proxy(addr string) *httputil.ReverseProxy {
	target := &url.URL{Scheme: "http", Host: addr}
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = pr.In.Host
			pr.Out.Header.Set(ForwardedHeader, "1")
		},
		FlushInterval: -1, // event streams
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if g.Log != nil {
				g.Log.Warn("forwarding to the leader failed", "leader", addr, "path", r.URL.Path, "err", err)
			}
			// The leader may or may not have acted on it: no retry here.
			writeError(w, http.StatusBadGateway, "leader_unreachable", "the leader did not answer; check the result before retrying")
		},
	}
}

func unavailable(w http.ResponseWriter, msg string) {
	w.Header().Set("Retry-After", "2")
	writeError(w, http.StatusServiceUnavailable, "leader_unavailable", msg)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(apiv1.Error{Error: apiv1.ErrorBody{Code: code, Message: msg}})
}
