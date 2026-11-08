package gateway

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Defaults for Config fields left zero.
const (
	DefaultDrainTimeout = 30 * time.Second
	// DefaultMaxHold bounds how long one write waits for release. It stays
	// under the idle timeout of the proxy in front, so a write that waits too
	// long gets our 503 hold_timeout with Retry-After, which a client can
	// retry, instead of the proxy's bare 504 (after which the gateway drops
	// the write when the connection closes). An AWS ALB's idle timeout is 60s
	// by default, Cloudflare's origin timeout about 100s. A longer hold needs
	// both this and the proxy's timeout raised.
	DefaultMaxHold = 55 * time.Second
	// DefaultMaxHeld bounds held-write memory: each buffers up to 64 KiB.
	DefaultMaxHeld                    = 2000
	DefaultWriteTimeout               = 2 * time.Minute
	DefaultBodyReadTimeout            = 10 * time.Second
	DefaultAdminBodyReadTimeout       = 10 * time.Second
	DefaultMaxConcurrentWrites        = 64
	DefaultMaxBufferedBytes     int64 = 64 << 20
	DefaultHealthInterval             = 2 * time.Second
	DefaultHealthPath                 = "/healthz"
	// DefaultConnRecycle is how often idle backend connections are closed, so
	// the next request dials, and resolves the backend's name, again.
	DefaultConnRecycle = 30 * time.Second
	// DefaultAppName names the app that Config.Backends serve
	// (app.hopcharlie.dev).
	DefaultAppName = "app"
)

// BackendConfig is one environment the gateway can send traffic to.
type BackendConfig struct {
	Name string
	URL  *url.URL
	// Dial is the "host:port" to connect to instead of URL's host
	// (AppSpec.Dial); empty dials URL.
	Dial string
}

// Config configures a Gateway.
type Config struct {
	AIDemo    *AIDemoConfig    // opt-in native chat contract on the default app
	AIQuality *AIQualityConfig // opt-in paired quality gate for the native AI route
	// AIFollow makes the AI demo route follow another app's write backend on
	// a traffic gateway. Nil (the default) leaves the route to POST /v1/route.
	AIFollow *AIFollowConfig
	// Backends in a stable order; reads are picked in this order.
	Backends []BackendConfig
	// InitialBackend receives writes when there is no state file yet. Empty
	// means the first backend.
	InitialBackend string
	// StateFile persists routing and hold state across restarts. Empty
	// disables persistence (tests).
	StateFile string
	// AdminToken guards the admin API. Empty disables auth.
	AdminToken string
	// AdminBodyReadTimeout bounds JSON body receipt, without bounding the
	// control action's subsequent drain or dispatch wait.
	AdminBodyReadTimeout time.Duration
	// AppDomain turns on host routing: Host <app>.<AppDomain> goes to the app
	// registered as <app> through the admin API, and is answered 404 when
	// there is none. Any other host goes to the default app. Empty sends
	// every request to the default app.
	AppDomain string
	// DefaultApp names the app that Backends serve: <DefaultApp>.<AppDomain>
	// reaches it, and so does /apps/<DefaultApp> on the admin API. Empty
	// means DefaultAppName.
	DefaultApp string

	DrainTimeout time.Duration // how long hold waits for in-flight writes
	// MaxHold is how long one write may wait for release before it gets 503
	// hold_timeout. The hold itself lasts until release.
	MaxHold time.Duration
	MaxHeld int // more waiting writes than this get 503
	// WriteTimeout bounds a forwarded write. Writes are not cancelled when
	// their client leaves, so the backend's work is always waited for.
	WriteTimeout time.Duration
	// BodyReadTimeout bounds the initial held-body read, also capped by MaxHold.
	BodyReadTimeout     time.Duration
	MaxConcurrentWrites int   // per app, including normal writes after release
	MaxBufferedBytes    int64 // global held-body reservation across all apps
	HealthInterval      time.Duration
	HealthPath          string

	Log *slog.Logger
	// Rand returns a number in [0, n). Tests inject it to make read routing
	// deterministic.
	Rand func(n int) int
	// Transport reaches the backends. Nil uses a clone of the default
	// transport with more idle connections per host.
	Transport http.RoundTripper
	// HealthTransport sends the health checks. Nil uses a clone of Transport
	// (when it is an *http.Transport) without keep-alives: every check dials
	// and resolves the backend's name, so a name that stopped resolving or an
	// address that moved shows up within one HealthInterval, even while pooled
	// proxy connections to the old address still work.
	HealthTransport http.RoundTripper
	// ConnRecycle is how often idle proxy connections are closed (Go keeps
	// no DNS cache of its own, but a pooled connection stays on the address it
	// was dialed to). Connections busy at that moment are closed at a later
	// round, once idle. They are also closed whenever a backend's health
	// changes.
	ConnRecycle time.Duration
}

var (
	backendName = regexp.MustCompile(`^[a-z0-9-]+$`)
	// appName is one DNS label, so <app>.<domain> is a valid host name. It
	// is up to 63 characters, for the platform's branch instances
	// <app>--<branch> (ADR 0015).
	appName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	// domainLabel is one label of AppDomain.
	domainLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)
)

// ParseBackends parses "gke=http://a,eks=http://b".
func ParseBackends(s string) ([]BackendConfig, error) {
	var out []BackendConfig
	seen := map[string]bool{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, raw, ok := strings.Cut(part, "=")
		if !ok {
			return nil, fmt.Errorf("backend %q: want name=url", part)
		}
		name = strings.TrimSpace(name)
		if seen[name] {
			return nil, fmt.Errorf("backend %q listed twice", name)
		}
		bc, err := parseBackend(name, strings.TrimSpace(raw))
		if err != nil {
			return nil, err
		}
		seen[name] = true
		out = append(out, bc)
	}
	if len(out) == 0 {
		return nil, errors.New("no backends configured")
	}
	return out, nil
}

// parseBackendMap parses a registered app's backends (name to URL), in name
// order.
func parseBackendMap(m map[string]string) ([]BackendConfig, error) {
	if len(m) == 0 {
		return nil, errors.New("no backends")
	}
	out := make([]BackendConfig, 0, len(m))
	for _, name := range slices.Sorted(maps.Keys(m)) {
		bc, err := parseBackend(name, m[name])
		if err != nil {
			return nil, err
		}
		out = append(out, bc)
	}
	return out, nil
}

// withDial sets the dial address of bcs from dial (AppSpec.Dial): every key
// must name one of bcs and every value be a host:port.
func withDial(bcs []BackendConfig, dial map[string]string) ([]BackendConfig, error) {
	for name, addr := range dial {
		i := slices.IndexFunc(bcs, func(bc BackendConfig) bool { return bc.Name == name })
		if i < 0 {
			return nil, fmt.Errorf("dial names backend %q, which is not one of the backends", name)
		}
		host, port, err := net.SplitHostPort(addr)
		if err != nil || host == "" || port == "" || strings.ContainsAny(addr, "/@?# ") {
			return nil, fmt.Errorf("dial address of backend %s: %q must be host:port", name, addr)
		}
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
			return nil, fmt.Errorf("dial address of backend %s: %q has no valid port", name, addr)
		}
		bcs[i].Dial = addr
	}
	return bcs, nil
}

func parseBackend(name, raw string) (BackendConfig, error) {
	if !backendName.MatchString(name) {
		return BackendConfig{}, fmt.Errorf("backend name %q: must match %s", name, backendName)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return BackendConfig{}, fmt.Errorf("backend %s: %w", name, err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return BackendConfig{}, fmt.Errorf("backend %s: url %q must be http(s)://host", name, raw)
	}
	return BackendConfig{Name: name, URL: u}, nil
}

// checkApps validates DefaultApp and AppDomain (after setDefaults).
func (c *Config) checkApps() error {
	if !validHealthPath(c.HealthPath) {
		return errors.New("health path must be a local HTTP path without fragment")
	}
	if !appName.MatchString(c.DefaultApp) {
		return fmt.Errorf("default app name %q: must match %s", c.DefaultApp, appName)
	}
	if c.AppDomain == "" {
		return nil
	}
	for _, l := range strings.Split(c.AppDomain, ".") {
		if !domainLabel.MatchString(l) {
			return fmt.Errorf("app domain %q: want a host name such as hopcharlie.dev", c.AppDomain)
		}
	}
	return nil
}

func validHealthPath(path string) bool {
	u, err := url.ParseRequestURI(path)
	return err == nil && strings.HasPrefix(path, "/") && !strings.HasPrefix(path, "//") && u.Scheme == "" && u.Host == "" && !strings.Contains(path, "#")
}

func (c *Config) setDefaults() {
	if c.AdminBodyReadTimeout <= 0 {
		c.AdminBodyReadTimeout = DefaultAdminBodyReadTimeout
	}
	if c.MaxConcurrentWrites <= 0 {
		c.MaxConcurrentWrites = DefaultMaxConcurrentWrites
	}
	if c.MaxBufferedBytes <= 0 {
		c.MaxBufferedBytes = DefaultMaxBufferedBytes
	}
	if c.BodyReadTimeout <= 0 {
		c.BodyReadTimeout = DefaultBodyReadTimeout
	}
	if c.DrainTimeout <= 0 {
		c.DrainTimeout = DefaultDrainTimeout
	}
	if c.MaxHold <= 0 {
		c.MaxHold = DefaultMaxHold
	}
	if c.MaxHeld <= 0 {
		c.MaxHeld = DefaultMaxHeld
	}
	if c.WriteTimeout <= 0 {
		c.WriteTimeout = DefaultWriteTimeout
	}
	if c.HealthInterval <= 0 {
		c.HealthInterval = DefaultHealthInterval
	}
	if c.HealthPath == "" {
		c.HealthPath = DefaultHealthPath
	}
	c.setAIFollowDefaults()
	if c.DefaultApp == "" {
		c.DefaultApp = DefaultAppName
	}
	// Host names are case-insensitive, and a trailing dot is the DNS root.
	c.AppDomain = strings.Trim(strings.ToLower(strings.TrimSpace(c.AppDomain)), ".")
	if c.Log == nil {
		c.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if c.Rand == nil {
		c.Rand = rand.IntN
	}
	if c.ConnRecycle <= 0 {
		c.ConnRecycle = DefaultConnRecycle
	}
	if c.Transport == nil {
		t := http.DefaultTransport.(*http.Transport).Clone()
		// The default of 2 idle connections per host makes a busy gateway
		// open and close connections constantly.
		t.MaxIdleConnsPerHost = 256
		t.IdleConnTimeout = c.ConnRecycle
		c.Transport = t
	}
	if c.HealthTransport == nil {
		c.HealthTransport = c.Transport
		if t, ok := c.Transport.(*http.Transport); ok {
			h := t.Clone()
			h.DisableKeepAlives = true
			c.HealthTransport = h
		}
	}
}
