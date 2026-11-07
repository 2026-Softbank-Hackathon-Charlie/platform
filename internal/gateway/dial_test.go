package gateway

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// legacyVM is an HTTPS server standing for an onboarding's source VM: its
// certificate is for example.com (httptest's), and it records the Host and
// TLS server name of every request.
type legacyVM struct {
	srv  *httptest.Server
	mu   sync.Mutex
	seen []string // "host sni path"
}

func newLegacyVM(t *testing.T) *legacyVM {
	t.Helper()
	vm := &legacyVM{}
	vm.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		vm.mu.Lock()
		vm.seen = append(vm.seen, r.Host+" "+r.TLS.ServerName+" "+r.URL.Path)
		vm.mu.Unlock()
		_, _ = w.Write([]byte("legacy"))
	}))
	t.Cleanup(vm.srv.Close)
	return vm
}

func (vm *legacyVM) requests() []string {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	return append([]string(nil), vm.seen...)
}

// trustingTransport trusts vm's certificate, as the gateway trusts the
// public CAs of a real VM's certificate.
func trustingTransport(vm *legacyVM) *http.Transport {
	pool := x509.NewCertPool()
	pool.AddCert(vm.srv.Certificate())
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.TLSClientConfig = &tls.Config{RootCAs: pool}
	return t
}

// TestDialBackend: a backend with a dial address is reached at that address
// by the proxy and the health check, while its URL's host stays the TLS
// server name and the Host header (ADR 0042). The address survives the
// state file, and changing it replaces the backend.
func TestDialBackend(t *testing.T) {
	vm := newLegacyVM(t)
	file := filepath.Join(t.TempDir(), "state.json")
	cfg := Config{StateFile: file, AppDomain: testDomain, Transport: trustingTransport(vm)}
	tg := newTestGW(t, cfg, "gke")
	ctx := context.Background()
	addr := vm.srv.Listener.Addr().String()
	// example.com does not resolve to the test server: only the dial
	// address can reach it.
	spec := AppSpec{
		Backends:     map[string]string{"legacy": "https://example.com", "eks": tg.backends["gke"].url(t).String()},
		WriteBackend: "legacy",
		Dial:         map[string]string{"legacy": addr},
	}
	if err := tg.client.PutApp(ctx, "shop", spec); err != nil {
		t.Fatal(err)
	}
	r := tg.hostDo(ctx, http.MethodGet, "shop."+testDomain, "/items", "")
	if r.status != http.StatusOK || r.body != "legacy" {
		t.Fatalf("proxied read: %+v", r)
	}
	if got := vm.requests(); len(got) != 1 || got[0] != "example.com example.com /items" {
		t.Fatalf("legacy saw %q, want Host and SNI example.com", got)
	}

	rt := tg.g.lookup("shop")
	b := rt.byName["legacy"]
	shared := &http.Client{Transport: tg.g.cfg.HealthTransport, Timeout: time.Second}
	b.healthy.Store(false)
	rt.checkBackend(ctx, shared, b)
	if !b.healthy.Load() || b.lastErr != "" {
		t.Fatalf("health check of the dialled backend: healthy=%v err=%q", b.healthy.Load(), b.lastErr)
	}
	if got := vm.requests(); len(got) != 2 || got[1] != "example.com example.com /healthz" {
		t.Fatalf("health check reached %q", got)
	}

	list, err := tg.client.Apps(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var info AppInfo
	for _, a := range list.Apps {
		if a.Name == "shop" {
			info = a
		}
	}
	if info.Dial["legacy"] != addr || len(info.Dial) != 1 {
		t.Fatalf("AppInfo.Dial = %v", info.Dial)
	}

	// The same spec keeps the backend; another address replaces it.
	if err := tg.client.PutApp(ctx, "shop", spec); err != nil {
		t.Fatal(err)
	}
	if tg.g.lookup("shop").byName["legacy"] != b {
		t.Fatal("an unchanged dial address replaced the backend")
	}
	moved := spec
	moved.Dial = map[string]string{"legacy": "127.0.0.1:1"}
	if err := tg.client.PutApp(ctx, "shop", moved); err != nil {
		t.Fatal(err)
	}
	if nb := tg.g.lookup("shop").byName["legacy"]; nb == b || nb.dial != "127.0.0.1:1" {
		t.Fatal("a changed dial address kept the backend")
	}
	if err := tg.client.PutApp(ctx, "shop", spec); err != nil {
		t.Fatal(err)
	}

	// A gateway restored from the state file dials the same address.
	g2, err := New(Config{Backends: tg.g.cfg.Backends, StateFile: file, AppDomain: testDomain, Transport: trustingTransport(vm)})
	if err != nil {
		t.Fatal(err)
	}
	defer g2.Close()
	if d := g2.lookup("shop").info().Dial; d["legacy"] != addr {
		t.Fatalf("restored dial = %v", d)
	}
	proxy := httptest.NewServer(g2.Proxy())
	defer proxy.Close()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, proxy.URL+"/after", nil)
	req.Host = "shop." + testDomain
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := vm.requests(); resp.StatusCode != http.StatusOK || got[len(got)-1] != "example.com example.com /after" {
		t.Fatalf("restored proxy: %d %q", resp.StatusCode, got)
	}
}
