package gateway

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"math/rand/v2"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Defaults for Config fields left zero.
const (
	DefaultDrainTimeout = 30 * time.Second
	// DefaultMaxHold stays under the Cloudflare origin timeout (about 100s):
	// a held request older than that gets a 524 at the edge anyway.
	DefaultMaxHold = 90 * time.Second
	// DefaultMaxHeld bounds held-write memory: each buffers up to 64 KiB.
	DefaultMaxHeld        = 2000
	DefaultWriteTimeout   = 2 * time.Minute
	DefaultHealthInterval = 2 * time.Second
	DefaultHealthPath     = "/healthz"
	// DefaultAppName names the app that Config.Backends serve
	// (app.hopcharlie.dev).
	DefaultAppName = "app"
)

// BackendConfig is one environment the gateway can send traffic to.
type BackendConfig struct {
	Name string
	URL  *url.URL
}

// Config configures a Gateway.
type Config struct {
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
	WriteTimeout   time.Duration
	HealthInterval time.Duration
	HealthPath     string

	Log *slog.Logger
	// Rand returns a number in [0, n). Tests inject it to make read routing
	// deterministic.
	Rand func(n int) int
	// Transport reaches the backends. Nil uses a clone of the default
	// transport with more idle connections per host.
	Transport http.RoundTripper
}

var (
	backendName = regexp.MustCompile(`^[a-z0-9-]+$`)
	// appName is one DNS label, so <app>.<domain> is a valid host name.
	appName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`)
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

func (c *Config) setDefaults() {
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
	if c.Transport == nil {
		t := http.DefaultTransport.(*http.Transport).Clone()
		// The default of 2 idle connections per host makes a busy gateway
		// open and close connections constantly.
		t.MaxIdleConnsPerHost = 256
		c.Transport = t
	}
}
