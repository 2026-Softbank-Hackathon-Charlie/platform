package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
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
	p := &Prober{ProxyURL: srv.URL, AppDomain: "hopcharlie.dev", HTTP: srv.Client()}

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
