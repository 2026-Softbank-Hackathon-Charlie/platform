package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"
	"time"
)

func TestAppReadinessPathPersistsAndUsesQuery(t *testing.T) {
	seen := make(chan string, 2)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.URL.RequestURI()
		if r.URL.Path == "/readyz" {
			w.WriteHeader(503)
		}
	}))
	defer s.Close()
	file := filepath.Join(t.TempDir(), "gateway.json")
	tg := newTestGW(t, Config{StateFile: file, AppDomain: testDomain}, "src")
	spec := AppSpec{Backends: map[string]string{"src": s.URL}, WriteBackend: "src", HealthPath: "/readyz?full=1"}
	if err := tg.client.PutApp(context.Background(), "shop", spec); err != nil {
		t.Fatal(err)
	}
	rt := tg.g.lookup("shop")
	rt.checkBackend(context.Background(), s.Client(), rt.byName["src"])
	if got := <-seen; got != "/readyz?full=1" {
		t.Fatal(got)
	}
	if rt.byName["src"].healthy.Load() {
		t.Fatal("liveness was mistaken for readiness")
	}
	restored, err := New(tg.g.cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if got := restored.lookup("shop").info().HealthPath; got != spec.HealthPath {
		t.Fatal("lost app readiness configuration")
	}
	spec.HealthPath = "https://example.com/readyz"
	if err := tg.client.PutApp(context.Background(), "shop", spec); err == nil {
		t.Fatal("accepted external readiness URL")
	}
}

func TestHealthRedirectIsNotHealthy(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			http.Redirect(w, r, "/login", 302)
		} else {
			w.WriteHeader(200)
		}
	}))
	defer s.Close()
	u, _ := url.Parse(s.URL)
	g, err := New(Config{Backends: []BackendConfig{{Name: "src", URL: u}}, HealthInterval: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { g.Run(ctx); close(done) }()
	defer func() { cancel(); <-done; g.Close() }()
	waitFor(t, "redirect rejected", func() bool { return !g.byName["src"].healthy.Load() })
}
