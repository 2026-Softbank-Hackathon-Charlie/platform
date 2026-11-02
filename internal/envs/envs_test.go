package envs

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

// fakeProber counts probes. With release set, Probe blocks on it and ignores
// ctx, like a hung connection.
type fakeProber struct {
	version string
	calls   atomic.Int32
	entered chan struct{} // optional, buffered: one send per call
	release chan struct{} // optional
}

func (p *fakeProber) Probe(context.Context) apiv1.EnvironmentStatus {
	p.calls.Add(1)
	if p.entered != nil {
		p.entered <- struct{}{}
	}
	if p.release != nil {
		<-p.release
	}
	v := p.version
	return apiv1.EnvironmentStatus{Reachable: true, KubernetesVersion: &v}
}

// fakeClock is a frozen clock that counts reads.
type fakeClock struct {
	mu    sync.Mutex
	t     time.Time
	calls int
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func (c *fakeClock) Calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

func testConfig() Config {
	return Config{Environments: []EnvConfig{
		{Name: "gke", Provider: "gcp", DisplayName: "GCP GKE", Region: "asia-northeast3", KubeContext: "gke-hopcharlie"},
		{Name: "eks", Provider: "aws", DisplayName: "AWS EKS", Region: "ap-northeast-2", KubeContext: "eks-hopcharlie"},
		{Name: "onprem", Provider: "onprem", KubeContext: "onprem-hopcharlie", GatewayBackend: "k3s"},
	}}
}

func newTestService(t *testing.T, probers map[string]*fakeProber, opt Options) (*Service, *fakeClock) {
	t.Helper()
	clock := &fakeClock{t: time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)}
	opt.Now = clock.Now
	opt.NewProber = func(c EnvConfig) (Prober, error) { return probers[c.Name], nil }
	s, err := New(testConfig(), opt)
	if err != nil {
		t.Fatal(err)
	}
	return s, clock
}

func threeProbers() map[string]*fakeProber {
	return map[string]*fakeProber{
		"gke":    {version: "v1.31.4-gke.1086000"},
		"eks":    {version: "v1.30.6-eks-7f9249a"},
		"onprem": {version: "v1.32.1+k3s1"},
	}
}

func TestListInConfigOrder(t *testing.T) {
	probers := threeProbers()
	s, clock := newTestService(t, probers, Options{})

	got := s.List(context.Background())
	if len(got) != 3 {
		t.Fatalf("got %d environments", len(got))
	}
	for i, name := range []string{"gke", "eks", "onprem"} {
		e := got[i]
		if e.Name != name || deref(e.Status.KubernetesVersion) != probers[name].version {
			t.Errorf("[%d] = %s %s, want %s %s", i, e.Name, deref(e.Status.KubernetesVersion), name, probers[name].version)
		}
		if e.Live {
			t.Errorf("%s: live = true, want false (the API layer sets it)", name)
		}
		if !e.Status.CheckedAt.Equal(clock.t) {
			t.Errorf("%s: checkedAt = %v, want %v", name, e.Status.CheckedAt, clock.t)
		}
	}
	eks, onprem := got[1], got[2]
	if eks.Provider != apiv1.ProviderAws || eks.DisplayName != "AWS EKS" || deref(eks.Region) != "ap-northeast-2" ||
		deref(eks.KubeContext) != "eks-hopcharlie" || eks.GatewayBackend != "eks" {
		t.Errorf("eks = %+v", eks)
	}
	if onprem.Provider != apiv1.ProviderOnprem || onprem.DisplayName != "onprem" || onprem.Region != nil || onprem.GatewayBackend != "k3s" {
		t.Errorf("onprem = %+v", onprem)
	}
	if got := strings.Join(s.Names(), ","); got != "gke,eks,onprem" {
		t.Errorf("Names() = %s", got)
	}
}

func TestStatusIsCachedForTTL(t *testing.T) {
	probers := threeProbers()
	s, clock := newTestService(t, probers, Options{CacheTTL: 5 * time.Second})
	ctx := context.Background()

	first := s.List(ctx)
	clock.Advance(4 * time.Second)
	s.List(ctx)
	if _, err := s.Get(ctx, "gke"); err != nil {
		t.Fatal(err)
	}
	for name, p := range probers {
		if n := p.calls.Load(); n != 1 {
			t.Errorf("%s probed %d times within the TTL, want 1", name, n)
		}
	}

	clock.Advance(time.Second) // exactly at expiry
	again, _ := s.Get(ctx, "gke")
	if n := probers["gke"].calls.Load(); n != 2 {
		t.Fatalf("gke probed %d times after the TTL, want 2", n)
	}
	if !again.Status.CheckedAt.After(first[0].Status.CheckedAt) {
		t.Errorf("checkedAt %v not after %v", again.Status.CheckedAt, first[0].Status.CheckedAt)
	}
	if n := probers["eks"].calls.Load(); n != 1 {
		t.Errorf("eks probed %d times, want 1 (only gke was asked)", n)
	}
}

func TestConcurrentCallersShareOneProbe(t *testing.T) {
	gke := &fakeProber{version: "v1.31.4", entered: make(chan struct{}, 16), release: make(chan struct{})}
	s, clock := newTestService(t, map[string]*fakeProber{"gke": gke}, Options{})

	const callers = 8
	results := make(chan apiv1.Environment, callers)
	for range callers {
		go func() {
			e, _ := s.Get(context.Background(), "gke")
			results <- e
		}()
	}
	<-gke.entered
	// Each caller reads the clock once, under the env lock, before joining the
	// flight. Once all have read it, all wait on the same probe.
	deadline := time.Now().Add(5 * time.Second)
	for clock.Calls() < callers {
		if time.Now().After(deadline) {
			t.Fatalf("only %d of %d callers arrived", clock.Calls(), callers)
		}
		time.Sleep(time.Millisecond)
	}
	clock.Advance(time.Hour) // a second probe would now be needed; dedup must avoid it
	close(gke.release)

	for range callers {
		if e := <-results; !e.Status.Reachable {
			t.Errorf("caller got %+v", e.Status)
		}
	}
	if n := gke.calls.Load(); n != 1 {
		t.Fatalf("%d probes for %d concurrent callers, want 1", n, callers)
	}
}

func TestSlowEnvironmentTimesOut(t *testing.T) {
	probers := threeProbers()
	probers["eks"].release = make(chan struct{})
	t.Cleanup(func() { close(probers["eks"].release) })
	s, _ := newTestService(t, probers, Options{ProbeTimeout: 100 * time.Millisecond})

	start := time.Now()
	got := s.List(context.Background())
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("List took %v with a 100ms probe timeout", d)
	}
	if !got[0].Status.Reachable || !got[2].Status.Reachable {
		t.Errorf("gke/onprem delayed or failed by eks: %+v %+v", got[0].Status, got[2].Status)
	}
	eks := got[1].Status
	if eks.Reachable || deref(eks.Error) != "no answer within 100ms" || eks.KubernetesVersion != nil || eks.CheckedAt.IsZero() {
		t.Fatalf("eks status = %+v error=%q, want unreachable with a timeout error", eks, deref(eks.Error))
	}

	// The timeout is cached like any result: no new probe piles up.
	start = time.Now()
	again, _ := s.Get(context.Background(), "eks")
	if time.Since(start) > 50*time.Millisecond || again.Status.Reachable || probers["eks"].calls.Load() != 1 {
		t.Fatalf("second Get = %+v, calls=%d, want the cached timeout", again.Status, probers["eks"].calls.Load())
	}
}

func TestCallerCancelDoesNotFailSharedProbe(t *testing.T) {
	gke := &fakeProber{version: "v1.31.4", entered: make(chan struct{}, 4), release: make(chan struct{})}
	s, _ := newTestService(t, map[string]*fakeProber{"gke": gke}, Options{})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan apiv1.Environment, 1)
	go func() {
		e, _ := s.Get(ctx, "gke")
		done <- e
	}()
	<-gke.entered
	cancel()
	e := <-done
	if e.Status.Reachable || !strings.HasPrefix(deref(e.Status.Error), "status check cancelled") || e.Status.CheckedAt.IsZero() {
		t.Fatalf("cancelled caller got %+v error=%q", e.Status, deref(e.Status.Error))
	}

	close(gke.release)
	e, _ = s.Get(context.Background(), "gke")
	if !e.Status.Reachable || gke.calls.Load() != 1 {
		t.Fatalf("after release: %+v, calls=%d, want the original probe's result", e.Status, gke.calls.Load())
	}
}

func TestProberBuildFailureIsReportedAndRetried(t *testing.T) {
	var builds atomic.Int32
	clock := &fakeClock{t: time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)}
	s, err := New(testConfig(), Options{Now: clock.Now, NewProber: func(c EnvConfig) (Prober, error) {
		if builds.Add(1) == 1 {
			return nil, errors.New(`kube context "gke-hopcharlie" not found in kubeconfig`)
		}
		return &fakeProber{version: "v1.31.4"}, nil
	}})
	if err != nil {
		t.Fatalf("New failed before any cluster exists: %v", err)
	}
	e, _ := s.Get(context.Background(), "gke")
	if e.Status.Reachable || deref(e.Status.Error) != `kube context "gke-hopcharlie" not found in kubeconfig` || e.Status.CheckedAt.IsZero() {
		t.Fatalf("status = %+v error=%q", e.Status, deref(e.Status.Error))
	}
	clock.Advance(DefaultCacheTTL)
	e, _ = s.Get(context.Background(), "gke")
	if !e.Status.Reachable {
		t.Fatalf("after the kubeconfig is fixed: %+v", e.Status)
	}
	clock.Advance(DefaultCacheTTL)
	s.Get(context.Background(), "gke")
	if n := builds.Load(); n != 2 {
		t.Fatalf("built %d times, want 2 (a built prober is reused)", n)
	}
}

func TestGetUnknownEnvironment(t *testing.T) {
	s, _ := newTestService(t, threeProbers(), Options{})
	if _, err := s.Get(context.Background(), "aks"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestDefaultProberReportsKubeconfigProblems(t *testing.T) {
	srv := fakeAPIServer(t, "good-token")
	kubeconfig := writeKubeconfig(t, srv, map[string]string{"onprem-hopcharlie": "good-token"})
	cfg := Config{Environments: []EnvConfig{
		{Name: "onprem", Provider: "onprem", KubeContext: "onprem-hopcharlie", Kubeconfig: kubeconfig},
		{Name: "gke", Provider: "gcp", KubeContext: "gke-hopcharlie", Kubeconfig: kubeconfig},
	}}
	s, err := New(cfg, Options{ProbeTimeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	got := s.List(context.Background())
	if !got[0].Status.Reachable || got[0].Status.Nodes == nil || got[0].Status.Nodes.Total != 2 {
		t.Errorf("onprem = %+v error=%q", got[0].Status, deref(got[0].Status.Error))
	}
	gke := got[1].Status
	if gke.Reachable || deref(gke.Error) != `kube context "gke-hopcharlie" not found in kubeconfig` {
		t.Errorf("gke = %+v error=%q", gke, deref(gke.Error))
	}
	assertNoSecrets(t, deref(gke.Error), kubeconfig, "good-token")
}
