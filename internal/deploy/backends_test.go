package deploy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
)

// countEvents counts the events of typ whose message contains substr.
func countEvents(evs []store.DeploymentEvent, typ, substr string) int {
	n := 0
	for _, e := range evs {
		if e.Type == typ && strings.Contains(e.Message, substr) {
			n++
		}
	}
	return n
}

func TestDeployProbesTheBackendBeforeTheGateway(t *testing.T) {
	te := newTestEnv(t)
	te.register("shop", "gke")
	te.start()

	// Answers at once: one probe of the health check, nothing logged.
	d := te.deployment("shop", sha("c1"), "gke")
	te.enqueue(d.ID)
	if got := te.wait(d.ID); got.Status != store.DeployHealthy {
		t.Fatalf("deployment %s %q", got.Status, got.Error)
	}
	want := probeCall{"http://shop-web.shop.gke.test:8080", "/healthz"}
	if calls := te.probe.called(); len(calls) != 1 || calls[0] != want {
		t.Errorf("probes %+v, want one %+v", calls, want)
	}
	if evs := te.events(d.ID); countEvents(evs, EventLog, "backend") != 0 {
		t.Errorf("a backend that answered at once was logged:\n%s", te.dump(d.ID))
	}

	// Already routed: the same deployment again probes again.
	d = te.deployment("shop", sha("c1"), "gke")
	te.enqueue(d.ID)
	te.wait(d.ID)
	if n := len(te.probe.called()); n != 2 || !hasEvent(te.events(d.ID), EventLog, "already sends") {
		t.Errorf("probes %d, events:\n%s", n, te.dump(d.ID))
	}
}

func TestDeployWaitsForTheBackendToAnswer(t *testing.T) {
	te := newTestEnv(t)
	var gatewayEarly atomic.Bool
	te.probe.answer = func(_ context.Context, n int) error {
		if _, ok := te.gw.app("shop"); ok {
			gatewayEarly.Store(true)
		}
		switch {
		case n <= 3:
			return errors.New("its name does not resolve: no such host")
		case n <= 5:
			return errors.New("connection refused")
		}
		return nil
	}
	te.register("shop", "gke")
	te.start()
	d := te.deployment("shop", sha("c1"), "gke")
	te.enqueue(d.ID)
	got := te.wait(d.ID)
	if got.Status != store.DeployHealthy {
		t.Fatalf("deployment %s %q\n%s", got.Status, got.Error, te.dump(d.ID))
	}
	if gatewayEarly.Load() {
		t.Error("the gateway was pointed at the backend before it answered")
	}
	if app, ok := te.gw.app("shop"); !ok || app.Backends["gke"] != "http://shop-web.shop.gke.test:8080" {
		t.Errorf("gateway app %+v", app)
	}
	evs := te.events(d.ID)
	// One line per reason, then one when it answers.
	if countEvents(evs, EventLog, "waiting for the gke backend http://shop-web.shop.gke.test:8080/healthz to answer") != 2 ||
		!hasEvent(evs, EventLog, "to answer: its name does not resolve") || !hasEvent(evs, EventLog, "to answer: connection refused") ||
		countEvents(evs, EventLog, "the gke backend http://shop-web.shop.gke.test:8080/healthz answered after") != 1 {
		t.Errorf("events:\n%s", te.dump(d.ID))
	}
	if n := len(te.probe.called()); n != 6 {
		t.Errorf("probes %d, want 6", n)
	}
}

func TestDeployFailsWhenTheBackendNeverAnswers(t *testing.T) {
	te := newTestEnv(t)
	te.cfg.SyncTimeout = 150 * time.Millisecond
	te.probe.answer = func(context.Context, int) error { return errors.New("connection refused") }
	te.register("shop", "gke")
	te.start()
	d := te.deployment("shop", sha("c1"), "gke")
	te.enqueue(d.ID)
	got := te.wait(d.ID)
	want := "the public service web did not answer at http://shop-web.shop.gke.test:8080/healthz in gke within 150ms of the commit, so the traffic gateway was not changed: connection refused"
	if got.Status != store.DeployFailed || got.Error != want {
		t.Fatalf("deployment %s %q, want error %q", got.Status, got.Error, want)
	}
	if _, ok := te.gw.app("shop"); ok || len(te.gw.puts) != 0 {
		t.Error("the gateway was changed for a backend that never answered")
	}
	evs := te.events(d.ID)
	if n := countEvents(evs, EventLog, "waiting for the gke backend"); n != 1 || len(te.probe.called()) < 3 {
		t.Errorf("%d waiting lines for %d probes, want 1:\n%s", n, len(te.probe.called()), te.dump(d.ID))
	}
}

func TestDeployProbesWithoutAHealthCheck(t *testing.T) {
	te := newTestEnv(t)
	te.register("shop", "gke")
	te.start()
	spec := strings.Replace(shopSpec, "    healthCheck: /healthz\n", "", 1)
	te.github.push("main", sha("c2"), map[string]string{"platform.yaml": spec, "Dockerfile": "x", "worker/Dockerfile": "x"})
	d := te.deployment("shop", sha("c2"), "gke")
	te.enqueue(d.ID)
	if got := te.wait(d.ID); got.Status != store.DeployHealthy {
		t.Fatalf("deployment %s %q", got.Status, got.Error)
	}
	want := probeCall{"http://shop-web.shop.gke.test:8080", ""}
	if calls := te.probe.called(); len(calls) != 1 || calls[0] != want {
		t.Errorf("probes %+v, want one %+v", calls, want)
	}
}

func TestDeploySupersededWhileWaitingForTheBackend(t *testing.T) {
	te := newTestEnv(t)
	waiting := make(chan struct{})
	te.probe.answer = func(ctx context.Context, n int) error {
		if n > 1 {
			return nil
		}
		close(waiting)
		<-ctx.Done() // the first deployment's backend answers only when it is cancelled
		return ctx.Err()
	}
	te.register("shop", "gke")
	te.start()
	old := te.deployment("shop", sha("c1"), "gke")
	te.enqueue(old.ID)
	select {
	case <-waiting:
	case <-time.After(5 * time.Second):
		t.Fatalf("the backend was never probed:\n%s", te.dump(old.ID))
	}

	newer := te.deployment("shop", sha("c1"), "gke")
	te.enqueue(newer.ID)
	if got := te.wait(newer.ID); got.Status != store.DeployHealthy {
		t.Fatalf("newer deployment %s %q", got.Status, got.Error)
	}
	waitIdle(t, te.ex)
	if got := te.get(old.ID); got.Status != store.DeploySuperseded || got.Error != "" {
		t.Errorf("old deployment %s %q", got.Status, got.Error)
	}
	if len(te.gw.puts) != 1 {
		t.Errorf("gateway puts %d, want 1 (the newer deployment's)", len(te.gw.puts))
	}
}

func TestProbeBackend(t *testing.T) {
	defer func(d time.Duration) { probeTimeout = d }(probeTimeout)
	probeTimeout = 200 * time.Millisecond

	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz":
			fmt.Fprint(w, "ok")
		case "/moved":
			http.Redirect(w, r, "http://127.0.0.1:1/elsewhere", http.StatusFound)
		case "/broken":
			w.WriteHeader(http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer app.Close()
	// A proxy whose backend does not answer.
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer proxy.Close()
	// Takes connections and never answers.
	slow, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer slow.Close()
	go func() {
		var held []net.Conn
		defer func() {
			for _, c := range held {
				c.Close()
			}
		}()
		for {
			c, err := slow.Accept()
			if err != nil {
				return
			}
			held = append(held, c)
		}
	}()
	slowURL := "http://" + slow.Addr().String()

	for _, tc := range []struct {
		name, url, healthCheck string
		want                   string // "" when it answers
	}{
		{"health check 200", app.URL, "/healthz", ""},
		{"health check redirect, not followed", app.URL, "/moved", ""},
		{"health check 500", app.URL, "/broken", "/broken answered 500 Internal Server Error"},
		{"health check 404", app.URL, "/missing", "/missing answered 404 Not Found"},
		{"no health check, 404 at /", app.URL + "/", "", ""},
		{"no health check, a proxy's 502", proxy.URL, "", "/ answered 502 Bad Gateway"},
		{"no health check, slow at / but takes connections", slowURL, "", ""},
		{"health check, slow", slowURL, "/healthz", "no answer within 200ms"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ProbeBackend(context.Background(), tc.url, tc.healthCheck)
			switch {
			case tc.want == "" && err != nil:
				t.Errorf("error %q, want an answer", err)
			case tc.want != "" && (err == nil || err.Error() != tc.want):
				t.Errorf("error %v, want %q", err, tc.want)
			}
		})
	}

	// Nothing listens.
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := closed.Addr().String()
	closed.Close()
	if err := ProbeBackend(context.Background(), "http://"+addr, ""); err == nil {
		t.Error("an address nothing listens on answered")
	}

	// Cancelled: no answer, even where a TCP connection would do.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := ProbeBackend(ctx, slowURL, ""); err == nil {
		t.Error("a cancelled probe answered")
	}
}

func TestProbeReason(t *testing.T) {
	get := func(err error) error {
		return &url.Error{Op: "Get", URL: "http://k8s-moimroom-web.elb.example:80/healthz", Err: &net.OpError{Op: "dial", Net: "tcp", Err: err}}
	}
	for _, tc := range []struct {
		err  error
		want string
	}{
		{get(&net.DNSError{Err: "no such host", Name: "k8s-moimroom-web.elb.example", IsNotFound: true}), "its name does not resolve: no such host"},
		{get(os.NewSyscallError("connect", syscall.ECONNREFUSED)), "connection refused"},
		{get(os.NewSyscallError("read", syscall.ECONNRESET)), "the connection closed without an answer"},
		{&url.Error{Op: "Get", URL: "http://x", Err: io.EOF}, "the connection closed without an answer"},
		{&url.Error{Op: "Get", URL: "http://x", Err: context.DeadlineExceeded}, "no answer within " + probeTimeout.String()},
	} {
		if got := probeReason(tc.err); got != tc.want {
			t.Errorf("probeReason(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
}
