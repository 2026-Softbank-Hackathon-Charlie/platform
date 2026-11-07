package gateway

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"regexp"
)

// A lease is a run-owned identifier, not an alternative to admin authentication.
var readExclusionLease = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

type ReadExclusionRequest struct {
	Backend string `json:"backend"`
	Lease   string `json:"lease"`
}

type readActivity struct {
	count int
	idle  chan struct{}
}

// reserveReadLocked registers a selected attempt before releasing the same
// mutex that selects it. Planned retries count too: they may touch the source
// after the first backend answers, even when no source request is running yet.
func (rt *router) reserveReadLocked(b *backend) {
	activity := rt.readActivity[b.name]
	if activity == nil {
		activity = &readActivity{idle: make(chan struct{})}
		rt.readActivity[b.name] = activity
	}
	activity.count++
}

func (rt *router) finishReadLocked(b *backend) {
	activity := rt.readActivity[b.name]
	activity.count--
	if activity.count == 0 {
		close(activity.idle)
		activity.idle = make(chan struct{})
	}
}

func (rt *router) finishReadAttempts(primary, alternative *backend) {
	rt.mu.Lock()
	rt.finishReadLocked(primary)
	if alternative != nil {
		rt.finishReadLocked(alternative)
	}
	rt.inflightReads.Add(-1)
	rt.mu.Unlock()
}

func (rt *router) notifyReadExclusionChangedLocked() {
	close(rt.readExclusionChanged)
	rt.readExclusionChanged = make(chan struct{})
}

var errReadExclusionChanged = errors.New("read exclusion lease changed while draining")

func (rt *router) waitReadsDrained(ctx context.Context, backend, lease string) error {
	for {
		rt.mu.Lock()
		if rt.readExclusions[backend] != lease {
			rt.mu.Unlock()
			return errReadExclusionChanged
		}
		activity := rt.readActivity[backend]
		if activity == nil || activity.count == 0 {
			rt.mu.Unlock()
			return nil
		}
		idle, changed := activity.idle, rt.readExclusionChanged
		rt.mu.Unlock()
		select {
		case <-idle:
		case <-changed:
		case <-ctx.Done():
			return ctx.Err()
		case <-rt.g.shutdown:
			return context.Canceled
		}
	}
}

func (rt *router) decodeReadExclusion(w http.ResponseWriter, r *http.Request) (ReadExclusionRequest, bool) {
	var req ReadExclusionRequest
	if !rt.g.decodeJSON(w, r, &req) {
		return req, false
	}
	if !backendName.MatchString(req.Backend) || !readExclusionLease.MatchString(req.Lease) {
		writeError(w, http.StatusBadRequest, "invalid_request", "backend and a 1-128 character lease identifier are required")
		return req, false
	}
	return req, true
}

func (rt *router) handleExcludeReads(w http.ResponseWriter, r *http.Request) {
	req, ok := rt.decodeReadExclusion(w, r)
	if !ok {
		return
	}
	rt.g.controlMu.Lock()
	rt.mu.Lock()
	if rt.byName[req.Backend] == nil {
		rt.mu.Unlock()
		rt.g.controlMu.Unlock()
		writeError(w, http.StatusBadRequest, "unknown_backend", "read exclusion backend is not configured")
		return
	}
	owner := rt.readExclusions[req.Backend]
	switch {
	case owner != "" && owner != req.Lease:
		rt.mu.Unlock()
		rt.g.controlMu.Unlock()
		readExclusionConflict(w)
		return
	case rt.g.cfg.StateFile == "":
		rt.mu.Unlock()
		rt.g.controlMu.Unlock()
		writeError(w, http.StatusServiceUnavailable, "read_exclusion_not_durable", "read exclusion requires a persistent state file")
		return
	case owner == "" && (!rt.held || rt.inflightWrites != 0):
		rt.mu.Unlock()
		rt.g.controlMu.Unlock()
		writeError(w, http.StatusConflict, "writes_not_drained", "hold and drain writes before excluding source reads")
		return
	}
	rt.readExclusions[req.Backend] = req.Lease
	rt.mu.Unlock()
	if err := rt.g.trySave(); err != nil {
		// A failed acknowledgment must never reopen the source in memory.
		rt.log.Error("persist source read exclusion", "backend", req.Backend, "err", err)
		rt.g.controlMu.Unlock()
		persistenceUnavailable(w)
		return
	}
	rt.g.controlMu.Unlock()
	ctx, cancel := context.WithTimeout(r.Context(), rt.g.cfg.DrainTimeout)
	defer cancel()
	if err := rt.waitReadsDrained(ctx, req.Backend, req.Lease); err != nil {
		if errors.Is(err, errReadExclusionChanged) {
			readExclusionConflict(w)
		} else {
			writeError(w, http.StatusServiceUnavailable, "read_drain_timeout", "source read attempts did not drain; the read exclusion remains active")
		}
		return
	}
	writeJSON(w, http.StatusOK, rt.state(defaultWindow))
}

func (rt *router) handleIncludeReads(w http.ResponseWriter, r *http.Request) {
	req, ok := rt.decodeReadExclusion(w, r)
	if !ok {
		return
	}
	rt.g.controlMu.Lock()
	defer rt.g.controlMu.Unlock()
	rt.mu.Lock()
	if rt.byName[req.Backend] == nil {
		rt.mu.Unlock()
		writeError(w, http.StatusBadRequest, "unknown_backend", "read exclusion backend is not configured")
		return
	}
	owner := rt.readExclusions[req.Backend]
	rt.mu.Unlock()
	if owner == "" {
		writeJSON(w, http.StatusOK, rt.state(defaultWindow))
		return
	}
	if owner != req.Lease {
		readExclusionConflict(w)
		return
	}
	// Save an included COPY first. The live exclusion stays active while disk
	// writes run, and no conflicting control change can acquire controlMu.
	if err := rt.g.persistReadInclusion(rt, req); err != nil {
		rt.log.Error("persist source read inclusion", "backend", req.Backend, "err", err)
		persistenceUnavailable(w)
		return
	}
	writeJSON(w, http.StatusOK, rt.state(defaultWindow))
}

// persistReadInclusion is called with controlMu held and no router/apps lock.
func (g *Gateway) persistReadInclusion(rt *router, req ReadExclusionRequest) error {
	if g.cfg.StateFile == "" {
		return errors.New("read exclusion state file is not configured")
	}
	g.saveMu.Lock()
	defer g.saveMu.Unlock()
	st := g.snapshot()
	if rt == g.router {
		delete(st.ReadExclusions, req.Backend)
	} else {
		app := st.Apps[rt.name]
		app.ReadExclusions = maps.Clone(app.ReadExclusions)
		delete(app.ReadExclusions, req.Backend)
		st.Apps[rt.name] = app
	}
	err := writeFileAtomic(g.cfg.StateFile, st)
	g.persistenceFailed.Store(err != nil)
	if err == nil {
		// Commit memory before releasing saveMu: a concurrent background save
		// must not snapshot the old exclusion after its removal reached disk.
		rt.mu.Lock()
		delete(rt.readExclusions, req.Backend)
		rt.notifyReadExclusionChangedLocked()
		rt.mu.Unlock()
	}
	return err
}

func readExclusionConflict(w http.ResponseWriter) {
	writeError(w, http.StatusConflict, "read_exclusion_conflict", "the source read exclusion belongs to a different lease or changed while draining")
}

func readsExcluded(w http.ResponseWriter) {
	writeError(w, http.StatusConflict, "read_exclusion_active", "a source read exclusion is active; restore it with its owner lease before this control change")
}
