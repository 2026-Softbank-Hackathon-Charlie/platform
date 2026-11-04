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
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/gateway"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/appspec"
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

	// Already routed to the same URL (a redeploy): not probed again.
	d = te.deployment("shop", sha("c1"), "gke")
	te.enqueue(d.ID)
	if got := te.wait(d.ID); got.Status != store.DeployHealthy {
		t.Fatalf("second deployment %s %q", got.Status, got.Error)
	}
	if n := len(te.probe.called()); n != 1 || !hasEvent(te.events(d.ID), EventLog, "already sends") {
		t.Errorf("probes %d, want 1; events:\n%s", n, te.dump(d.ID))
	}
}

func TestDeployWaitsForTheBackendToAnswer(t *testing.T) {
	te := newTestEnv(t)
	var gatewayEarly atomic.Bool
	te.probe.answer = func(_ context.Context, _ string, n int) error {
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
	te.probe.answer = func(context.Context, string, int) error { return errors.New("connection refused") }
	te.register("shop", "gke")
	te.start()
	d := te.deployment("shop", sha("c1"), "gke")
	te.enqueue(d.ID)
	got := te.wait(d.ID)
	want := "the public service web did not answer within 150ms of the commit at http://shop-web.shop.gke.test:8080/healthz in gke (connection refused), so the traffic gateway was not changed"
	if got.Status != store.DeployFailed || got.Error != want {
		t.Fatalf("deployment %s %q, want error %q", got.Status, got.Error, want)
	}
	if e := got.Environments[0]; e.Status != EnvFailed || !strings.Contains(e.Message, "did not answer") || e.URL != "" {
		t.Errorf("environment %+v", e)
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
	te.probe.answer = func(ctx context.Context, _ string, n int) error {
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
	err = ProbeBackend(context.Background(), "http://"+addr, "")
	if err == nil || err.Error() == "" || strings.Contains(err.Error(), addr) {
		t.Errorf("an address nothing listens on: %v, want a reason without the address", err)
	}
	// Windows says it in its own words (WSAECONNREFUSED).
	if runtime.GOOS != "windows" && (err == nil || err.Error() != "connection refused") {
		t.Errorf("an address nothing listens on: %v, want connection refused", err)
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
		// Unknown: the innermost error, without the addresses.
		{get(os.NewSyscallError("connect", syscall.EHOSTUNREACH)), syscall.EHOSTUNREACH.Error()},
		{&url.Error{Op: "Get", URL: "http://x", Err: errors.New("malformed HTTP response")}, "malformed HTTP response"},
	} {
		if got := probeReason(tc.err); got != tc.want {
			t.Errorf("probeReason(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
}

func TestDeployLeavesOutABackendWithoutTraffic(t *testing.T) {
	te := newTestEnv(t)
	te.cfg.SyncTimeout = time.Minute // not waited for: gke, which gets the requests, answers at once
	var eksUp atomic.Bool
	te.probe.answer = func(_ context.Context, u string, _ int) error {
		if strings.Contains(u, ".eks.") && !eksUp.Load() {
			return errors.New("connection refused")
		}
		return nil
	}
	te.register("shop", "gke", "eks") // a new app writes to gke, its first environment
	te.start()

	// eks never answers: gke is routed, eks is left out, and the deployment
	// is healthy.
	start := time.Now()
	d := te.deployment("shop", sha("c1"), "gke", "eks")
	te.enqueue(d.ID)
	got := te.wait(d.ID)
	if got.Status != store.DeployHealthy {
		t.Fatalf("deployment %s %q\n%s", got.Status, got.Error, te.dump(d.ID))
	}
	if took := time.Since(start); took > 20*time.Second {
		t.Errorf("took %s: waited for eks, which gets no requests, until the deadline", took)
	}
	if app, ok := te.gw.app("shop"); !ok || app.WriteBackend != "gke" || len(app.Backends) != 1 || app.Backends["gke"] == "" || len(te.gw.puts) != 1 {
		t.Errorf("gateway app %+v after %d puts", app, len(te.gw.puts))
	}
	for _, e := range got.Environments {
		switch {
		case e.Env == "gke" && (e.Status != EnvHealthy || e.URL == ""),
			e.Env == "eks" && (e.Status != EnvHealthy || e.URL != "" || !strings.Contains(e.Message, "had not answered at http://shop-web.shop.eks.test:8080/healthz (connection refused)") || !strings.Contains(e.Message, "was not pointed at it")):
			t.Errorf("environment %+v", e)
		}
	}
	evs := te.events(d.ID)
	if !hasEvent(evs, EventLog, "left out of the traffic gateway, as the public service did not answer there: eks") || !hasEvent(evs, EventDone, "leaves out eks") {
		t.Errorf("events:\n%s", te.dump(d.ID))
	}

	// eks answers now: only eks is probed (gke already has its URL), and
	// added next to gke.
	eksUp.Store(true)
	before := len(te.probe.called())
	d = te.deployment("shop", sha("c1"), "gke", "eks")
	te.enqueue(d.ID)
	if got := te.wait(d.ID); got.Status != store.DeployHealthy {
		t.Fatalf("second deployment %s %q", got.Status, got.Error)
	}
	if calls := te.probe.called()[before:]; len(calls) != 1 || calls[0].url != "http://shop-web.shop.eks.test:8080" {
		t.Errorf("probes %+v, want eks only", calls)
	}
	if app, _ := te.gw.app("shop"); app.WriteBackend != "gke" || len(app.Backends) != 2 {
		t.Errorf("gateway app %+v", app)
	}
}

func TestDeployFailsWhenTheTrafficBackendDoesNotAnswer(t *testing.T) {
	te := newTestEnv(t)
	te.cfg.SyncTimeout = 150 * time.Millisecond
	te.probe.answer = func(_ context.Context, u string, _ int) error {
		if strings.Contains(u, ".gke.") {
			return errors.New("connection refused")
		}
		return nil
	}
	te.register("shop", "gke", "eks")
	te.start()
	d := te.deployment("shop", sha("c1"), "gke", "eks")
	te.enqueue(d.ID)
	got := te.wait(d.ID)
	want := "the public service web did not answer within 150ms of the commit at http://shop-web.shop.gke.test:8080/healthz in gke (connection refused), so the traffic gateway was not changed"
	if got.Status != store.DeployFailed || got.Error != want {
		t.Fatalf("deployment %s %q, want error %q", got.Status, got.Error, want)
	}
	if _, ok := te.gw.app("shop"); ok || len(te.gw.puts) != 0 {
		t.Error("the gateway was changed though gke, which takes the writes, never answered")
	}
	for _, e := range got.Environments {
		if e.Env == "gke" && e.Status != EnvFailed || e.Env == "eks" && e.Status != EnvHealthy {
			t.Errorf("environment %+v", e)
		}
	}
}

func TestWaitForBackendsStopsAtTheDeadline(t *testing.T) {
	te := newTestEnv(t)
	te.cfg.PollInterval = time.Hour
	te.probe.answer = func(context.Context, string, int) error { return errors.New("connection refused") }
	te.register("shop", "gke")
	ex, err := New(te.cfg)
	if err != nil {
		t.Fatal(err)
	}
	d := te.deployment("shop", sha("c1"), "gke")
	svc := appspec.Service{Name: "web", Port: 8080, HealthCheck: "/healthz"}
	urls := map[string]string{"gke": "http://shop-web.shop.gke.test:8080"}
	wait := func(ctx context.Context, deadline time.Time) (map[string]string, error) {
		j := &job{e: ex, d: d, syncDeadline: deadline}
		return j.waitForBackends(ctx, []string{"gke"}, []string{"gke"}, urls, svc)
	}

	// Syncing used up the deadline: probed once all the same.
	silent, err := wait(context.Background(), time.Now().Add(-time.Second))
	if err != nil || silent["gke"] != "connection refused" || len(te.probe.called()) != 1 {
		t.Errorf("silent %v, err %v, probes %d; want gke silent after one probe", silent, err, len(te.probe.called()))
	}

	// A little time left: the wait between probes ends at the deadline,
	// not after PollInterval.
	start := time.Now()
	silent, err = wait(context.Background(), start.Add(100*time.Millisecond))
	if err != nil || len(silent) != 1 || len(te.probe.called()) != 3 || time.Since(start) > 5*time.Second {
		t.Errorf("silent %v, err %v, probes %d after %s; want 2 more probes, ending at the deadline", silent, err, len(te.probe.called()), time.Since(start))
	}

	// Cancelled while waiting between probes.
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	start = time.Now()
	if _, err := wait(ctx, start.Add(time.Hour)); !errors.Is(err, context.Canceled) || time.Since(start) > 5*time.Second {
		t.Errorf("err %v after %s, want context.Canceled at once", err, time.Since(start))
	}
}

func TestDeployFailsWhenAReadBackendDoesNotAnswer(t *testing.T) {
	te := newTestEnv(t)
	te.cfg.SyncTimeout = 150 * time.Millisecond
	te.probe.answer = func(_ context.Context, u string, _ int) error {
		if strings.Contains(u, ".eks.") {
			return errors.New("connection refused")
		}
		return nil
	}
	te.register("shop", "gke", "eks")
	// A migration sends half the reads to eks, whose Service has moved.
	before := gateway.AppInfo{Name: "shop", Backends: map[string]string{"gke": "http://shop-web.shop.gke.test:8080", "eks": "http://old-eks"}, WriteBackend: "gke", ReadWeights: map[string]int{"gke": 50, "eks": 50}}
	te.gw.apps["shop"] = before
	te.start()
	d := te.deployment("shop", sha("c1"), "gke", "eks")
	te.enqueue(d.ID)
	got := te.wait(d.ID)
	if got.Status != store.DeployFailed || !strings.Contains(got.Error, "in eks (connection refused), so the traffic gateway was not changed") {
		t.Fatalf("deployment %s %q", got.Status, got.Error)
	}
	if calls := te.probe.called(); len(calls) < 1 || slices.ContainsFunc(calls, func(c probeCall) bool { return strings.Contains(c.url, ".gke.") }) {
		t.Errorf("probes %+v, want eks only (gke is routed at the same URL)", calls)
	}
	if app, _ := te.gw.app("shop"); len(te.gw.puts) != 0 || app.Backends["eks"] != "http://old-eks" {
		t.Errorf("gateway app %+v after %d puts", app, len(te.gw.puts))
	}
}

func TestDeployKeepsTheOldRouteOfABackendWithoutTraffic(t *testing.T) {
	te := newTestEnv(t)
	te.probe.answer = func(_ context.Context, u string, _ int) error {
		if strings.Contains(u, ".eks.") {
			return errors.New("connection refused")
		}
		return nil
	}
	te.register("shop", "gke", "eks")
	te.gw.apps["shop"] = gateway.AppInfo{Name: "shop", Backends: map[string]string{"gke": "http://shop-web.shop.gke.test:8080", "eks": "http://old-eks"}, WriteBackend: "gke"}
	te.start()
	d := te.deployment("shop", sha("c1"), "gke", "eks")
	te.enqueue(d.ID)
	if got := te.wait(d.ID); got.Status != store.DeployHealthy {
		t.Fatalf("deployment %s %q", got.Status, got.Error)
	}
	if app, _ := te.gw.app("shop"); app.Backends["eks"] != "http://old-eks" || app.WriteBackend != "gke" {
		t.Errorf("gateway app %+v, want eks's old route kept", app)
	}
}

func TestDeployProbesTheEnvironmentsAtOnce(t *testing.T) {
	te := newTestEnv(t)
	var arrived sync.WaitGroup
	arrived.Add(2)
	both := make(chan struct{})
	go func() { arrived.Wait(); close(both) }()
	te.probe.answer = func(_ context.Context, _ string, n int) error {
		if n > 2 {
			return nil
		}
		arrived.Done()
		select {
		case <-both:
			return nil
		case <-time.After(2 * time.Second):
			return errors.New("probed one at a time")
		}
	}
	te.register("shop", "gke", "eks")
	te.start()
	d := te.deployment("shop", sha("c1"), "gke", "eks")
	te.enqueue(d.ID)
	if got := te.wait(d.ID); got.Status != store.DeployHealthy {
		t.Fatalf("deployment %s %q", got.Status, got.Error)
	}
	if n := countEvents(te.events(d.ID), EventLog, "waiting for the"); n != 0 || len(te.probe.called()) != 2 {
		t.Errorf("%d waiting lines, %d probes, want both probed at once:\n%s", n, len(te.probe.called()), te.dump(d.ID))
	}
}

func TestDeployWritesHeldFailsBeforeProbing(t *testing.T) {
	te := newTestEnv(t)
	te.register("shop", "gke")
	te.gw.apps["shop"] = gateway.AppInfo{Name: "shop", Backends: map[string]string{"gke": "http://old"}, WriteBackend: "gke", WritesHeld: true}
	te.start()
	d := te.deployment("shop", sha("c1"), "gke")
	te.enqueue(d.ID)
	if got := te.wait(d.ID); got.Status != store.DeployFailed || !strings.Contains(got.Error, "holds shop's writes") {
		t.Fatalf("deployment %s %q", got.Status, got.Error)
	}
	if n := len(te.probe.called()); n != 0 {
		t.Errorf("probes %d, want none while writes are held", n)
	}
}

func TestReceivers(t *testing.T) {
	for _, tc := range []struct {
		spec gateway.AppSpec
		want []string
	}{
		{gateway.AppSpec{WriteBackend: "gke"}, []string{"gke"}},
		{gateway.AppSpec{WriteBackend: "gke", ReadWeights: map[string]int{"gke": 100, "eks": 0}}, []string{"gke"}},
		{gateway.AppSpec{WriteBackend: "gke", ReadWeights: map[string]int{"gke": 0, "eks": 100}}, []string{"eks", "gke"}},
		{gateway.AppSpec{WriteBackend: "gke", ReadWeights: map[string]int{"eks": 30, "onprem": 70}}, []string{"eks", "gke", "onprem"}},
	} {
		got := receivers(tc.spec)
		slices.Sort(got)
		if !slices.Equal(got, tc.want) {
			t.Errorf("receivers(%+v) = %v, want %v", tc.spec, got, tc.want)
		}
	}
}
