//go:build linux

package main

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testUnixRelay(t *testing.T, owner http.Handler) (*httptest.Server, *http.Transport) {
	t.Helper()
	dir, err := os.MkdirTemp("", "hop-relay-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "owner.sock")
	if owner != nil {
		ln, err := net.Listen("unix", socket)
		if err != nil {
			t.Fatal(err)
		}
		s := &http.Server{Handler: owner}
		go s.Serve(ln)
		t.Cleanup(func() { s.Close() })
	}
	h, transport := unixRelay(socket)
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	t.Cleanup(transport.CloseIdleConnections)
	return s, transport
}

func TestRelayWithoutOwnerFailsClosed(t *testing.T) {
	s, _ := testUnixRelay(t, nil)
	for _, method := range []string{"GET", "POST", "PUT", "DELETE"} {
		req, _ := http.NewRequest(method, s.URL+"/v1/writes/release", strings.NewReader("secret-body"))
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != 503 || res.Header.Get("Retry-After") != "1" || strings.Contains(string(b), "secret-body") {
			t.Fatalf("%s: %d %s", method, res.StatusCode, b)
		}
	}
}

func TestRelayPreservesAuthorizationHostAndStreaming(t *testing.T) {
	finish := make(chan struct{})
	defer close(finish)
	observed := make(chan string, 1)
	s, _ := testUnixRelay(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		observed <- r.Host + "|" + r.Header.Get("Authorization") + "|" + r.Header.Get("X-Remove-Me") + "|" + string(body)
		io.WriteString(w, "first\n")
		w.(http.Flusher).Flush()
		<-finish
		io.WriteString(w, "second\n")
	}))
	req, _ := http.NewRequest("POST", s.URL, strings.NewReader("request-body"))
	req.Host = "shop.hopcharlie.dev"
	req.Header.Set("Authorization", "Bearer bounded-token")
	req.Header.Set("Connection", "X-Remove-Me")
	req.Header.Set("X-Remove-Me", "hop-by-hop-secret")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	res, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	line, err := bufio.NewReader(res.Body).ReadString('\n')
	if err != nil || line != "first\n" {
		t.Fatalf("stream did not flush before completion: %q %v", line, err)
	}
	if got := <-observed; got != "shop.hopcharlie.dev|Bearer bounded-token||request-body" {
		t.Fatal(got)
	}
}

func TestRelayNeverRetriesUnknownWrite(t *testing.T) {
	var attempts atomic.Int64
	s, transport := testUnixRelay(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		io.Copy(io.Discard, r.Body)
		c, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			c.Close()
		}
	}))
	if !transport.DisableKeepAlives {
		t.Fatal("reused connections could retry idempotency-key POSTs")
	}
	req, _ := http.NewRequest("POST", s.URL, strings.NewReader("commit-once"))
	req.Header.Set("Idempotency-Key", "this-is-not-proof-of-an-uncommitted-write")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 503 || attempts.Load() != 1 {
		t.Fatalf("status=%d attempts=%d", res.StatusCode, attempts.Load())
	}
}

func TestStateLockRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "state.json")
	target := filepath.Join(dir, "other")
	if err := os.WriteFile(target, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, state+".lock"); err != nil {
		t.Fatal(err)
	}
	if l, err := openStateLock(state); err == nil {
		l.Close()
		t.Fatal("symlink lock accepted")
	}
	if b, _ := os.ReadFile(target); string(b) != "keep" {
		t.Fatal("symlink target changed")
	}
}
