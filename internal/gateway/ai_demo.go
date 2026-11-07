package gateway

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// AIDemoConfig enables the bounded native chat contract on the default app.
type AIDemoConfig struct{ App, Token string }

type aiRequestKey struct{}
type aiRequest struct {
	RequestID  string     `json:"requestId"`
	Backend    string     `json:"backend"`
	StartedAt  time.Time  `json:"startedAt"`
	FinishedAt *time.Time `json:"finishedAt,omitempty"`
	Status     int        `json:"status"`
	Outcome    string     `json:"outcome"`
}
type aiState struct {
	active  map[string]int
	records []*aiRequest
}

var aiRequestID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

func (c *Config) checkAI() error {
	if c.AIDemo == nil {
		if c.AIFollow != nil {
			return errors.New("AI follow requires the AI demo (-ai-app)")
		}
		return nil
	}
	if c.AIDemo.App != c.DefaultApp || strings.TrimSpace(c.AIDemo.Token) == "" || strings.TrimSpace(c.AdminToken) == "" {
		return errors.New("AI demo requires the dedicated default app and both tokens")
	}
	if c.WriteTimeout != 12*time.Second {
		return errors.New("AI demo write timeout must be 12s")
	}
	if len(c.Backends) != 2 {
		return errors.New("AI demo requires gcp and laptop")
	}
	names := map[string]bool{}
	for _, b := range c.Backends {
		names[b.Name] = true
	}
	if !names["gcp"] || !names["laptop"] {
		return errors.New("AI demo requires gcp and laptop")
	}
	return c.checkAIFollow()
}

func (rt *router) aiEnabled() bool { return rt.g.cfg.AIDemo != nil && rt.g.cfg.AIDemo.App == rt.name }

// Validate the complete bounded body before any backend side effect. Re-encoding
// gives the upstream exactly the validated fields, including duplicate JSON keys.
func aiBody(w http.ResponseWriter, r *http.Request) ([]byte, int) {
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 32<<10))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return nil, http.StatusRequestEntityTooLarge
		}
		if r.Context().Err() != nil {
			return nil, http.StatusGatewayTimeout
		}
		if deadline, ok := r.Context().Deadline(); ok && !time.Now().Before(deadline) {
			return nil, http.StatusGatewayTimeout
		}
		return nil, http.StatusBadRequest
	}
	var payload struct {
		Model    string `json:"model"`
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
		Stream    *bool  `json:"stream"`
		KeepAlive string `json:"keep_alive"`
		Options   struct {
			Context     *int     `json:"num_ctx"`
			Predict     *int     `json:"num_predict"`
			Temperature *float64 `json:"temperature"`
			Seed        *int     `json:"seed"`
		} `json:"options"`
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if !utf8.Valid(b) || d.Decode(&payload) != nil {
		return nil, http.StatusBadRequest
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return nil, http.StatusBadRequest
	}
	o := payload.Options
	if payload.Model != "qwen2.5:1.5b-instruct-q4_K_M" || payload.Stream == nil || *payload.Stream || payload.KeepAlive != "60s" {
		return nil, http.StatusBadRequest
	}
	if len(payload.Messages) != 2 || payload.Messages[0].Role != "system" || payload.Messages[1].Role != "user" {
		return nil, http.StatusBadRequest
	}
	if o.Context == nil || *o.Context != 2048 || o.Predict == nil || *o.Predict != 128 || o.Temperature == nil || *o.Temperature != 0 || o.Seed == nil || *o.Seed != 42 {
		return nil, http.StatusBadRequest
	}
	n := 0
	for _, m := range payload.Messages {
		n += utf8.RuneCountInString(m.Content)
	}
	if n > 2000 {
		return nil, http.StatusUnprocessableEntity
	}
	b, err = json.Marshal(payload)
	if err != nil {
		return nil, http.StatusBadRequest
	}
	return b, 0
}

func (rt *router) serveAI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || subtle.ConstantTimeCompare([]byte(token), []byte(rt.g.cfg.AIDemo.Token)) != 1 {
		writeError(w, http.StatusUnauthorized, "unauthorized", "AI authentication required")
		return
	}
	if r.Method != http.MethodPost || r.URL.Path != "/api/chat" || r.URL.RawQuery != "" || r.URL.ForceQuery {
		writeError(w, http.StatusNotFound, "not_found", "AI path not available")
		return
	}
	id := r.Header.Get("X-Request-Id")
	if !aiRequestID.MatchString(id) {
		writeError(w, http.StatusBadRequest, "bad_request_id", "request ID required")
		return
	}
	w.Header().Set("X-Request-Id", id)
	ctx, cancel := context.WithTimeoutCause(r.Context(), 12*time.Second, errWriteTimeout)
	defer cancel()
	r = r.WithContext(ctx)
	// Server read deadlines bound slow request bodies as well as inference.
	rc := http.NewResponseController(w)
	deadline, _ := ctx.Deadline()
	_ = rc.SetReadDeadline(deadline)
	body, code := aiBody(w, r)
	if code != 0 {
		// Do not drain an incomplete body after its deadline has expired.
		w.Header().Set("Connection", "close")
		writeError(w, code, "invalid_ai_request", "AI request rejected")
		return
	}
	_ = rc.SetReadDeadline(time.Time{})
	if ctx.Err() != nil {
		writeError(w, http.StatusGatewayTimeout, "backend_timeout", "AI request deadline exceeded")
		return
	}
	// Context cancellation alone cannot interrupt a blocked downstream write/flush.
	_ = rc.SetWriteDeadline(deadline)
	writeCancelled := make(chan struct{})
	stopWriteCancel := context.AfterFunc(ctx, func() {
		defer close(writeCancelled)
		_ = rc.SetWriteDeadline(time.Now())
	})
	defer func() {
		if !stopWriteCancel() {
			<-writeCancelled
		}
		// net/http clears the deadline after its final buffered response flush.
	}()
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	r.GetBody = nil
	rt.mu.Lock()
	if rt.retired || rt.held || rt.g.shuttingDown() {
		rt.mu.Unlock()
		writeError(w, http.StatusServiceUnavailable, "unavailable", "AI routing unavailable")
		return
	}
	b := rt.byName[rt.writeBackend]
	if b == nil || (b.name != "gcp" && b.name != "laptop") {
		rt.mu.Unlock()
		writeError(w, http.StatusServiceUnavailable, "bad_configuration", "AI backend unavailable")
		return
	}
	// Route selection, admission and counting share handleRoute's mutex.
	w.Header().Set("X-Hop-Backend", b.name)
	if rt.ai == nil {
		rt.ai = &aiState{active: map[string]int{"gcp": 0, "laptop": 0}}
	}
	for _, rec := range rt.ai.records {
		if rec.RequestID == id {
			rt.mu.Unlock()
			writeError(w, http.StatusConflict, "duplicate_request_id", "request ID already observed")
			return
		}
	}
	if rt.ai.active[b.name] >= 1 {
		rt.mu.Unlock()
		writeError(w, http.StatusTooManyRequests, "ai_busy", "AI backend already active")
		return
	}
	if len(rt.ai.records) >= 1024 {
		for i, rec := range rt.ai.records {
			if rec.FinishedAt != nil {
				rt.ai.records = append(rt.ai.records[:i], rt.ai.records[i+1:]...)
				break
			}
		}
	}
	rec := &aiRequest{RequestID: id, Backend: b.name, StartedAt: time.Now().UTC(), Outcome: "active"}
	rt.ai.records = append(rt.ai.records, rec)
	rt.ai.active[b.name]++
	rt.inflightWrites++
	rt.mu.Unlock()
	defer rt.writeDone()
	r = r.WithContext(context.WithValue(ctx, aiRequestKey{}, rec))
	rt.forward(w, r, b, classWrite, nil)
}

// Completion is observed only after the response body copy returns or aborts.
func (rt *router) finishAI(r *http.Request, code int, returned bool) {
	rec, ok := r.Context().Value(aiRequestKey{}).(*aiRequest)
	if !ok {
		return
	}
	rt.mu.Lock()
	now := time.Now().UTC()
	rec.FinishedAt, rec.Status = &now, code
	rec.Outcome = "failed"
	if !returned || r.Context().Err() != nil {
		rec.Outcome = "aborted"
	} else if code == http.StatusOK {
		rec.Outcome = "complete"
	}
	rt.ai.active[rec.Backend]--
	snapshot := *rec
	rt.mu.Unlock()
	rt.log.Info("AI request finished", "requestId", snapshot.RequestID, "backend", snapshot.Backend, "status", snapshot.Status, "outcome", snapshot.Outcome, "durationMs", now.Sub(snapshot.StartedAt).Milliseconds())
}

func (rt *router) handleAIRequests(w http.ResponseWriter, r *http.Request) {
	if !rt.aiEnabled() {
		writeError(w, http.StatusNotFound, "not_found", "AI observation disabled")
		return
	}
	rt.mu.Lock()
	state := struct {
		Route    string         `json:"route"`
		Held     bool           `json:"writesHeld"`
		Active   map[string]int `json:"active"`
		Requests []aiRequest    `json:"requests"`
	}{rt.writeBackend, rt.held, map[string]int{"gcp": 0, "laptop": 0}, []aiRequest{}}
	if rt.ai != nil {
		for k, v := range rt.ai.active {
			state.Active[k] = v
		}
		for _, rec := range rt.ai.records {
			state.Requests = append(state.Requests, *rec)
		}
	}
	rt.mu.Unlock()
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, state)
}
