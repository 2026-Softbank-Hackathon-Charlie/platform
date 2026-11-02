// Package agentlog records what the AI agent does: one session per task
// (planning a migration, a sandbox rehearsal, a diagnosis) and one event per
// tool call, tool result, message, blocked action, decision or error.
//
// Events are stored before they are delivered to live subscribers, so a
// client that reconnects with the last seq it saw misses nothing. Values of
// keys that look like credentials are redacted before anything is stored.
package agentlog

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/auth"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

var (
	ErrNotFound = errors.New("agent session not found")
	// ErrFinished means the session already ended; it takes no more events.
	ErrFinished = errors.New("agent session is finished")
	// ErrForbidden means the caller is neither the session's actor nor an operator.
	ErrForbidden = errors.New("only the session's actor or an operator may write to it")
	ErrInvalid   = errors.New("invalid agent request")
	// ErrTooLarge means an event's input or output exceeds MaxPayloadBytes.
	ErrTooLarge = errors.New("agent event payload too large")
)

// Limits.
const (
	// MaxPayloadBytes bounds an event's input and output, each, as stored.
	MaxPayloadBytes = 64 << 10
	// SubscriberBuffer is how many events a slow subscriber may lag before it
	// is dropped; it then reconnects with Last-Event-ID and replays from the store.
	SubscriberBuffer = 256
	maxTitle         = 200
	maxRef           = 200
	maxStatus        = 64
	maxSummary       = 4000
)

// Redacted replaces the value of every key that matches SecretKey.
const Redacted = "[redacted]"

// SecretKey matches keys whose values are stored as Redacted.
var SecretKey = regexp.MustCompile(`(?i)password|secret|token|authorization|api[_-]?key`)

// EventInput is one event as the agent sends it. Input and Output are any
// JSON value; absent or null means none.
type EventInput struct {
	Type           apiv1.AgentEventType `json:"type"`
	Tool           *string              `json:"tool,omitempty"`
	Input          json.RawMessage      `json:"input,omitempty"`
	Output         json.RawMessage      `json:"output,omitempty"`
	Status         *string              `json:"status,omitempty"`
	DurationMillis *int64               `json:"durationMillis,omitempty"`
	Usage          *apiv1.TokenUsage    `json:"usage,omitempty"`
}

// Service is the agent activity log. It is safe for concurrent use.
//
// Appends and finishes go through one mutex: within this process that keeps
// the store write and the delivery to subscribers in seq order, and a finish
// can never overtake an append. Agent events arrive a few per second, so one
// lock for all sessions costs nothing that matters.
type Service struct {
	st    store.AgentStore
	now   func() time.Time
	newID func() string

	mu   sync.Mutex
	subs map[string]map[chan apiv1.AgentEvent]struct{} // by session id
}

// New returns a Service backed by st.
func New(st store.AgentStore) *Service {
	return &Service{
		st:    st,
		now:   func() time.Time { return time.Now().UTC() },
		newID: func() string { b := make([]byte, 6); _, _ = rand.Read(b); return "ags_" + hex.EncodeToString(b) },
		subs:  map[string]map[chan apiv1.AgentEvent]struct{}{},
	}
}

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
}

func checkLen(field, v string, n int) error {
	if utf8.RuneCountInString(v) > n {
		return invalid("%s is longer than %d characters", field, n)
	}
	return nil
}

// Create starts a running session owned by actor.
func (s *Service) Create(ctx context.Context, actor string, req apiv1.CreateAgentSessionRequest) (apiv1.AgentSession, error) {
	title := strings.TrimSpace(req.Title)
	switch {
	case !req.Kind.Valid():
		return apiv1.AgentSession{}, invalid("kind must be plan, rehearsal, diagnose, explain or other")
	case title == "":
		return apiv1.AgentSession{}, invalid("title is required")
	}
	a := store.AgentSession{
		ID: s.newID(), Kind: string(req.Kind), Title: title, Actor: actor,
		PlanRef: deref(req.PlanRef), RunID: deref(req.RunId), Model: deref(req.Model),
		Status: store.AgentRunning, CreatedAt: s.now(),
	}
	for _, f := range []struct {
		name, v string
		n       int
	}{{"title", a.Title, maxTitle}, {"planRef", a.PlanRef, maxRef}, {"runId", a.RunID, maxRef}, {"model", a.Model, maxRef}} {
		if err := checkLen(f.name, f.v, f.n); err != nil {
			return apiv1.AgentSession{}, err
		}
	}
	if err := s.st.CreateAgentSession(ctx, a); err != nil {
		return apiv1.AgentSession{}, err
	}
	return apiSession(a), nil
}

// Get returns a session with its totals.
func (s *Service) Get(ctx context.Context, id string) (apiv1.AgentSession, error) {
	a, err := s.st.GetAgentSession(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return apiv1.AgentSession{}, ErrNotFound
	}
	if err != nil {
		return apiv1.AgentSession{}, err
	}
	return apiSession(a), nil
}

// List returns sessions newest first, filtered by plan and run when set.
func (s *Service) List(ctx context.Context, planRef, runID string, limit int) ([]apiv1.AgentSession, error) {
	rows, err := s.st.ListAgentSessions(ctx, store.AgentSessionFilter{PlanRef: planRef, RunID: runID, Limit: limit})
	if err != nil {
		return nil, err
	}
	out := make([]apiv1.AgentSession, len(rows))
	for i, a := range rows {
		out[i] = apiSession(a)
	}
	return out, nil
}

// Events returns a session's stored events with seq > after.
func (s *Service) Events(ctx context.Context, id string, after int64) ([]apiv1.AgentEvent, error) {
	if _, err := s.Get(ctx, id); err != nil {
		return nil, err
	}
	rows, err := s.st.ListAgentEvents(ctx, id, after, 0)
	if err != nil {
		return nil, err
	}
	out := make([]apiv1.AgentEvent, len(rows))
	for i, e := range rows {
		out[i] = apiEvent(e)
	}
	return out, nil
}

// allowed reports whether p may write to a.
func allowed(p auth.Principal, a store.AgentSession) bool {
	return p.Name == a.Actor || p.Allows(apiv1.RoleOperator)
}

// Append stores one event of a running session and delivers it to the
// session's subscribers.
func (s *Service) Append(ctx context.Context, p auth.Principal, id string, in EventInput) (apiv1.AgentEvent, error) {
	e := store.AgentEvent{SessionID: id, Type: string(in.Type), Tool: deref(in.Tool), Status: deref(in.Status), DurationMillis: in.DurationMillis}
	switch {
	case !in.Type.Valid():
		return apiv1.AgentEvent{}, invalid("type must be tool_call, tool_result, message, blocked, decision or error")
	case in.DurationMillis != nil && *in.DurationMillis < 0:
		return apiv1.AgentEvent{}, invalid("durationMillis must not be negative")
	case in.Usage != nil && (in.Usage.InputTokens < 0 || in.Usage.OutputTokens < 0):
		return apiv1.AgentEvent{}, invalid("usage must not be negative")
	}
	if err := checkLen("tool", e.Tool, maxTitle); err != nil {
		return apiv1.AgentEvent{}, err
	}
	if err := checkLen("status", e.Status, maxStatus); err != nil {
		return apiv1.AgentEvent{}, err
	}
	if in.Usage != nil {
		e.InputTokens, e.OutputTokens = in.Usage.InputTokens, in.Usage.OutputTokens
	}
	var err error
	if e.Input, err = payload("input", in.Input); err != nil {
		return apiv1.AgentEvent{}, err
	}
	if e.Output, err = payload("output", in.Output); err != nil {
		return apiv1.AgentEvent{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.authorize(ctx, p, id); err != nil {
		return apiv1.AgentEvent{}, err
	}
	e.Time = s.now()
	stored, err := s.st.AppendAgentEvent(ctx, e)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return apiv1.AgentEvent{}, ErrNotFound
	case errors.Is(err, store.ErrConflict):
		return apiv1.AgentEvent{}, ErrFinished
	case err != nil:
		return apiv1.AgentEvent{}, err
	}
	ev := apiEvent(stored)
	for ch := range s.subs[id] {
		select {
		case ch <- ev:
		default: // too slow: drop; the client reconnects with Last-Event-ID
			s.drop(id, ch)
		}
	}
	return ev, nil
}

// Finish ends a running session and closes its subscribers, which then send
// their final snapshot.
func (s *Service) Finish(ctx context.Context, p auth.Principal, id string, req apiv1.FinishAgentSessionRequest) (apiv1.AgentSession, error) {
	if req.Status == apiv1.AgentSessionStatusRunning || !req.Status.Valid() {
		return apiv1.AgentSession{}, invalid("status must be succeeded, failed or cancelled")
	}
	summary := strings.TrimSpace(deref(req.Summary))
	if err := checkLen("summary", summary, maxSummary); err != nil {
		return apiv1.AgentSession{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.authorize(ctx, p, id); err != nil {
		return apiv1.AgentSession{}, err
	}
	a, err := s.st.FinishAgentSession(ctx, id, string(req.Status), summary, s.now())
	switch {
	case errors.Is(err, store.ErrNotFound):
		return apiv1.AgentSession{}, ErrNotFound
	case errors.Is(err, store.ErrConflict):
		return apiv1.AgentSession{}, ErrFinished
	case err != nil:
		return apiv1.AgentSession{}, err
	}
	for ch := range s.subs[id] {
		s.drop(id, ch)
	}
	return apiSession(a), nil
}

// authorize checks that the session exists and that p may write to it.
// Call it with s.mu held.
func (s *Service) authorize(ctx context.Context, p auth.Principal, id string) error {
	a, err := s.st.GetAgentSession(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if !allowed(p, a) {
		return ErrForbidden
	}
	return nil
}

// Subscribe returns the live events of a running session. The channel closes
// when the session finishes or the subscriber falls behind; for a finished
// session it is closed already. Subscribe before reading stored events so
// nothing falls between the two, and drop duplicates by seq.
func (s *Service) Subscribe(ctx context.Context, id string) (<-chan apiv1.AgentEvent, func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, err := s.st.GetAgentSession(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil, ErrNotFound
	}
	if err != nil {
		return nil, nil, err
	}
	ch := make(chan apiv1.AgentEvent, SubscriberBuffer)
	if a.Status != store.AgentRunning {
		close(ch)
		return ch, func() {}, nil
	}
	if s.subs[id] == nil {
		s.subs[id] = map[chan apiv1.AgentEvent]struct{}{}
	}
	s.subs[id][ch] = struct{}{}
	cancel := func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.drop(id, ch)
	}
	return ch, cancel, nil
}

// drop closes and forgets one subscriber, once. Call it with s.mu held.
func (s *Service) drop(id string, ch chan apiv1.AgentEvent) {
	if _, ok := s.subs[id][ch]; !ok {
		return
	}
	delete(s.subs[id], ch)
	close(ch)
	if len(s.subs[id]) == 0 {
		delete(s.subs, id)
	}
}

// payload redacts a JSON value and checks its stored size. Numbers keep
// their exact digits.
func payload(field string, raw json.RawMessage) ([]byte, error) {
	if len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, invalid("%s is not valid JSON", field)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(Redact(v)); err != nil {
		return nil, invalid("%s: %v", field, err)
	}
	b := bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
	if len(b) > MaxPayloadBytes {
		return nil, fmt.Errorf("%w: %s is %d bytes; the limit is %d", ErrTooLarge, field, len(b), MaxPayloadBytes)
	}
	return b, nil
}

// Redact returns v with the value of every object key that matches SecretKey
// replaced by Redacted, at any depth. It does not modify v.
func Redact(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			if SecretKey.MatchString(k) {
				out[k] = Redacted
			} else {
				out[k] = Redact(val)
			}
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = Redact(val)
		}
		return out
	}
	return v
}

// --- conversions ---

func apiSession(a store.AgentSession) apiv1.AgentSession {
	return apiv1.AgentSession{
		Id: a.ID, Kind: apiv1.AgentSessionKind(a.Kind), Title: a.Title, Actor: a.Actor,
		Status: apiv1.AgentSessionStatus(a.Status), CreatedAt: a.CreatedAt, EndedAt: a.EndedAt,
		PlanRef: optional(a.PlanRef), RunId: optional(a.RunID), Model: optional(a.Model), Summary: optional(a.Summary),
		Totals: apiv1.AgentSessionTotals{
			Events: a.Totals.Events, ToolCalls: a.Totals.ToolCalls, InputTokens: a.Totals.InputTokens,
			OutputTokens: a.Totals.OutputTokens, DurationMillis: a.Totals.DurationMillis,
		},
	}
}

func apiEvent(e store.AgentEvent) apiv1.AgentEvent {
	out := apiv1.AgentEvent{
		Seq: e.Seq, SessionId: e.SessionID, Time: e.Time, Type: apiv1.AgentEventType(e.Type),
		Tool: optional(e.Tool), Status: optional(e.Status), DurationMillis: e.DurationMillis,
	}
	if len(e.Input) > 0 {
		out.Input = json.RawMessage(e.Input)
	}
	if len(e.Output) > 0 {
		out.Output = json.RawMessage(e.Output)
	}
	if e.InputTokens > 0 || e.OutputTokens > 0 {
		out.Usage = &apiv1.TokenUsage{InputTokens: e.InputTokens, OutputTokens: e.OutputTokens}
	}
	return out
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
