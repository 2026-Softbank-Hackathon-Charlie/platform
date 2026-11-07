package gateway

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

const (
	// DefaultAIFollowInterval is how often the AI demo gateway reads the
	// followed app's state.
	DefaultAIFollowInterval = 2 * time.Second
	minAIFollowInterval     = 500 * time.Millisecond
	aiFollowTimeout         = 2 * time.Second
	maxAIFollowBody         = 1 << 20
	// The followed app's gauges on the traffic gateway (registered apps,
	// app label first; see newFamilies). /metrics needs no token.
	followBackendMetric = "charlie_gateway_app_write_backend"
	followHeldMetric    = "charlie_gateway_app_writes_held"
)

// AIFollowConfig makes the AI demo route follow another app's write backend
// on a traffic gateway: the AI gateway reads that app's write_backend and
// writes_held gauges from the unauthenticated GET {URL}/metrics and routes to
// Map[writeBackend]. App must be registered there (PUT /apps/{app}); the
// traffic gateway's default app has no app label and cannot be followed.
type AIFollowConfig struct {
	App string
	URL string // traffic gateway admin base URL; no token is sent
	// Map maps the followed app's backend to an AI backend (gcp or laptop).
	Map      map[string]string
	Interval time.Duration // zero means DefaultAIFollowInterval
}

// ParseAIFollowMap parses "onprem=laptop,gke=gcp,eks=gcp".
func ParseAIFollowMap(s string) (map[string]string, error) {
	out := map[string]string{}
	if strings.TrimSpace(s) == "" {
		return nil, errors.New("empty map; expected appBackend=aiBackend,...")
	}
	for _, part := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if !ok || k == "" || v == "" {
			return nil, fmt.Errorf("entry %q: expected appBackend=aiBackend", part)
		}
		if _, dup := out[k]; dup {
			return nil, fmt.Errorf("app backend %q listed twice", k)
		}
		out[k] = v
	}
	return out, nil
}

func (c *Config) checkAIFollow() error {
	f := c.AIFollow
	if f == nil {
		return nil
	}
	if !appName.MatchString(f.App) {
		return fmt.Errorf("AI follow app %q must match %s", f.App, appName)
	}
	u, err := url.Parse(f.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return errors.New("AI follow URL must be an http(s) URL with a host and no userinfo, query or fragment")
	}
	if len(f.Map) == 0 {
		return errors.New("AI follow map is empty")
	}
	for k, v := range f.Map {
		if strings.TrimSpace(k) == "" {
			return errors.New("AI follow map has an empty app backend")
		}
		if v != "gcp" && v != "laptop" {
			return fmt.Errorf("AI follow map sends %q to %q; AI backends are gcp and laptop", k, v)
		}
	}
	if f.Interval < minAIFollowInterval {
		return fmt.Errorf("AI follow interval must be at least %s", minAIFollowInterval)
	}
	return nil
}

// aiFollower keeps the AI demo route on the AI backend that matches the
// followed app's write backend. A manual route pins the route (enabled
// false) until POST /v1/follow enables following again.
type aiFollower struct {
	rt         *router
	cfg        AIFollowConfig
	metricsURL string
	client     *http.Client

	errors    *prometheus.CounterVec // reason
	switches  *prometheus.CounterVec // fallback
	ticks     <-chan time.Time       // tests inject ticks; nil uses a ticker
	stopped   chan struct{}          // closed when the loop Run started returns
	stopOnce  sync.Once
	syncMu    sync.Mutex // one sync at a time, so an older read never wins
	mu        sync.Mutex // guards the fields below; innermost lock
	enabled   bool
	appBack   string
	target    string
	fallback  bool
	lastSync  time.Time
	lastError string
}

// aiFollowState is the follow object in the AI app's GET /v1/state.
type aiFollowState struct {
	Enabled    bool       `json:"enabled"`
	App        string     `json:"app"`
	AppBackend string     `json:"appBackend"`
	Target     string     `json:"target"`
	Fallback   bool       `json:"fallback"`
	LastSync   *time.Time `json:"lastSync"`
	LastError  string     `json:"lastError"`
}

var errFollowPaused = errors.New("following paused")

func newAIFollower(rt *router, cfg AIFollowConfig) *aiFollower {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	f := &aiFollower{
		rt:  rt,
		cfg: cfg,
		// name[] makes promhttp answer only these two families.
		metricsURL: strings.TrimRight(cfg.URL, "/") + "/metrics?" + url.Values{"name[]": {followBackendMetric, followHeldMetric}}.Encode(),
		client:     &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		stopped:    make(chan struct{}),
		enabled:    true,
		errors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "charlie_gateway_ai_follow_errors_total",
			Help: "AI follow syncs that kept the route because the followed app's state could not be used, by reason (unreachable, status, decode, unmapped, route).",
		}, []string{"reason"}),
		switches: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "charlie_gateway_ai_follow_switches_total",
			Help: "AI route changes made by following the app; fallback=true when the mapped AI backend was unhealthy.",
		}, []string{"fallback"}),
	}
	for _, r := range []string{"unreachable", "status", "decode", "unmapped", "route"} {
		f.errors.WithLabelValues(r)
	}
	f.switches.WithLabelValues("false")
	f.switches.WithLabelValues("true")
	rt.g.prom.reg.MustRegister(f.errors, f.switches, prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "charlie_gateway_ai_follow_fallback",
		Help: "1 while the AI route uses the other AI backend because the mapped one is unhealthy.",
	}, func() float64 {
		f.mu.Lock()
		defer f.mu.Unlock()
		return b2f(f.fallback)
	}))
	return f
}

// run syncs at once, then every interval, until ctx ends or the gateway
// shuts down.
func (f *aiFollower) run(ctx context.Context) {
	ticks := f.ticks
	if ticks == nil {
		t := time.NewTicker(f.cfg.Interval)
		defer t.Stop()
		ticks = t.C
	}
	u, _ := url.Parse(f.cfg.URL)
	f.rt.log.Info("AI follow started", "app", f.cfg.App, "url", u.Redacted(), "interval", f.cfg.Interval.String(), "map", aiFollowMapString(f.cfg.Map))
	for {
		if ctx.Err() != nil || f.rt.g.shuttingDown() {
			return
		}
		f.sync(ctx)
		select {
		case <-ctx.Done():
			return
		case <-f.rt.g.shutdown:
			return
		case <-ticks:
		}
	}
}

// sync reads the followed app once and moves the AI route if needed.
func (f *aiFollower) sync(ctx context.Context) {
	f.syncMu.Lock()
	defer f.syncMu.Unlock()
	if !f.isEnabled() {
		return
	}
	st, reason, err := f.fetch(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return // shutting down or the caller left; not a follow failure
		}
		f.fail(reason, err)
		return
	}
	if st.WritesHeld {
		// A cutover is in progress; its route may still change. Record what
		// was read, but switch only once the app is released.
		f.mu.Lock()
		f.appBack, f.lastSync, f.lastError = st.WriteBackend, time.Now().UTC(), ""
		f.mu.Unlock()
		return
	}
	want, ok := f.cfg.Map[st.WriteBackend]
	if !ok {
		f.fail("unmapped", fmt.Errorf("app backend %q is not in the AI follow map", st.WriteBackend))
		return
	}
	target, fallback, note, keep := want, false, "", false
	if !f.healthy(want) {
		other := "gcp"
		if want == "gcp" {
			other = "laptop"
		}
		if f.healthy(other) {
			target, fallback = other, true
		} else {
			// Report the route actually in effect, not a fallback.
			f.rt.mu.Lock()
			target = f.rt.writeBackend
			f.rt.mu.Unlock()
			keep = true
			note = "AI backends gcp and laptop are both unhealthy; keeping the current route"
			f.rt.log.Warn("AI follow keeping route", "appBackend", st.WriteBackend, "target", want, "err", note)
		}
	}
	if !keep {
		if err := f.apply(target, st.WriteBackend, fallback); err != nil {
			if errors.Is(err, errFollowPaused) {
				return
			}
			f.fail("route", err)
			return
		}
	}
	f.mu.Lock()
	f.appBack, f.target, f.fallback, f.lastSync, f.lastError = st.WriteBackend, target, fallback, time.Now().UTC(), note
	f.mu.Unlock()
}

type followedState struct {
	WriteBackend string
	WritesHeld   bool
}

func (f *aiFollower) fetch(ctx context.Context) (followedState, string, error) {
	ctx, cancel := context.WithTimeout(ctx, aiFollowTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.metricsURL, nil)
	if err != nil {
		return followedState{}, "unreachable", err
	}
	req.Header.Set("Accept", "text/plain; version=0.0.4")
	resp, err := f.client.Do(req)
	if err != nil {
		return followedState{}, "unreachable", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAIFollowBody+1))
	if err != nil {
		return followedState{}, "unreachable", err
	}
	if resp.StatusCode != http.StatusOK {
		return followedState{}, "status", fmt.Errorf("traffic gateway /metrics answered %d", resp.StatusCode)
	}
	if len(body) > maxAIFollowBody {
		return followedState{}, "decode", fmt.Errorf("traffic gateway /metrics is larger than %d bytes", maxAIFollowBody)
	}
	st, err := parseFollowed(body, f.cfg.App)
	if err != nil {
		return followedState{}, "decode", err
	}
	return st, "", nil
}

// parseFollowed reads app's one-hot write_backend gauge and its writes_held
// gauge from Prometheus text. Anything but exactly one backend at 1 is an
// error, so a half-written or unexpected exposition never moves the route.
func parseFollowed(body []byte, app string) (followedState, error) {
	var st followedState
	p := expfmt.NewTextParser(model.LegacyValidation)
	fams, err := p.TextToMetricFamilies(bytes.NewReader(body))
	if err != nil {
		return st, fmt.Errorf("traffic gateway metrics: %w", err)
	}
	series := func(name string) ([]*dto.Metric, error) {
		fam := fams[name]
		if fam == nil || fam.GetType() != dto.MetricType_GAUGE {
			return nil, fmt.Errorf("traffic gateway metrics have no %s gauge", name)
		}
		var out []*dto.Metric
		for _, m := range fam.GetMetric() {
			if metricLabel(m, "app") == app {
				out = append(out, m)
			}
		}
		if len(out) == 0 {
			return nil, fmt.Errorf("app %q has no %s series; is it registered on the traffic gateway?", app, name)
		}
		return out, nil
	}
	backends, err := series(followBackendMetric)
	if err != nil {
		return st, err
	}
	var on []string
	for _, m := range backends {
		switch m.GetGauge().GetValue() {
		case 1:
			on = append(on, metricLabel(m, "backend"))
		case 0:
		default:
			return st, fmt.Errorf("app %q %s is %v; want 0 or 1", app, followBackendMetric, m.GetGauge().GetValue())
		}
	}
	if len(on) != 1 || on[0] == "" {
		return st, fmt.Errorf("app %q has %d write backends at 1 (%s); want exactly one", app, len(on), strings.Join(on, ","))
	}
	held, err := series(followHeldMetric)
	if err != nil {
		return st, err
	}
	if len(held) != 1 {
		return st, fmt.Errorf("app %q has %d %s series; want one", app, len(held), followHeldMetric)
	}
	switch v := held[0].GetGauge().GetValue(); v {
	case 0, 1:
		st.WritesHeld = v == 1
	default:
		return st, fmt.Errorf("app %q %s is %v; want 0 or 1", app, followHeldMetric, v)
	}
	st.WriteBackend = on[0]
	return st, nil
}

func metricLabel(m *dto.Metric, name string) string {
	for _, l := range m.GetLabel() {
		if l.GetName() == name {
			return l.GetValue()
		}
	}
	return ""
}

func (f *aiFollower) healthy(name string) bool {
	f.rt.mu.Lock()
	b := f.rt.byName[name]
	f.rt.mu.Unlock()
	return b != nil && b.healthy.Load()
}

// apply routes to target through handleRoute's checks and locking. It does
// nothing when following was paused after the state was read.
func (f *aiFollower) apply(target, appBackend string, fallback bool) error {
	rt := f.rt
	rt.g.controlMu.Lock()
	defer rt.g.controlMu.Unlock()
	rt.mu.Lock()
	if !f.isEnabled() {
		rt.mu.Unlock()
		return errFollowPaused
	}
	if rt.writeBackend == target {
		rt.mu.Unlock()
		return nil
	}
	prev, _, rerr := rt.routeLocked(target)
	if rerr != nil {
		return rerr
	}
	rt.g.save()
	f.switches.WithLabelValues(fmt.Sprint(fallback)).Inc()
	rt.log.Info("AI route follows app", "from", prev, "to", target, "appBackend", appBackend, "fallback", fallback, "app", f.cfg.App)
	return nil
}

func (f *aiFollower) fail(reason string, err error) {
	f.errors.WithLabelValues(reason).Inc()
	f.mu.Lock()
	f.lastError = err.Error()
	f.mu.Unlock()
	f.rt.log.Warn("AI follow failed; keeping route", "app", f.cfg.App, "reason", reason, "err", err)
}

func (f *aiFollower) isEnabled() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.enabled
}

// setEnabled is called with controlMu held, so it orders with route changes.
func (f *aiFollower) setEnabled(on bool) (prev bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	prev, f.enabled = f.enabled, on
	return prev
}

func (f *aiFollower) view() aiFollowState {
	f.mu.Lock()
	defer f.mu.Unlock()
	v := aiFollowState{Enabled: f.enabled, App: f.cfg.App, AppBackend: f.appBack, Target: f.target, Fallback: f.fallback, LastError: f.lastError}
	if !f.lastSync.IsZero() {
		t := f.lastSync
		v.LastSync = &t
	}
	return v
}

// handleFollow turns following on or off: {"enabled":true|false}. Enabling
// syncs before answering.
func (rt *router) handleFollow(w http.ResponseWriter, r *http.Request) {
	f := rt.follow
	if f == nil {
		writeError(w, http.StatusNotFound, "not_found", "AI follow is not configured for this app")
		return
	}
	var req struct {
		Enabled *bool `json:"enabled"`
	}
	if !rt.g.decodeJSON(w, r, &req) {
		return
	}
	if req.Enabled == nil {
		writeError(w, http.StatusBadRequest, "invalid_request", `"enabled" is required`)
		return
	}
	rt.g.controlMu.Lock()
	prev := f.setEnabled(*req.Enabled)
	rt.g.controlMu.Unlock()
	rt.log.Info("admin action", "action", "follow", "enabled", *req.Enabled, "previous", prev)
	if *req.Enabled {
		f.sync(r.Context())
	}
	writeJSON(w, http.StatusOK, rt.stateView(defaultWindow))
}

// stateView is the TrafficState, plus the follow object on the AI app when
// following is configured.
func (rt *router) stateView(window time.Duration) any {
	st := rt.state(window)
	if rt.follow == nil {
		return st
	}
	return struct {
		apiv1.TrafficState
		Follow aiFollowState `json:"follow"`
	}{st, rt.follow.view()}
}

func (c *Config) setAIFollowDefaults() {
	if c.AIFollow == nil {
		return
	}
	f := *c.AIFollow
	f.Map = maps.Clone(f.Map)
	if f.Interval == 0 {
		f.Interval = DefaultAIFollowInterval
	}
	c.AIFollow = &f
}

// aiFollowMapString is the map for logs, in a stable order.
func aiFollowMapString(m map[string]string) string {
	parts := make([]string, 0, len(m))
	for _, k := range slices.Sorted(maps.Keys(m)) {
		parts = append(parts, k+"="+m[k])
	}
	return strings.Join(parts, ",")
}
