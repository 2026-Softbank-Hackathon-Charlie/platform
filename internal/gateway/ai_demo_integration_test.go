package gateway

import (
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
	"time"
)

// This host is reachable only on ephemeral loopback ports and is inert in normal tests.
func TestAIRehearsalHost(t *testing.T) {
	if os.Getenv("AI_REHEARSAL_HOST") != "1" {
		t.Skip("explicit local rehearsal only")
	}
	token, adminToken := os.Getenv("AI_GATEWAY_TOKEN"), os.Getenv("GATEWAY_ADMIN_TOKEN")
	if len(token) < 32 || len(adminToken) < 32 {
		t.Fatal("process-scoped tokens required")
	}
	backends := []BackendConfig{}
	for _, name := range []string{"gcp", "laptop"} {
		u, err := url.Parse(os.Getenv("AI_UPSTREAM_" + name))
		if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.Port() == "" || u.Path != "" || u.RawQuery != "" || u.User != nil {
			t.Fatal("loopback-only upstream required")
		}
		backends = append(backends, BackendConfig{Name: name, URL: u})
	}
	g, err := New(Config{Backends: backends, InitialBackend: "gcp", DefaultApp: "moimroom-ai", AdminToken: adminToken, WriteTimeout: 12 * time.Second, AIDemo: &AIDemoConfig{App: "moimroom-ai", Token: token}})
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	public := httptest.NewTLSServer(g.Proxy())
	defer public.Close()
	stop := make(chan struct{}, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /__rehearsal_stop", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+adminToken {
			w.WriteHeader(401)
			return
		}
		select {
		case stop <- struct{}{}:
		default:
		}
		w.WriteHeader(204)
	})
	mux.Handle("/", g.Admin())
	admin := httptest.NewServer(mux)
	defer admin.Close()
	cert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: public.Certificate().Raw})
	ready, _ := json.Marshal(map[string]any{"url": public.URL, "admin": admin.URL, "rootPEM": base64.StdEncoding.EncodeToString(cert), "pid": os.Getpid()})
	fmt.Println("AI_HOST " + string(ready))
	select {
	case <-stop:
	case <-time.After(180 * time.Second):
		t.Fatal("rehearsal host deadline")
	}
}
