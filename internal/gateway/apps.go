package gateway

import (
	"fmt"
	"maps"
	"net"
	"net/http"
	"slices"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
)

// AppSpec is the body of PUT /apps/{app}.
type AppSpec struct {
	// HealthPath is the app readiness endpoint. Empty inherits the gateway default.
	HealthPath string `json:"healthPath,omitempty"`
	// Backends maps each environment to the app's URL there.
	Backends map[string]string `json:"backends"`
	// WriteBackend receives every write.
	WriteBackend string `json:"writeBackend"`
	// ReadWeights split reads over the backends, summing to 100. Empty sends
	// every read to WriteBackend.
	ReadWeights map[string]int `json:"readWeights,omitempty"`
	// Dial maps a backend to the address ("host:port") the gateway
	// connects to instead of its URL's host, keeping the URL's host as the
	// TLS server name and the Host header (ADR 0042): the legacy backend of
	// an onboarding is https://<app>.<domain> dialled at the VM's public
	// IP, so the gateway does not reach itself once the name points at it.
	// Backends without an entry dial their URL. The proxy and the health
	// checks both use it, and it is part of the saved state and of whether
	// a replace keeps a backend (a changed address is a new backend).
	Dial map[string]string `json:"dial,omitempty"`
}

// AppInfo is one app in GET /apps and the answer to PUT /apps/{app}.
type AppInfo struct {
	HealthPath string `json:"healthPath,omitempty"`
	Name       string `json:"name"`
	// Default marks the app that the gateway's configuration (-backends)
	// serves. It cannot be replaced or deleted through the API.
	Default bool `json:"default"`
	// Host reaches the app: <name>.<app domain>. Empty without an app domain.
	Host         string            `json:"host,omitempty"`
	Backends     map[string]string `json:"backends"` // URLs without passwords
	WriteBackend string            `json:"writeBackend"`
	ReadWeights  map[string]int    `json:"readWeights"`
	WritesHeld   bool              `json:"writesHeld"`
	// Dial is AppSpec.Dial; empty without any.
	Dial map[string]string `json:"dial,omitempty"`
}

// AppList is the answer to GET /apps.
type AppList struct {
	// Domain is the app domain. Empty means host routing is off and every
	// request goes to the default app.
	Domain string `json:"domain"`
	// Apps lists the default app first, then the registered apps by name.
	Apps []AppInfo `json:"apps"`
}

// routerFor picks the app for a request's Host. With an app domain, Host
// <app>.<domain> (any case, port ignored) goes to that app, and nil means no
// app has that name: a typo must not reach another app's backends. Any other
// host, and every host without an app domain, goes to the default app. name
// is the app name the host asked for.
func (g *Gateway) routerFor(host string) (rt *router, name string) {
	if g.cfg.AppDomain == "" {
		return g.router, ""
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	name, ok := strings.CutSuffix(host, "."+g.cfg.AppDomain)
	if !ok {
		return g.router, ""
	}
	return g.lookup(name), name
}

// lookup finds an app by name, the default app included, or nil.
func (g *Gateway) lookup(name string) *router {
	if name == g.router.name {
		return g.router
	}
	g.appsMu.RLock()
	defer g.appsMu.RUnlock()
	return g.apps[name]
}

// routers lists the default app and the registered apps.
func (g *Gateway) routers() []*router {
	g.appsMu.Lock()
	defer g.appsMu.Unlock()
	for name := range g.retired {
		g.reapRetiredLocked(name)
	}
	return append([]*router{g.router}, slices.Collect(maps.Values(g.apps))...)
}

// scoped serves one app's copy of the per-app admin API.
func (g *Gateway) scoped(h func(*router, http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rt := g.lookup(r.PathValue("app"))
		if rt == nil {
			unknownApp(w, r.PathValue("app"))
			return
		}
		h(rt, w, r)
	}
}

func unknownApp(w http.ResponseWriter, name string) {
	writeError(w, http.StatusNotFound, "unknown_app", fmt.Sprintf("no app named %q is registered at this gateway", name))
}

func (g *Gateway) handleListApps(w http.ResponseWriter, _ *http.Request) {
	list := AppList{Domain: g.cfg.AppDomain, Apps: []AppInfo{g.router.info()}}
	g.appsMu.RLock()
	for _, name := range slices.Sorted(maps.Keys(g.apps)) {
		list.Apps = append(list.Apps, g.apps[name].info())
	}
	g.appsMu.RUnlock()
	writeJSON(w, http.StatusOK, list)
}

// handlePutApp registers an app, or replaces a registered app's backends and
// routing. A replace keeps the app's in-flight writes counted, so a hold
// right after it still waits for writes sent before it.
func (g *Gateway) handlePutApp(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("app")
	if !appName.MatchString(name) {
		writeError(w, http.StatusBadRequest, "invalid_app_name", fmt.Sprintf("app name %q must match %s", name, appName))
		return
	}
	if name == g.router.name {
		defaultApp(w, name)
		return
	}
	var spec AppSpec
	if !g.decodeJSON(w, r, &spec) {
		return
	}
	if spec.HealthPath != "" && !validHealthPath(spec.HealthPath) {
		writeError(w, http.StatusBadRequest, "invalid_health_path", "healthPath must be a local HTTP path without fragment")
		return
	}
	healthPath := spec.HealthPath
	if healthPath == "" {
		healthPath = g.cfg.HealthPath
	}
	bcs, err := parseBackendMap(spec.Backends)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_backends", err.Error())
		return
	}
	if bcs, err = withDial(bcs, spec.Dial); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_dial", err.Error())
		return
	}
	names := make([]string, len(bcs))
	for i, bc := range bcs {
		names[i] = bc.Name
	}
	if !slices.Contains(names, spec.WriteBackend) {
		writeError(w, http.StatusBadRequest, "unknown_backend",
			fmt.Sprintf("writeBackend %q is not one of the backends (%s)", spec.WriteBackend, strings.Join(names, ", ")))
		return
	}
	weights := spec.ReadWeights
	if len(weights) == 0 {
		weights = map[string]int{spec.WriteBackend: 100}
	}
	if err := validateWeights(weights, names); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_weights", err.Error())
		return
	}

	// Reading and validating an administrator's body must not hold up another
	// app's Hold/Route/Release. Mutable state is still checked and changed in
	// the same control transaction, including an app held while decoding.
	g.controlMu.Lock()
	defer g.controlMu.Unlock()
	g.appsMu.Lock()
	if g.reapRetiredLocked(name) {
		g.appsMu.Unlock()
		writeError(w, http.StatusConflict, "app_draining", "the deleted app's previous generation still has active or reserved requests")
		return
	}
	rt, exists := g.apps[name]
	if !exists {
		// Cannot fail: the backend names come from a map.
		rt, _ = g.newRouter(name, bcs, g.log.With("app", name), g.prom.apps.view(prometheus.Labels{"app": name}))
	}
	rt.mu.Lock()
	if len(rt.readExclusions) != 0 {
		rt.mu.Unlock()
		g.appsMu.Unlock()
		readsExcluded(w)
		return
	}
	if rt.held {
		rt.mu.Unlock()
		g.appsMu.Unlock()
		writesHeld(w, name)
		return
	}
	if exists {
		rt.backends, rt.byName, _ = rt.newBackends(bcs, rt.byName)
	}
	if rt.healthPath != healthPath {
		rt.healthPath = healthPath
		for _, b := range rt.backends {
			b.healthy.Store(false)
		}
	}
	rt.writeBackend = spec.WriteBackend
	rt.readWeights = rt.fullWeightsLocked(weights)
	rt.mu.Unlock()
	g.apps[name] = rt
	g.appsMu.Unlock()
	g.save()

	info := rt.info()
	rt.log.Info("admin action", "action", "put_app", "created", !exists, "backends", info.Backends,
		"writeBackend", info.WriteBackend, "readWeights", info.ReadWeights)
	status := http.StatusOK
	if !exists {
		status = http.StatusCreated
	}
	writeJSON(w, status, info)
}

// handleDeleteApp removes a registered app. Its host answers 404 from then on;
// requests already being proxied finish.
func (g *Gateway) handleDeleteApp(w http.ResponseWriter, r *http.Request) {
	g.controlMu.Lock()
	defer g.controlMu.Unlock()
	name := r.PathValue("app")
	if name == g.router.name {
		defaultApp(w, name)
		return
	}
	g.appsMu.Lock()
	rt := g.apps[name]
	if rt == nil {
		g.appsMu.Unlock()
		unknownApp(w, name)
		return
	}
	rt.mu.Lock()
	held := rt.held
	excluded := len(rt.readExclusions) != 0
	if !held && !excluded {
		rt.retired = true
		if rt.drainingLocked() {
			g.retired[name] = rt
		}
	}
	rt.mu.Unlock()
	if excluded {
		g.appsMu.Unlock()
		readsExcluded(w)
		return
	}
	if held {
		g.appsMu.Unlock()
		writesHeld(w, name)
		return
	}
	delete(g.apps, name)
	g.appsMu.Unlock()
	g.prom.apps.forget(prometheus.Labels{"app": name})
	g.save()
	rt.log.Info("admin action", "action", "delete_app")
	w.WriteHeader(http.StatusNoContent)
}

func defaultApp(w http.ResponseWriter, name string) {
	writeError(w, http.StatusConflict, "default_app",
		fmt.Sprintf("%q is the default app, configured by the gateway's -backends; it cannot be replaced or deleted here", name))
}

func writesHeld(w http.ResponseWriter, name string) {
	writeError(w, http.StatusConflict, "writes_held",
		fmt.Sprintf("writes of app %q are held for a migration; release them first", name))
}

func (rt *router) info() AppInfo {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	info := AppInfo{
		HealthPath:   rt.healthPath,
		Name:         rt.name,
		Default:      rt == rt.g.router,
		Backends:     make(map[string]string, len(rt.backends)),
		WriteBackend: rt.writeBackend,
		ReadWeights:  maps.Clone(rt.readWeights),
		WritesHeld:   rt.held,
	}
	if d := rt.g.cfg.AppDomain; d != "" {
		info.Host = rt.name + "." + d
	}
	for _, b := range rt.backends {
		info.Backends[b.name] = b.url.Redacted()
		if b.dial != "" {
			if info.Dial == nil {
				info.Dial = map[string]string{}
			}
			info.Dial[b.name] = b.dial
		}
	}
	return info
}
