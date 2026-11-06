package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestProbeAddressesTheAppScope(t *testing.T) {
	hosts := make(chan string, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/" || r.UserAgent() != "hopcharlie-canary-probe" {
			t.Errorf("probe request: %s %s %q", r.Method, r.URL.Path, r.UserAgent())
		}
		hosts <- r.Host
	}))
	defer srv.Close()
	p := &Prober{ProxyURL: srv.URL, AppDomain: "hopcharlie.dev", HTTP: ProbeClient()}

	if err := p.Probe(context.Background(), "http://hopcharlie-gateway:9901/apps/moimroom-mig"); err != nil {
		t.Fatal(err)
	}
	if h := <-hosts; h != "moimroom-mig.hopcharlie.dev" {
		t.Fatalf("app scope host = %q", h)
	}
	// the gateway root is the default app: no app host
	if err := p.Probe(context.Background(), "http://hopcharlie-gateway:9901"); err != nil {
		t.Fatal(err)
	}
	if h := <-hosts; h != srv.Listener.Addr().String() {
		t.Fatalf("root host = %q", h)
	}
}

func TestProbeDoesNotFollowRedirects(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Redirect(w, r, "/login", http.StatusFound)
	}))
	defer srv.Close()
	p := &Prober{ProxyURL: srv.URL, HTTP: ProbeClient()}
	if err := p.Probe(context.Background(), "http://gw:9901/apps/a"); err != nil {
		t.Fatal(err)
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("requests = %d, want 1 (no redirect followed)", n)
	}
}
