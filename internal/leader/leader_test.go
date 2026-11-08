package leader

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func handler(name string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, name+" "+r.Header.Get(ForwardedHeader))
	})
}

func do(t *testing.T, h http.Handler, method, path string) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader("{}")))
	return rec.Code, strings.TrimSpace(rec.Body.String())
}

func TestLeaderOnly(t *testing.T) {
	for _, c := range []struct {
		method, path string
		want         bool
	}{
		{"GET", "/v1/migrations", false},
		{"GET", "/v1/migrations/r1/events", true},
		{"HEAD", "/healthz", false},
		{"OPTIONS", "/v1/migrations", false},
		{"POST", "/v1/migrations", true},
		{"PUT", "/v1/apps/a", true},
		{"DELETE", "/v1/apps/a", true},
		{"PATCH", "/v1/apps/a", true},
	} {
		if got := LeaderOnly(httptest.NewRequest(c.method, c.path, nil)); got != c.want {
			t.Errorf("%s %s = %v", c.method, c.path, got)
		}
	}
}

func TestGateServesLocallyWhenActive(t *testing.T) {
	g := &Gate{Self: "self:1", LeaderAddr: func(context.Context) (string, error) { t.Fatal("looked up"); return "", nil }}
	g.Activate()
	h := g.Wrap(handler("local"))
	if code, body := do(t, h, "POST", "/v1/x"); code != 200 || body != "local" {
		t.Fatalf("%d %q", code, body)
	}
}

func TestGateForwardsToLeader(t *testing.T) {
	leaderSrv := httptest.NewServer(handler("leader"))
	defer leaderSrv.Close()
	addr := strings.TrimPrefix(leaderSrv.URL, "http://")
	g := &Gate{Self: "self:1", LeaderAddr: func(context.Context) (string, error) { return addr, nil }}
	h := g.Wrap(handler("local"))
	if code, body := do(t, h, "POST", "/v1/x"); code != 200 || body != "leader 1" {
		t.Fatalf("POST: %d %q", code, body)
	}
	if code, body := do(t, h, "GET", "/v1/x/events"); code != 200 || body != "leader 1" {
		t.Fatalf("stream: %d %q", code, body)
	}
	// Reads stay here.
	if code, body := do(t, h, "GET", "/v1/x"); code != 200 || body != "local" {
		t.Fatalf("GET: %d %q", code, body)
	}
}

func TestGateNeverForwardsTwice(t *testing.T) {
	g := &Gate{Self: "self:1", LeaderAddr: func(context.Context) (string, error) { return "other:1", nil }}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/x", nil)
	req.Header.Set(ForwardedHeader, "1")
	g.Wrap(handler("local")).ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" || !strings.Contains(rec.Body.String(), "leader_unavailable") {
		t.Fatalf("%d %q", rec.Code, rec.Body.String())
	}
}

func TestGateWaitsForLeadershipHere(t *testing.T) {
	g := &Gate{Self: "self:1", Wait: 5 * time.Second, Poll: 10 * time.Millisecond,
		LeaderAddr: func(context.Context) (string, error) { return "self:1", nil }} // our own name: never forwarded to
	time.AfterFunc(100*time.Millisecond, g.Activate)
	start := time.Now()
	if code, body := do(t, g.Wrap(handler("local")), "POST", "/v1/x"); code != 200 || body != "local" {
		t.Fatalf("%d %q", code, body)
	}
	if time.Since(start) < 90*time.Millisecond {
		t.Fatal("served before activation")
	}
}

func TestGateGivesUpWithoutLeader(t *testing.T) {
	g := &Gate{Self: "self:1", Wait: 50 * time.Millisecond, Poll: 10 * time.Millisecond,
		LeaderAddr: func(context.Context) (string, error) { return "", nil }}
	if code, body := do(t, g.Wrap(handler("local")), "POST", "/v1/x"); code != 503 || !strings.Contains(body, "leader_unavailable") {
		t.Fatalf("%d %q", code, body)
	}
}

func TestDeactivateWaitsForRunningRequests(t *testing.T) {
	g := &Gate{Self: "self:1", LeaderAddr: func(context.Context) (string, error) { return "", nil }, Wait: 20 * time.Millisecond, Poll: 5 * time.Millisecond}
	g.Activate()
	started, release := make(chan struct{}), make(chan struct{})
	h := g.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
	}))
	go do(t, h, "POST", "/v1/x")
	<-started
	var deactivated atomic.Bool
	go func() { g.Deactivate(); deactivated.Store(true) }()
	time.Sleep(50 * time.Millisecond)
	if deactivated.Load() {
		t.Fatal("deactivated while a leader request ran")
	}
	close(release)
	for i := 0; i < 100 && !deactivated.Load(); i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if !deactivated.Load() {
		t.Fatal("never deactivated")
	}
	if code, _ := do(t, h, "POST", "/v1/y"); code != 503 {
		t.Fatalf("after deactivation: %d", code)
	}
}
