package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	enginegw "github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/gateway"
)

const testDomain = "hopcharlie.dev"

// appBackends starts one fake backend per environment of an app. Each answers
// reads with "<app>-<env>". The spec sends writes to the first environment.
func appBackends(t *testing.T, app string, envs ...string) (AppSpec, map[string]*fakeBackend) {
	t.Helper()
	spec := AppSpec{Backends: map[string]string{}, WriteBackend: envs[0]}
	fbs := map[string]*fakeBackend{}
	for _, env := range envs {
		fb := newFakeBackend(t, app+"-"+env)
		fbs[env] = fb
		spec.Backends[env] = fb.srv.URL
	}
	return spec, fbs
}

// hostDo sends a request through the proxy with the given Host header.
func (tg *testGW) hostDo(ctx context.Context, method, host, path, body string) writeResult {
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, tg.proxy.URL+path, rd)
	if err != nil {
		return writeResult{err: err}
	}
	req.Host = host
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return writeResult{err: err}
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return writeResult{status: resp.StatusCode, backend: resp.Header.Get(headerBackend),
		retryAfter: resp.Header.Get("Retry-After"), body: string(b)}
}

func (tg *testGW) asyncHostDo(ctx context.Context, method, host, path, body string) <-chan writeResult {
	ch := make(chan writeResult, 1)
	go func() { ch <- tg.hostDo(ctx, method, host, path, body) }()
	return ch
}

// appHeldWrites counts writes waiting on an app's hold.
func (tg *testGW) appHeldWrites(name string) int {
	rt := tg.g.lookup(name)
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.waitingLocked()
}

func wantAPIError(t *testing.T, err error, status int, code string) {
	t.Helper()
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != status || ae.Code != code {
		t.Fatalf("got %v, want %d %s", err, status, code)
	}
}

// adminDo sends a raw admin request and returns the status and error code.
func (tg *testGW) adminDo(t *testing.T, method, path, auth, body string) (int, string) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, tg.admin.URL+path, rd)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var e struct {
		Error struct{ Code string } `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&e)
	return resp.StatusCode, e.Error.Code
}

// TestHostRouting: <app>.<domain> reaches that app in any case and with any
// port; a name under the domain that no app has is 404, never another app;
// every other host is the default app, as before host routing.
func TestHostRouting(t *testing.T) {
	tg := newTestGW(t, Config{AppDomain: " HopCharlie.DEV. "}, "gke", "eks")
	ctx := context.Background()
	spec, shop := appBackends(t, "shop", "gke", "eks")
	spec.WriteBackend = "eks"
	if err := tg.client.PutApp(ctx, "shop", spec); err != nil {
		t.Fatal(err)
	}

	for host, want := range map[string]string{
		"shop.hopcharlie.dev":              "shop-eks",
		"SHOP.HopCharlie.Dev:8443":         "shop-eks",
		"shop.hopcharlie.dev.":             "shop-eks",
		"app.hopcharlie.dev":               "gke", // the default app by its name
		"APP.hopcharlie.dev:443":           "gke",
		"hopcharlie.dev":                   "gke", // outside <app>.<domain>
		"example.com":                      "gke",
		"127.0.0.1:8000":                   "gke",
		"[::1]:8000":                       "gke",
		"":                                 "gke",
		"shophopcharlie.dev":               "gke",
		"shop.hopcharlie.dev.example.com":  "gke",
		"shpo.hopcharlie.dev":              "404",
		"a.shop.hopcharlie.dev":            "404",
		".hopcharlie.dev":                  "404",
		"Shop_1.hopcharlie.dev":            "404",
		"x.y.z.hopcharlie.dev:80":          "404",
		"verylongnamethatnoappcouldhave-x": "gke",
	} {
		req := httptest.NewRequest(http.MethodGet, "/items", nil)
		req.Host = host
		rec := httptest.NewRecorder()
		tg.g.Proxy().ServeHTTP(rec, req)
		if want == "404" {
			if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), `"code":"unknown_app"`) {
				t.Errorf("host %q: %d %q, want 404 unknown_app", host, rec.Code, rec.Body.String())
			}
			continue
		}
		if rec.Code != http.StatusOK || rec.Body.String() != want {
			t.Errorf("host %q: %d %q, want %q", host, rec.Code, rec.Body.String(), want)
		}
	}

	// Writes follow the same rule and reach only the app's write backend.
	if r := tg.hostDo(ctx, http.MethodPost, "shop.hopcharlie.dev", "/orders", "to-shop"); r.status != http.StatusOK || r.backend != "eks" {
		t.Fatalf("write to shop: %+v", r)
	}
	if r := tg.hostDo(ctx, http.MethodPost, "app.hopcharlie.dev", "/orders", "to-default"); r.status != http.StatusOK || r.backend != "gke" {
		t.Fatalf("write to default: %+v", r)
	}
	if r := tg.hostDo(ctx, http.MethodPost, "nope.hopcharlie.dev", "/orders", "to-nobody"); r.status != http.StatusNotFound {
		t.Fatalf("write to unknown app: %+v", r)
	}
	if shop["eks"].got("to-shop") != 1 || shop["gke"].got("to-shop") != 0 || tg.backends["gke"].got("to-shop") != 0 {
		t.Fatal("shop's write went elsewhere")
	}
	if tg.backends["gke"].got("to-default") != 1 || shop["eks"].got("to-default") != 0 {
		t.Fatal("default write went elsewhere")
	}
	for _, fb := range []*fakeBackend{tg.backends["gke"], tg.backends["eks"], shop["gke"], shop["eks"]} {
		if fb.got("to-nobody") != 0 {
			t.Fatalf("write for an unknown app reached %s", fb.name)
		}
	}

	// Without an app domain, registered apps exist but every host is the
	// default app.
	plain := newTestGW(t, Config{}, "gke")
	if err := plain.client.PutApp(ctx, "shop", spec); err != nil {
		t.Fatal(err)
	}
	if r := plain.hostDo(ctx, http.MethodGet, "shop.hopcharlie.dev", "/items", ""); r.status != http.StatusOK || r.body != "gke" {
		t.Fatalf("no app domain: %+v", r)
	}

	if _, err := New(Config{Backends: tg.g.cfg.Backends, AppDomain: "hop charlie.dev"}); err == nil {
		t.Error("accepted an invalid app domain")
	}
	if _, err := New(Config{Backends: tg.g.cfg.Backends, DefaultApp: "App"}); err == nil {
		t.Error("accepted an invalid default app name")
	}
}

// TestAppIsolation: each app has its own hold, routing and stats. Holding one
// app's writes neither waits for nor delays another app's writes.
func TestAppIsolation(t *testing.T) {
	tg := newTestGW(t, Config{AppDomain: testDomain, MaxHold: time.Hour}, "gke", "eks")
	ctx := context.Background()
	spec, shop := appBackends(t, "shop", "gke", "eks")
	if err := tg.client.PutApp(ctx, "shop", spec); err != nil {
		t.Fatal(err)
	}
	shopc := tg.client.App("shop")
	const shopHost, defHost = "shop.hopcharlie.dev", "app.hopcharlie.dev"

	// A default-app write stuck at its backend does not delay shop's hold.
	unblock := tg.backends["gke"].blockWrites()
	defer unblock()
	stuck := tg.asyncHostDo(ctx, http.MethodPost, defHost, "/orders", "stuck")
	waitFor(t, "default write at its backend", func() bool { return tg.backends["gke"].activeWrites.Load() == 1 })
	start := time.Now()
	if err := shopc.HoldWrites(ctx); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("shop's hold waited %s for another app's write", d)
	}
	unblock()
	if r := recv(t, stuck); r.status != http.StatusOK {
		t.Fatalf("default write: %+v", r)
	}

	// Shop's writes wait; the default app's writes and shop's reads flow.
	held := tg.asyncHostDo(ctx, http.MethodPost, shopHost, "/orders", "shop-held")
	waitFor(t, "shop write held", func() bool { return tg.appHeldWrites("shop") == 1 })
	if r := tg.hostDo(ctx, http.MethodPost, defHost, "/orders", "default-during-hold"); r.status != http.StatusOK || r.backend != "gke" {
		t.Fatalf("default write while shop is held: %+v", r)
	}
	if r := tg.hostDo(ctx, http.MethodGet, shopHost, "/items", ""); r.status != http.StatusOK || r.body != "shop-gke" {
		t.Fatalf("shop read while held: %+v", r)
	}
	if tg.g.state(defaultWindow).WritesHeld {
		t.Fatal("default app reports shop's hold")
	}

	// Cutover of shop alone: route, release. The default app keeps gke.
	if err := shopc.Route(ctx, "eks"); err != nil {
		t.Fatal(err)
	}
	if err := shopc.ReleaseWrites(ctx); err != nil {
		t.Fatal(err)
	}
	if r := recv(t, held); r.status != http.StatusOK || r.backend != "eks" {
		t.Fatalf("shop held write: %+v", r)
	}
	if shop["eks"].got("shop-held") != 1 || shop["gke"].got("shop-held") != 0 {
		t.Fatal("held write must reach only shop's new backend")
	}
	def := tg.g.state(defaultWindow)
	if def.WriteBackend != "gke" || def.ReadWeights["gke"] != 100 {
		t.Fatalf("default app moved with shop: %+v", def)
	}

	// The default app's hold does not hold shop.
	if err := tg.client.HoldWrites(ctx); err != nil {
		t.Fatal(err)
	}
	if r := tg.hostDo(ctx, http.MethodPost, shopHost, "/orders", "shop-during-default-hold"); r.status != http.StatusOK || r.backend != "eks" {
		t.Fatalf("shop write while the default app is held: %+v", r)
	}
	if err := tg.client.ReleaseWrites(ctx); err != nil {
		t.Fatal(err)
	}

	// Stats are per app: the default app counts only its own requests.
	st, err := shopc.State(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if st.WriteBackend != "eks" || st.Totals["eks"].Requests != 2 || st.Totals["gke"].Requests != 1 || len(st.Backends) != 2 {
		t.Fatalf("shop state: %+v", st)
	}
	if def := tg.g.state(defaultWindow); def.Totals["gke"].Requests != 2 || def.Totals["eks"].Requests != 0 {
		t.Fatalf("default totals: %+v", def.Totals)
	}
}

// TestAppAdmin: create, replace, list and delete apps, the checks on them,
// and the token on every new endpoint.
func TestAppAdmin(t *testing.T) {
	tg := newTestGW(t, Config{AppDomain: testDomain}, "gke", "eks")
	ctx := context.Background()
	c := tg.client

	list, err := c.Apps(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if list.Domain != testDomain || len(list.Apps) != 1 {
		t.Fatalf("initial list: %+v", list)
	}
	if a := list.Apps[0]; a.Name != DefaultAppName || !a.Default || a.Host != "app.hopcharlie.dev" || a.WriteBackend != "gke" || len(a.Backends) != 2 {
		t.Fatalf("default app: %+v", a)
	}

	spec, _ := appBackends(t, "shop", "gke", "eks")
	for _, name := range []string{"Shop", "-shop", "shop-", "sh_op", "s.p", strings.Repeat("a", 64)} {
		wantAPIError(t, c.PutApp(ctx, name, spec), http.StatusBadRequest, "invalid_app_name")
	}
	// A name is one DNS label of up to 63 characters, so the platform's
	// branch instances such as moimroom--dev are names too (ADR 0015).
	for _, name := range []string{"moimroom--dev", strings.Repeat("a", 63)} {
		if err := c.PutApp(ctx, name, spec); err != nil {
			t.Fatalf("put %s: %v", name, err)
		}
		if r := tg.hostDo(ctx, http.MethodGet, name+"."+testDomain, "/items", ""); r.status != http.StatusOK {
			t.Fatalf("%s.%s: %+v", name, testDomain, r)
		}
		if err := c.DeleteApp(ctx, name); err != nil {
			t.Fatalf("delete %s: %v", name, err)
		}
	}
	wantAPIError(t, c.PutApp(ctx, DefaultAppName, spec), http.StatusConflict, "default_app")
	wantAPIError(t, c.DeleteApp(ctx, DefaultAppName), http.StatusConflict, "default_app")
	wantAPIError(t, c.DeleteApp(ctx, "shop"), http.StatusNotFound, "unknown_app")
	for _, bad := range []struct {
		spec AppSpec
		code string
	}{
		{AppSpec{WriteBackend: "gke"}, "invalid_backends"},
		{AppSpec{Backends: map[string]string{"gke": "ftp://x"}, WriteBackend: "gke"}, "invalid_backends"},
		{AppSpec{Backends: map[string]string{"GKE": "http://x"}, WriteBackend: "GKE"}, "invalid_backends"},
		{AppSpec{Backends: spec.Backends}, "unknown_backend"},
		{AppSpec{Backends: spec.Backends, WriteBackend: "aks"}, "unknown_backend"},
		{AppSpec{Backends: spec.Backends, WriteBackend: "gke", ReadWeights: map[string]int{"gke": 90}}, "invalid_weights"},
		{AppSpec{Backends: spec.Backends, WriteBackend: "gke", ReadWeights: map[string]int{"gke": 50, "aks": 50}}, "invalid_weights"},
	} {
		wantAPIError(t, c.PutApp(ctx, "shop", bad.spec), http.StatusBadRequest, bad.code)
	}
	auth := "Bearer " + testToken
	if status, code := tg.adminDo(t, http.MethodPut, "/apps/shop", auth, `{"backends":{"gke":"http://x"},"writeBackend":"gke","extra":1}`); status != http.StatusBadRequest || code != "invalid_request" {
		t.Fatalf("unknown field: %d %s", status, code)
	}
	if list, _ := c.Apps(ctx); len(list.Apps) != 1 {
		t.Fatalf("a refused PUT registered an app: %+v", list)
	}

	// Create answers 201, replace 200.
	body, _ := json.Marshal(AppSpec{Backends: spec.Backends, WriteBackend: "gke", ReadWeights: map[string]int{"gke": 80, "eks": 20}})
	if status, _ := tg.adminDo(t, http.MethodPut, "/apps/shop", auth, string(body)); status != http.StatusCreated {
		t.Fatalf("create: %d", status)
	}
	if status, _ := tg.adminDo(t, http.MethodPut, "/apps/shop", auth, string(body)); status != http.StatusOK {
		t.Fatalf("replace: %d", status)
	}
	st, err := c.App("shop").State(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if st.WriteBackend != "gke" || st.ReadWeights["gke"] != 80 || st.ReadWeights["eks"] != 20 {
		t.Fatalf("shop after PUT: %+v", st)
	}

	// A replace changes the backends; one whose URL is unchanged keeps its
	// stats. Passwords in URLs are not shown.
	if r := tg.hostDo(ctx, http.MethodPost, "shop.hopcharlie.dev", "/orders", "w"); r.status != http.StatusOK {
		t.Fatalf("write: %+v", r)
	}
	aks := newFakeBackend(t, "shop-aks")
	aksURL := strings.Replace(aks.srv.URL, "http://", "http://user:secret@", 1)
	replaced := AppSpec{Backends: map[string]string{"gke": spec.Backends["gke"], "aks": aksURL}, WriteBackend: "aks"}
	if err := c.PutApp(ctx, "shop", replaced); err != nil {
		t.Fatal(err)
	}
	st, _ = c.App("shop").State(ctx, 0)
	if st.WriteBackend != "aks" || st.ReadWeights["aks"] != 100 || len(st.ReadWeights) != 2 || st.Totals["gke"].Requests != 1 {
		t.Fatalf("shop after replace: %+v", st)
	}
	list, _ = c.Apps(ctx)
	if len(list.Apps) != 2 || list.Apps[1].Name != "shop" || list.Apps[1].Default || list.Apps[1].Host != "shop.hopcharlie.dev" {
		t.Fatalf("list: %+v", list)
	}
	if u := list.Apps[1].Backends["aks"]; strings.Contains(u, "secret") || !strings.Contains(u, "user") {
		t.Fatalf("listed URL %q", u)
	}
	if r := tg.hostDo(ctx, http.MethodPost, "shop.hopcharlie.dev", "/orders", "to-aks"); r.status != http.StatusOK || aks.got("to-aks") != 1 {
		t.Fatalf("write after replace: %+v", r)
	}

	// No replace or delete while the app's writes are held.
	if err := c.App("shop").HoldWrites(ctx); err != nil {
		t.Fatal(err)
	}
	wantAPIError(t, c.PutApp(ctx, "shop", replaced), http.StatusConflict, "writes_held")
	wantAPIError(t, c.DeleteApp(ctx, "shop"), http.StatusConflict, "writes_held")
	if err := c.App("shop").ReleaseWrites(ctx); err != nil {
		t.Fatal(err)
	}
	if status, _ := tg.adminDo(t, http.MethodDelete, "/apps/shop", auth, ""); status != http.StatusNoContent {
		t.Fatalf("delete: %d", status)
	}
	wantAPIError(t, c.DeleteApp(ctx, "shop"), http.StatusNotFound, "unknown_app")
	_, err = c.App("shop").State(ctx, 0)
	wantAPIError(t, err, http.StatusNotFound, "unknown_app")
	wantAPIError(t, c.App("shop").Health(ctx), http.StatusNotFound, "unknown_app")
	if r := tg.hostDo(ctx, http.MethodGet, "shop.hopcharlie.dev", "/items", ""); r.status != http.StatusNotFound {
		t.Fatalf("deleted app still served: %+v", r)
	}

	// The default app answers under its name too.
	if err := c.App(DefaultAppName).Route(ctx, "eks"); err != nil {
		t.Fatal(err)
	}
	if st := tg.g.state(defaultWindow); st.WriteBackend != "eks" {
		t.Fatalf("route through /apps/app: %+v", st)
	}
	if err := c.App(DefaultAppName).Health(ctx); err != nil {
		t.Fatal(err)
	}

	// Every new endpoint needs the token.
	for _, e := range []struct{ method, path string }{
		{http.MethodGet, "/apps"},
		{http.MethodPut, "/apps/x"},
		{http.MethodDelete, "/apps/x"},
		{http.MethodGet, "/apps/app/healthz"},
		{http.MethodGet, "/apps/app/v1/state"},
		{http.MethodPost, "/apps/app/v1/writes/hold"},
		{http.MethodPost, "/apps/app/v1/writes/release"},
		{http.MethodPost, "/apps/app/v1/route"},
		{http.MethodPost, "/apps/app/v1/weights"},
	} {
		for _, a := range []string{"", "Bearer wrong"} {
			if status, code := tg.adminDo(t, e.method, e.path, a, ""); status != http.StatusUnauthorized || code != "unauthorized" {
				t.Errorf("%s %s with %q: %d %s", e.method, e.path, a, status, code)
			}
		}
	}
	if tg.g.state(defaultWindow).WritesHeld {
		t.Fatal("an unauthorized call held writes")
	}
}

// TestAppPersistence: registered apps, their routing and holds survive a
// restart. The file keeps the default app in the single-app format, which
// both older gateways and this one read.
func TestAppPersistence(t *testing.T) {
	file := filepath.Join(t.TempDir(), "gateway.json")
	ctx := context.Background()
	cfg := Config{StateFile: file, AppDomain: testDomain, MaxHold: time.Hour}
	tg := newTestGW(t, cfg, "gke", "eks")
	spec, shop := appBackends(t, "shop", "gke", "eks")
	spec.ReadWeights = map[string]int{"gke": 70, "eks": 30}
	if err := tg.client.PutApp(ctx, "shop", spec); err != nil {
		t.Fatal(err)
	}
	blogSpec, blog := appBackends(t, "blog", "onprem")
	if err := tg.client.PutApp(ctx, "blog", blogSpec); err != nil {
		t.Fatal(err)
	}
	if err := tg.client.App("shop").HoldWrites(ctx); err != nil {
		t.Fatal(err)
	}
	heldSince := *tg.g.lookup("shop").state(defaultWindow).HeldSince
	if err := tg.client.Route(ctx, "eks"); err != nil {
		t.Fatal(err)
	}
	tg.g.Close()

	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	// What a gateway from before apps reads: the default app, unchanged.
	var old struct {
		WriteBackend string         `json:"writeBackend"`
		ReadWeights  map[string]int `json:"readWeights"`
		WritesHeld   bool           `json:"writesHeld"`
	}
	if err := json.Unmarshal(b, &old); err != nil || old.WriteBackend != "eks" || old.ReadWeights["eks"] != 100 || old.WritesHeld {
		t.Fatalf("default app in the state file: %+v (%v)\n%s", old, err, b)
	}
	var onDisk persistedState
	if err := json.Unmarshal(b, &onDisk); err != nil {
		t.Fatal(err)
	}
	if a := onDisk.Apps["shop"]; !a.WritesHeld || a.WriteBackend != "gke" || a.ReadWeights["eks"] != 30 || a.Backends["eks"] != spec.Backends["eks"] {
		t.Fatalf("shop in the state file: %s", b)
	}

	tg2 := newTestGW(t, cfg, "gke", "eks")
	if st := tg2.g.state(defaultWindow); st.WriteBackend != "eks" || st.WritesHeld {
		t.Fatalf("default app after restart: %+v", st)
	}
	st, err := tg2.client.App("shop").State(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !st.WritesHeld || st.HeldSince == nil || !st.HeldSince.Equal(heldSince) || st.WriteBackend != "gke" || st.ReadWeights["gke"] != 70 {
		t.Fatalf("shop after restart: %+v", st)
	}
	if r := tg2.hostDo(ctx, http.MethodGet, "blog.hopcharlie.dev", "/items", ""); r.status != http.StatusOK || r.body != "blog-onprem" {
		t.Fatalf("blog after restart: %+v", r)
	}
	// The resumed hold still holds shop's writes, and only shop's.
	res := tg2.asyncHostDo(ctx, http.MethodPost, "shop.hopcharlie.dev", "/orders", "after-restart")
	waitFor(t, "shop write held", func() bool { return tg2.appHeldWrites("shop") == 1 })
	if r := tg2.hostDo(ctx, http.MethodPost, "blog.hopcharlie.dev", "/orders", "blog-write"); r.status != http.StatusOK || blog["onprem"].got("blog-write") != 1 {
		t.Fatalf("blog write: %+v", r)
	}
	if shop["gke"].got("after-restart") != 0 {
		t.Fatal("held write reached the backend")
	}
	if err := tg2.client.App("shop").Route(ctx, "eks"); err != nil {
		t.Fatal(err)
	}
	if err := tg2.client.App("shop").ReleaseWrites(ctx); err != nil {
		t.Fatal(err)
	}
	if r := recv(t, res); r.status != http.StatusOK || shop["eks"].got("after-restart") != 1 {
		t.Fatalf("after release: %+v", r)
	}
	// A deleted app stays deleted.
	if err := tg2.client.DeleteApp(ctx, "blog"); err != nil {
		t.Fatal(err)
	}
	tg2.g.Close()
	g3, err := New(Config{Backends: tg2.g.cfg.Backends, StateFile: file, AppDomain: testDomain})
	if err != nil {
		t.Fatal(err)
	}
	defer g3.Close()
	if g3.lookup("blog") != nil || g3.lookup("shop") == nil {
		t.Fatal("restart after delete")
	}
}

// TestStateFileFormats: an old single-app state file needs no migration, and
// a state file with a broken app is refused rather than guessed.
func TestStateFileFormats(t *testing.T) {
	backends := []BackendConfig{
		{Name: "gke", URL: newFakeBackend(t, "gke").url(t)},
		{Name: "eks", URL: newFakeBackend(t, "eks").url(t)},
	}
	start := func(content string) (*Gateway, string, error) {
		file := filepath.Join(t.TempDir(), "gateway.json")
		if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		g, err := New(Config{Backends: backends, StateFile: file, AppDomain: testDomain})
		return g, file, err
	}

	// Written by the gateway before apps existed.
	g, file, err := start(`{
  "writeBackend": "eks",
  "readWeights": {"eks": 60, "gke": 40},
  "writesHeld": true,
  "heldSince": "2026-11-02T01:02:03Z"
}`)
	if err != nil {
		t.Fatal(err)
	}
	st := g.state(defaultWindow)
	if st.WriteBackend != "eks" || st.ReadWeights["gke"] != 40 || !st.WritesHeld || st.HeldSince.Format(time.RFC3339) != "2026-11-02T01:02:03Z" {
		t.Fatalf("old format: %+v", st)
	}
	if len(g.routers()) != 1 {
		t.Fatal("old format grew apps")
	}
	g.Close()
	if b, _ := os.ReadFile(file); strings.Contains(string(b), `"apps"`) || !strings.Contains(string(b), `"writesHeld": true`) {
		t.Fatalf("rewritten old format: %s", b)
	}

	app := func(name, body string) string {
		return fmt.Sprintf(`{"writeBackend":"gke","readWeights":{"gke":100},"apps":{%q:%s}}`, name, body)
	}
	for _, bad := range []struct{ content, want string }{
		{app("Shop", `{"backends":{"gke":"http://x"},"writeBackend":"gke","readWeights":{"gke":100}}`), "must match"},
		{app("app", `{"backends":{"gke":"http://x"},"writeBackend":"gke","readWeights":{"gke":100}}`), "default app"},
		{app("shop", `{"backends":{},"writeBackend":"gke","readWeights":{"gke":100}}`), "no backends"},
		{app("shop", `{"backends":{"gke":"http://x"},"writeBackend":"eks","readWeights":{"gke":100}}`), `"eks"`},
		{app("shop", `{"backends":{"gke":"http://x"},"writeBackend":"gke","readWeights":{"gke":100,"eks":10}}`), `"eks"`},
		{app("shop", `{"backends":{"gke":"http://x"},"writeBackend":"gke","readWeights":{"gke":90}}`), "sum to 100"},
	} {
		if _, _, err := start(bad.content); err == nil || !strings.Contains(err.Error(), bad.want) {
			t.Errorf("state %s: got %v, want an error containing %s", bad.content, err, bad.want)
		}
	}
}

// TestAppMetrics: the default app keeps the original metric names and labels
// for itself alone; registered apps are charlie_gateway_app_* with an app
// label, and a deleted app's series go away.
func TestAppMetrics(t *testing.T) {
	tg := newTestGW(t, Config{AppDomain: testDomain}, "gke", "eks")
	ctx := context.Background()
	spec, _ := appBackends(t, "shop", "gke", "eks")
	if err := tg.client.PutApp(ctx, "shop", spec); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		tg.hostDo(ctx, http.MethodGet, "shop.hopcharlie.dev", "/items", "")
	}
	tg.hostDo(ctx, http.MethodGet, "app.hopcharlie.dev", "/items", "")
	tg.hostDo(ctx, http.MethodPost, "shop.hopcharlie.dev", "/orders", "w")

	m := tg.metrics(t)
	for _, want := range []string{
		`charlie_gateway_requests_total{backend="gke",class="read",code="200"} 1`,
		`charlie_gateway_write_backend{backend="gke"} 1`,
		`charlie_gateway_hold_expired_total 0`,
		`charlie_gateway_writes_held 0`,
		`charlie_gateway_app_requests_total{app="shop",backend="gke",class="read",code="200"} 2`,
		`charlie_gateway_app_requests_total{app="shop",backend="gke",class="write",code="200"} 1`,
		`charlie_gateway_app_request_duration_seconds_bucket{app="shop",backend="gke",class="write"`,
		`charlie_gateway_app_hold_expired_total{app="shop"} 0`,
		`charlie_gateway_app_held_dropped_total{app="shop"} 0`,
		`charlie_gateway_app_inflight{app="shop",class="write"} 0`,
		`charlie_gateway_app_held_writes{app="shop"} 0`,
		`charlie_gateway_app_writes_held{app="shop"} 0`,
		`charlie_gateway_app_read_weight{app="shop",backend="gke"} 100`,
		`charlie_gateway_app_write_backend{app="shop",backend="gke"} 1`,
		`charlie_gateway_app_backend_healthy{app="shop",backend="eks"} 1`,
	} {
		if !strings.Contains(m, want) {
			t.Errorf("metrics missing %s", want)
		}
	}
	if strings.Contains(m, `charlie_gateway_requests_total{backend="gke",class="write"`) {
		t.Error("shop's write counted for the default app")
	}

	if err := tg.client.DeleteApp(ctx, "shop"); err != nil {
		t.Fatal(err)
	}
	if m := tg.metrics(t); strings.Contains(m, `app="shop"`) {
		t.Errorf("deleted app still in metrics:\n%s", m)
	}
}

// TestSecondAppWithEngineClient: a second app registered through the admin
// API is reached by its host name, and the engine's own client migrates it
// with <admin>/apps/<app> as the gateway URL, leaving the default app alone.
func TestSecondAppWithEngineClient(t *testing.T) {
	tg := newTestGW(t, Config{AppDomain: testDomain}, "gke", "eks")
	ctx := context.Background()
	spec, shop := appBackends(t, "shop", "gke", "eks")
	if err := tg.client.PutApp(ctx, "shop", spec); err != nil {
		t.Fatal(err)
	}
	const host = "shop.hopcharlie.dev"
	if r := tg.hostDo(ctx, http.MethodGet, host, "/items", ""); r.status != http.StatusOK || r.body != "shop-gke" || r.backend != "gke" {
		t.Fatalf("read through the host name: %+v", r)
	}

	eng := enginegw.NewHTTP(tg.admin.URL+"/apps/shop/", testToken)
	if err := eng.Health(ctx); err != nil {
		t.Fatal(err)
	}
	if err := eng.HoldWrites(ctx); err != nil {
		t.Fatal(err)
	}
	pending := tg.asyncHostDo(ctx, http.MethodPost, host, "/orders", "during-fence")
	waitFor(t, "write held", func() bool { return tg.appHeldWrites("shop") == 1 })
	if err := eng.Route(ctx, "eks"); err != nil {
		t.Fatal(err)
	}
	if err := eng.ReleaseWrites(ctx); err != nil {
		t.Fatal(err)
	}
	if r := recv(t, pending); r.status != http.StatusOK || r.backend != "eks" || shop["eks"].got("during-fence") != 1 {
		t.Fatalf("held write: %+v", r)
	}
	st, err := tg.client.App("shop").State(ctx, 0)
	if err != nil || st.WriteBackend != "eks" || st.Totals["eks"].Requests != 1 || st.Totals["gke"].Requests != 1 {
		t.Fatalf("shop state: %+v %v", st, err)
	}
	if st := tg.g.state(defaultWindow); st.WriteBackend != "gke" || st.Totals["gke"].Requests != 0 {
		t.Fatalf("default app: %+v", st)
	}
	if err := enginegw.NewHTTP(tg.admin.URL+"/apps/nope", testToken).HoldWrites(ctx); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("unknown app: %v", err)
	}
}

// TestAppsConcurrent exercises the registry under -race: apps come and go
// while traffic, holds, health checks, metrics and the state file run.
func TestAppsConcurrent(t *testing.T) {
	file := filepath.Join(t.TempDir(), "gateway.json")
	tg := newTestGW(t, Config{StateFile: file, AppDomain: testDomain, HealthInterval: 5 * time.Millisecond}, "gke", "eks")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	spec, _ := appBackends(t, "shop", "gke", "eks")
	if err := tg.client.PutApp(ctx, "shop", spec); err != nil {
		t.Fatal(err)
	}
	churnA, _ := appBackends(t, "churn", "gke", "eks")
	churnB, _ := appBackends(t, "churn", "aks")

	healthDone := make(chan struct{})
	go func() { tg.g.Run(ctx); close(healthDone) }()

	var (
		wg   sync.WaitGroup
		stop atomic.Bool
		bad  atomic.Value
	)
	fail := func(format string, args ...any) { bad.CompareAndSwap(nil, fmt.Sprintf(format, args...)) }
	loop := func(f func(i int)) {
		wg.Go(func() {
			for i := 0; !stop.Load(); i++ {
				f(i)
			}
		})
	}
	hosts := []string{"shop.hopcharlie.dev", "churn.hopcharlie.dev", "app.hopcharlie.dev", "example.com"}
	for w := range 4 {
		loop(func(i int) {
			host := hosts[(i+w)%len(hosts)]
			method := http.MethodGet
			if i%2 == 0 {
				method = http.MethodPost
			}
			r := tg.hostDo(context.Background(), method, host, "/orders", "x")
			switch {
			case r.err != nil:
				fail("%s %s: %v", method, host, r.err)
			case r.status == http.StatusOK:
			case r.status == http.StatusNotFound && host == "churn.hopcharlie.dev":
			default:
				fail("%s %s: %d %s", method, host, r.status, r.body)
			}
		})
	}
	loop(func(i int) {
		s := churnA
		if i%3 == 1 {
			s = churnB
		}
		var err error
		if i%3 == 2 {
			err = tg.client.DeleteApp(context.Background(), "churn")
		} else {
			err = tg.client.PutApp(context.Background(), "churn", s)
		}
		if err != nil {
			fail("churn %d: %v", i, err)
		}
	})
	loop(func(int) {
		shopc := tg.client.App("shop")
		if err := shopc.HoldWrites(context.Background()); err != nil {
			fail("hold: %v", err)
		}
		time.Sleep(time.Millisecond)
		if err := shopc.ReleaseWrites(context.Background()); err != nil {
			fail("release: %v", err)
		}
	})
	loop(func(int) {
		if _, err := tg.client.Apps(context.Background()); err != nil {
			fail("apps: %v", err)
		}
		if _, err := tg.client.App("shop").State(context.Background(), 0); err != nil {
			fail("state: %v", err)
		}
		resp, err := http.Get(tg.admin.URL + "/metrics")
		if err != nil {
			fail("metrics: %v", err)
			return
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	})
	time.Sleep(time.Second)
	stop.Store(true)
	wg.Wait()
	cancel()
	<-healthDone
	if v := bad.Load(); v != nil {
		t.Fatal(v)
	}
	st, err := tg.client.App("shop").State(context.Background(), 0)
	if err != nil || st.WritesHeld || st.InFlight.Writes != 0 || st.Totals["gke"].Requests == 0 {
		t.Fatalf("shop at the end: %+v %v", st, err)
	}
	tg.g.Close()
	if _, err := loadState(file); err != nil {
		t.Fatal(err)
	}
}
