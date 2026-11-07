package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash/fnv"
	"net/http"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/llmquality"
)

type QualityProvider interface {
	Quality(context.Context, llmquality.Policy, time.Time, time.Time) llmquality.Verdict
}

type AIQualityConfig struct {
	Policy   llmquality.Policy
	Provider QualityProvider
}

// Only the dedicated, non-streaming native inference route supports canary.
// Database writeBackend is never changed by this controller.
type AICanaryState struct {
	Version             uint64             `json:"version"`
	PolicyDigest        string             `json:"policyDigest"`
	TargetPercent       int                `json:"targetPercent"`
	LastAdvanceAt       time.Time          `json:"lastAdvanceAt"`
	Verdict             llmquality.Verdict `json:"verdict"`
	LastCommand         string             `json:"lastCommand,omitempty"`
	LastExpectedVersion uint64             `json:"lastExpectedVersion,omitempty"`
	LastTargetPercent   int                `json:"lastTargetPercent,omitempty"`
}

type advanceAIRequest struct {
	ExpectedVersion *uint64 `json:"expectedVersion"`
	TargetPercent   int     `json:"targetPercent"`
}

func (rt *router) qualityEnabled() bool { return rt.aiEnabled() && rt.g.cfg.AIQuality != nil }

func (g *Gateway) initializeAIQuality(st *persistedState) error {
	var saved *AICanaryState
	if st != nil {
		saved = st.AICanary
	}
	if g.cfg.AIQuality == nil {
		if saved != nil {
			return errors.New("persisted AI quality control requires its policy configuration")
		}
		return nil
	}
	cfg := g.cfg.AIQuality
	if !g.aiEnabled() || cfg.Provider == nil || cfg.Policy.Validate() != nil || cfg.Policy.App != g.name || cfg.Policy.SourceBackend != "gcp" || cfg.Policy.TargetBackend != "laptop" {
		return errors.New("invalid AI quality configuration")
	}
	if g.writeBackend != cfg.Policy.SourceBackend {
		return errors.New("enable AI quality control only with the source as write backend")
	}
	encoded, _ := json.Marshal(cfg.Policy)
	digest := sha256.Sum256(encoded)
	state := AICanaryState{PolicyDigest: hex.EncodeToString(digest[:]), Verdict: llmquality.Unknown(time.Now(), "scores_not_checked")}
	if saved != nil {
		if saved.PolicyDigest != state.PolicyDigest || !canaryStep(saved.TargetPercent) {
			return errors.New("persisted AI policy differs from configured policy")
		}
		state = *saved
		// A restart cannot reuse a decision that may have regressed while down.
		state.Verdict = llmquality.Unknown(time.Now(), "scores_not_checked_after_restart")
		// Never count downtime or an earlier persistence-intent timestamp as
		// observation time for a newly reactivated process.
		if state.Version > 0 || state.TargetPercent > 0 {
			state.LastAdvanceAt = time.Now().UTC()
		}
	}
	g.canary = &state
	return nil
}

func canaryStep(n int) bool { return n == 0 || n == 5 || n == 25 || n == 50 || n == 100 }
func nextCanaryStep(n int) int {
	switch n {
	case 0:
		return 5
	case 5:
		return 25
	case 25:
		return 50
	case 50:
		return 100
	}
	return -1
}

// Caller holds rt.mu. Hashing a request ID pins its single attempt; no shadow
// inference, retry to the other model, duplicated tool call or body replay occurs.
func (rt *router) aiBackendLocked(id string, now time.Time) *backend {
	if !rt.qualityEnabled() {
		return rt.byName[rt.writeBackend]
	}
	p := rt.g.cfg.AIQuality.Policy
	name := p.SourceBackend
	if c := rt.canary; c != nil && c.TargetPercent > 0 && c.Verdict.Status == "pass" && c.Verdict.ValidUntil.After(now) && !rt.g.persistenceFailed.Load() {
		h := fnv.New32a()
		_, _ = h.Write([]byte(id))
		target := rt.byName[p.TargetBackend]
		if target != nil && target.healthy.Load() && rt.readExclusions[p.TargetBackend] == "" && int(h.Sum32()%100) < c.TargetPercent {
			name = p.TargetBackend
		}
	}
	b := rt.byName[name]
	if b == nil || !b.healthy.Load() || rt.readExclusions[name] != "" {
		return nil
	}
	return b
}

func (rt *router) handleAICanary(w http.ResponseWriter, r *http.Request) {
	if !rt.qualityEnabled() {
		writeError(w, 404, "not_found", "AI quality control disabled")
		return
	}
	rt.mu.Lock()
	state := *rt.canary
	if state.Verdict.Status == "pass" && !state.Verdict.ValidUntil.After(time.Now()) {
		state.Verdict = llmquality.Unknown(time.Now(), "scores_expired")
	}
	rt.mu.Unlock()
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, rt.canaryView(state))
}

func (rt *router) canaryView(state AICanaryState) any {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	effective := 0
	p := rt.g.cfg.AIQuality.Policy
	target := rt.byName[p.TargetBackend]
	if target != nil && state.Verdict.Status == "pass" && state.Verdict.ValidUntil.After(time.Now()) && !rt.g.persistenceFailed.Load() && !rt.held && target.healthy.Load() && rt.readExclusions[p.TargetBackend] == "" {
		effective = state.TargetPercent
	}
	return struct {
		AICanaryState
		EffectiveTargetPercent int    `json:"effectiveTargetPercent"`
		SourceBackend          string `json:"sourceBackend"`
		TargetBackend          string `json:"targetBackend"`
	}{state, effective, p.SourceBackend, p.TargetBackend}
}

func (rt *router) handleAdvanceAI(w http.ResponseWriter, r *http.Request) {
	if !rt.qualityEnabled() {
		writeError(w, 404, "not_found", "AI quality control disabled")
		return
	}
	var req advanceAIRequest
	if !rt.g.decodeJSON(w, r, &req) {
		return
	}
	id := r.Header.Get("Idempotency-Key")
	if !aiRequestID.MatchString(id) || req.ExpectedVersion == nil || !canaryStep(req.TargetPercent) {
		writeError(w, 400, "invalid_canary_request", "expectedVersion, targetPercent and Idempotency-Key are required")
		return
	}
	rt.mu.Lock()
	prev := *rt.canary
	observedQualityGeneration := rt.qualityGeneration
	rt.mu.Unlock()
	if prev.LastCommand == id {
		if prev.LastExpectedVersion != *req.ExpectedVersion || prev.LastTargetPercent != req.TargetPercent {
			writeError(w, 409, "idempotency_conflict", "command key was used for another transition")
			return
		}
		writeJSON(w, 200, rt.canaryView(prev))
		return
	}
	if prev.Version != *req.ExpectedVersion || req.TargetPercent != 0 && req.TargetPercent != nextCanaryStep(prev.TargetPercent) {
		writeError(w, 409, "canary_version_or_step_conflict", "reload state and advance one step: 0, 5, 25, 50, 100")
		return
	}
	now := time.Now().UTC()
	if req.TargetPercent > 0 && !prev.LastAdvanceAt.IsZero() && now.Sub(prev.LastAdvanceAt) < time.Duration(rt.g.cfg.AIQuality.Policy.MinSoakSeconds)*time.Second {
		writeError(w, 409, "canary_soak_incomplete", "wait for the configured observation interval")
		return
	}
	verdict := llmquality.Unknown(now, "operator_rollback")
	if req.TargetPercent > 0 {
		ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
		verdict = rt.g.cfg.AIQuality.Provider.Quality(ctx, rt.g.cfg.AIQuality.Policy, now, prev.LastAdvanceAt)
		cancel()
		if r.Context().Err() != nil {
			return
		}
		if verdict.Status != "pass" || !verdict.ValidUntil.After(time.Now()) {
			writeJSON(w, http.StatusPreconditionFailed, verdict)
			return
		}
	}
	if !rt.lockControl(w) {
		return
	}
	defer rt.g.controlMu.Unlock()
	if r.Context().Err() != nil {
		rt.mu.Unlock()
		return
	}
	if rt.canary.Version != prev.Version {
		rt.mu.Unlock()
		writeError(w, 409, "canary_version_conflict", "another transition changed the canary")
		return
	}
	// A watcher may revoke evidence without changing a 0% stage/version. An
	// earlier, slower passing fetch must not overwrite that newer revocation.
	if req.TargetPercent > 0 && rt.canary.Verdict.Status != "pass" && rt.qualityGeneration != observedQualityGeneration {
		latest := rt.canary.Verdict
		rt.mu.Unlock()
		writeJSON(w, http.StatusPreconditionFailed, latest)
		return
	}
	if req.TargetPercent > 0 && !verdict.ValidUntil.After(time.Now()) {
		rt.mu.Unlock()
		writeJSON(w, http.StatusPreconditionFailed, llmquality.Unknown(time.Now(), "scores_expired_waiting_for_control"))
		return
	}
	if req.TargetPercent > 0 && rt.held || rt.g.shuttingDown() {
		rt.mu.Unlock()
		writeError(w, 409, "ai_routing_held", "finish the existing write hold before a canary transition")
		return
	}
	next := prev
	next.Version++
	next.TargetPercent = req.TargetPercent
	next.LastAdvanceAt = time.Now().UTC() // persistence intent; activation is later
	next.Verdict = verdict
	next.LastCommand = id
	next.LastExpectedVersion = *req.ExpectedVersion
	next.LastTargetPercent = req.TargetPercent
	// Persist before making the increased target share visible to admission.
	// The previous source-only/stage state remains active while disk is written.
	if err := rt.g.saveAICanaryLocked(&next); err != nil {
		rt.mu.Unlock()
		persistenceUnavailable(w)
		return
	}
	rt.mu.Unlock()
	rt.log.Info("admin action", "action", "ai_canary_advance", "command", id, "version", next.Version, "previousPercent", prev.TargetPercent, "targetPercent", next.TargetPercent, "quality", verdict.Status, "batchDigest", verdict.Batch)
	writeJSON(w, 200, rt.canaryView(next))
}

// saveAICanaryLocked keeps lock order controlMu -> saveMu -> rt.mu safe by
// unlocking rt.mu before snapshot. controlMu prevents another admin mutation.
func (g *Gateway) saveAICanaryLocked(next *AICanaryState) error {
	g.mu.Unlock()
	if g.cfg.StateFile == "" {
		g.mu.Lock()
		g.activateAIStageLocked(next)
		return nil
	}
	g.saveMu.Lock()
	state := g.snapshot()
	state.AICanary = next
	err := writeFileAtomic(g.cfg.StateFile, state)
	g.persistenceFailed.Store(err != nil)
	// Update memory before releasing saveMu: a competing snapshot must never
	// overwrite the newly durable stage with the previous in-memory stage.
	g.mu.Lock()
	if err == nil {
		g.activateAIStageLocked(next)
	}
	g.saveMu.Unlock()
	return err
}

// Publication happens only after persistence. The durable timestamp records
// intent; restart resets its observation boundary conservatively to startup.
func (g *Gateway) activateAIStageLocked(next *AICanaryState) {
	now := time.Now().UTC()
	next.LastAdvanceAt = now
	if next.TargetPercent > 0 && !next.Verdict.ValidUntil.After(now) {
		next.Verdict = llmquality.Unknown(now, "scores_expired_during_persistence")
	}
	g.canary = next
}

func (g *Gateway) refreshAIQuality(ctx context.Context) {
	if !g.qualityEnabled() {
		return
	}
	g.mu.Lock()
	version := g.canary.Version
	g.mu.Unlock()
	now := time.Now().UTC()
	bounded, cancel := context.WithTimeout(ctx, 8*time.Second)
	v := g.cfg.AIQuality.Provider.Quality(bounded, g.cfg.AIQuality.Policy, now, time.Time{})
	cancel()
	if ctx.Err() != nil {
		return
	}
	g.controlMu.Lock()
	defer g.controlMu.Unlock()
	g.mu.Lock()
	if ctx.Err() != nil || g.canary.Version != version {
		g.mu.Unlock()
		return
	}
	next := *g.canary
	g.qualityGeneration++
	next.Verdict = v
	if next.Verdict.Status != g.canary.Verdict.Status || next.Verdict.Reason != g.canary.Verdict.Reason {
		g.log.Info("AI quality verdict", "status", v.Status, "reason", v.Reason, "version", version, "batchDigest", v.Batch)
	}
	rolledBack := false
	if v.Status == "regressed" && next.TargetPercent > 0 {
		rolledBack = true
		next.Version++
		next.TargetPercent = 0
		next.LastCommand = ""
		next.LastAdvanceAt = now
		g.log.Warn("AI quality rollback", "version", next.Version, "reason", v.Reason, "batchDigest", v.Batch, "databaseRouteChanged", false)
	}
	// Revocation takes effect immediately even if disk fails. On restart the
	// stored evidence is unknown, so stale durable weights cannot enable target.
	g.canary = &next
	g.mu.Unlock()
	if rolledBack {
		if err := g.trySave(); err != nil {
			g.log.Error("persist AI quality decision", "reason", "persistence_unavailable")
		}
	}
}

func (g *Gateway) runAIQuality(ctx context.Context) {
	if !g.qualityEnabled() {
		return
	}
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		g.refreshAIQuality(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
