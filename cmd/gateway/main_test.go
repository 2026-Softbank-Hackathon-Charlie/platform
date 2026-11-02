package main

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/gateway"
)

type result struct {
	status     int
	retryAfter string
	body       string
	err        error
}

func send(method, u string) <-chan result {
	ch := make(chan result, 1)
	go func() {
		req, _ := http.NewRequest(method, u, strings.NewReader("w"))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			ch <- result{err: err}
			return
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		ch <- result{status: resp.StatusCode, retryAfter: resp.Header.Get("Retry-After"), body: string(b)}
	}()
	return ch
}

func recv(t *testing.T, ch <-chan result) result {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a response")
		return result{}
	}
}

func serve(t *testing.T, h http.Handler) (*http.Server, string) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &http.Server{Handler: h}
	go func() { _ = s.Serve(ln) }()
	t.Cleanup(func() { s.Close() })
	return s, "http://" + ln.Addr().String()
}

// TestShutdown: held writes are answered 503 shutting_down instead of being
// dropped, the admin API keeps answering until the proxy has finished, and
// the hold is still recorded for the next start.
func TestShutdown(t *testing.T) {
	slowStarted, finishSlow := make(chan struct{}), make(chan struct{})
	writes := make(chan string, 10)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/slow":
			close(slowStarted)
			<-finishSlow
		case r.Method == http.MethodPost:
			writes <- r.URL.Path
		}
		_, _ = io.WriteString(w, "ok")
	}))
	defer backend.Close()
	var once sync.Once
	letSlowFinish := func() { once.Do(func() { close(finishSlow) }) }
	defer letSlowFinish() // before backend.Close, which waits for the slow request
	u, _ := url.Parse(backend.URL)
	file := filepath.Join(t.TempDir(), "gateway.json")
	gw, err := gateway.New(gateway.Config{Backends: []gateway.BackendConfig{{Name: "gke", URL: u}}, StateFile: file})
	if err != nil {
		t.Fatal(err)
	}
	public, publicURL := serve(t, gw.Proxy())
	admin, adminURL := serve(t, gw.Admin())
	// No keep-alive: every call needs the admin listener to be open.
	client := &gateway.Client{BaseURL: adminURL, HTTP: &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}}
	ctx := context.Background()

	if err := client.HoldWrites(ctx); err != nil {
		t.Fatal(err)
	}
	held := send(http.MethodPost, publicURL+"/orders")
	for {
		st, err := client.State(ctx, 0)
		if err != nil {
			t.Fatal(err)
		}
		if st.HeldWrites == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	slow := send(http.MethodGet, publicURL+"/slow") // keeps the proxy busy
	<-slowStarted

	done := make(chan struct{})
	go func() {
		shutdown(slog.New(slog.NewTextHandler(io.Discard, nil)), gw, public, admin, 5*time.Second)
		close(done)
	}()
	if r := recv(t, held); r.err != nil || r.status != http.StatusServiceUnavailable || r.retryAfter != "1" || !strings.Contains(r.body, "shutting_down") {
		t.Fatalf("held write at shutdown: %+v", r)
	}
	if _, err := client.State(ctx, 0); err != nil {
		t.Fatalf("admin API stopped before the proxy finished: %v", err)
	}
	select {
	case <-done:
		t.Fatal("shutdown returned while a request was in flight")
	default:
	}

	letSlowFinish()
	if r := recv(t, slow); r.err != nil || r.status != http.StatusOK {
		t.Fatalf("in-flight read at shutdown: %+v", r)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not return")
	}
	if err := client.Health(ctx); err == nil {
		t.Fatal("admin API still up after shutdown")
	}
	gw.Close()
	if len(writes) != 0 {
		t.Fatal("a held write was forwarded")
	}
	if b, _ := os.ReadFile(file); !strings.Contains(string(b), `"writesHeld": true`) {
		t.Fatalf("hold not kept in the state file: %s", b)
	}
}
