//go:build linux

package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/gateway"
)

// These are real, independent gateway processes sharing only a local directory.
// Killing the owner must not reset its persisted hold, routing or app registry.
func TestExclusiveProcessFailover(t *testing.T) {
	if os.Getenv("HOP_EXCLUSIVE_HELPER") == "1" {
		u, _ := url.Parse(os.Getenv("HOP_EXCLUSIVE_BACKEND"))
		cfg := gateway.Config{Backends: []gateway.BackendConfig{{Name: "gke", URL: u}, {Name: "onprem", URL: u}}, StateFile: os.Getenv("HOP_EXCLUSIVE_STATE"), AdminToken: "test-admin", MaxHold: 150 * time.Millisecond, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
		ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM)
		defer cancel()
		if err := runExclusive(ctx, cfg, os.Getenv("HOP_EXCLUSIVE_PUBLIC"), os.Getenv("HOP_EXCLUSIVE_ADMIN"), cfg.Log); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		os.Exit(0)
	}
	var writes atomic.Int64
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			writes.Add(1)
		}
		w.Write([]byte("ok"))
	}))
	defer backend.Close()
	// A short path also exercises Linux's bounded sockaddr_un path.
	dir, err := os.MkdirTemp("", "hop-ha-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	state := filepath.Join(dir, "state.json")
	addresses := func() (string, string) {
		a := func() string {
			l, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer l.Close()
			return l.Addr().String()
		}
		return a(), a()
	}
	start := func() (*exec.Cmd, string, string) {
		public, admin := addresses()
		exe, _ := os.Executable()
		cmd := exec.Command(exe, "-test.run=^TestExclusiveProcessFailover$")
		cmd.Env = append(os.Environ(), "HOP_EXCLUSIVE_HELPER=1", "HOP_EXCLUSIVE_BACKEND="+backend.URL, "HOP_EXCLUSIVE_STATE="+state, "HOP_EXCLUSIVE_PUBLIC="+public, "HOP_EXCLUSIVE_ADMIN="+admin)
		var output bytes.Buffer
		cmd.Stderr = &output
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			cmd.Process.Kill()
			cmd.Wait()
			if output.Len() > 0 {
				t.Log(output.String())
			}
		})
		return cmd, "http://" + public, "http://" + admin
	}
	request := func(method, endpoint, body string) (int, string) {
		req, _ := http.NewRequest(method, endpoint, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer test-admin")
		client := &http.Client{Timeout: time.Second}
		resp, err := client.Do(req)
		if err != nil {
			return 0, ""
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	waitReady := func(admin string) {
		t.Helper()
		deadline := time.Now().Add(8 * time.Second)
		for time.Now().Before(deadline) {
			if code, _ := request("GET", admin+"/healthz", ""); code == 200 {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatal("gateway did not become ready")
	}
	owner, publicA, adminA := start()
	waitReady(adminA)
	standby, publicB, adminB := start()
	waitReady(adminB)
	// A clean SIGTERM drains, durably checkpoints, clears the marker and lets
	// the other process serve writes without entering unknown-owner recovery.
	if err := owner.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := owner.Wait(); err != nil {
		t.Fatalf("clean exit: %v", err)
	}
	waitReady(adminB)
	if code, body := request("POST", publicB+"/after-clean-handoff", "clean"); code != 200 {
		t.Fatalf("clean handoff write: %d %s", code, body)
	}
	if writes.Swap(0) != 1 {
		t.Fatal("clean handoff write duplicated")
	}
	owner, publicA, adminA = standby, publicB, adminB
	standby, publicB, adminB = start()
	waitReady(adminB)
	for _, admin := range []string{adminA, adminB} {
		if code, body := request("PUT", admin+"/apps/shop", `{"backends":{"gke":"`+backend.URL+`","onprem":"`+backend.URL+`"},"writeBackend":"gke"}`); code != 201 && code != 200 {
			t.Fatalf("register: %d %s", code, body)
		}
	}
	if code, body := request("POST", adminB+"/v1/writes/hold", ""); code != 200 {
		t.Fatalf("hold via relay: %d %s", code, body)
	}
	if code, body := request("POST", adminA+"/v1/route", `{"backend":"onprem"}`); code != 200 {
		t.Fatalf("route: %d %s", code, body)
	}
	for _, public := range []string{publicA, publicB} {
		if code, _ := request("POST", public+"/orders", "one"); code != 503 {
			t.Fatalf("hold bypassed: %d", code)
		}
	}
	if writes.Load() != 0 {
		t.Fatal("held write reached backend")
	}
	if err := owner.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	// Wait on health/state, never POST retries: unknown writes must remain unknown.
	waitReady(adminB)
	deadline := time.Now().Add(8 * time.Second)
	for {
		code, body := request("GET", adminB+"/v1/state", "")
		if code == 200 && strings.Contains(body, `"writeBackend":"onprem"`) && strings.Contains(body, `"writesHeld":true`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("hold/route not recovered: %d %s", code, body)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if code, body := request("GET", adminB+"/apps/shop/v1/state", ""); code != 200 {
		t.Fatalf("app registry lost: %d %s", code, body)
	}
	if code, _ := request("POST", publicB+"/orders", "two"); code != 503 {
		t.Fatalf("hold lost on failover: %d", code)
	}
	if writes.Load() != 0 {
		t.Fatal("write leaked during failover")
	}
	for _, path := range []string{"/v1/writes/release", "/v1/writes/hold", "/v1/route"} {
		if code, body := request("POST", adminB+path, `{"backend":"gke"}`); code != 503 || !strings.Contains(body, "owner_recovery_required") {
			t.Fatalf("unclean control bypass %s: %d %s", path, code, body)
		}
	}
	// Explicit operator recovery: BOTH processes are dead, controlled backend
	// proves no write was dispatched, and only then is the marker removed.
	owner.Wait()
	standby.Process.Kill()
	standby.Wait()
	if err := os.Remove(state + ".running"); err != nil {
		t.Fatal(err)
	}
	_, recoveredPublic, recoveredAdmin := start()
	waitReady(recoveredAdmin)
	if code, body := request("POST", recoveredAdmin+"/v1/writes/release", ""); code != 200 {
		t.Fatalf("explicit recovery release: %d %s", code, body)
	}
	if code, body := request("POST", recoveredPublic+"/orders", "three"); code != 200 {
		t.Fatalf("write after release: %d %s", code, body)
	}
	if writes.Load() != 1 {
		t.Fatalf("write duplicated: %d", writes.Load())
	}
}

func TestOwnerMarkerRemainsOnUncleanState(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state.json")
	unclean, err := markOwnerRunning(state)
	if err != nil || unclean {
		t.Fatalf("first start: %v %v", unclean, err)
	}
	unclean, err = markOwnerRunning(state)
	if err != nil || !unclean {
		t.Fatalf("unclean exit: %v %v", unclean, err)
	}
	if err := clearOwnerRunning(state); err != nil {
		t.Fatal(err)
	}
	unclean, err = markOwnerRunning(state)
	if err != nil || unclean {
		t.Fatalf("clean handoff: %v %v", unclean, err)
	}
}

func TestExclusiveUnknownInflightWriteCannotBeReleased(t *testing.T) {
	started, finish := make(chan struct{}), make(chan struct{})
	var writes atomic.Int64
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			writes.Add(1)
			close(started)
			<-finish
		}
		w.Write([]byte("ok"))
	}))
	defer backend.Close()
	defer close(finish)
	dir, err := os.MkdirTemp("", "hop-unknown-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	state := filepath.Join(dir, "s.json")
	free := func() string {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()
		return l.Addr().String()
	}
	start := func() (*exec.Cmd, string, string) {
		public, admin := free(), free()
		exe, _ := os.Executable()
		cmd := exec.Command(exe, "-test.run=^TestExclusiveProcessFailover$")
		cmd.Env = append(os.Environ(), "HOP_EXCLUSIVE_HELPER=1", "HOP_EXCLUSIVE_BACKEND="+backend.URL, "HOP_EXCLUSIVE_STATE="+state, "HOP_EXCLUSIVE_PUBLIC="+public, "HOP_EXCLUSIVE_ADMIN="+admin)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
		return cmd, "http://" + public, "http://" + admin
	}
	check := func(method, endpoint string) int {
		req, _ := http.NewRequest(method, endpoint, strings.NewReader(`{"backend":"onprem"}`))
		req.Header.Set("Authorization", "Bearer test-admin")
		res, err := (&http.Client{Timeout: time.Second}).Do(req)
		if err != nil {
			return 0
		}
		defer res.Body.Close()
		io.Copy(io.Discard, res.Body)
		return res.StatusCode
	}
	wait := func(admin string) {
		t.Helper()
		deadline := time.Now().Add(8 * time.Second)
		for time.Now().Before(deadline) {
			if check("GET", admin+"/healthz") == 200 {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatal("owner unavailable")
	}
	owner, publicA, adminA := start()
	wait(adminA)
	_, publicB, adminB := start()
	wait(adminB)
	done := make(chan struct{})
	go func() { check("POST", publicA+"/commit"); close(done) }()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("write not dispatched")
	}
	owner.Process.Kill()
	owner.Wait()
	wait(adminB)
	// The remote commit is STILL RUNNING. Local inflight=0 cannot prove drain.
	for _, target := range []string{publicB + "/commit", adminB + "/v1/writes/hold", adminB + "/v1/writes/release", adminB + "/v1/route"} {
		if code := check("POST", target); code != 503 {
			t.Fatalf("unknown owner write bypassed gate: %s %d", target, code)
		}
	}
	if code := check("GET", publicB+"/read"); code != 200 {
		t.Fatalf("read failover: %d", code)
	}
	if writes.Load() != 1 {
		t.Fatalf("unknown write retried: %d", writes.Load())
	}
	<-done
}

func TestExclusiveLockStableAcrossStateRename(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	a, err := openStateLock(path)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if ok, err := a.TryLock(); err != nil || !ok {
		t.Fatalf("first: %v %v", ok, err)
	}
	if err := os.WriteFile(path+".tmp", []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path+".tmp", path); err != nil {
		t.Fatal(err)
	}
	b, err := openStateLock(path)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if ok, err := b.TryLock(); err != nil || ok {
		t.Fatalf("second owner after rename: %v %v", ok, err)
	}
}
