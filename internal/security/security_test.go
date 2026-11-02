package security

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

// fakeCollector is a test collector that counts its calls.
type fakeCollector struct {
	srcs  []Source
	calls atomic.Int32
	fn    func(ctx context.Context) []Report
}

func (f *fakeCollector) Sources() []Source { return f.srcs }

func (f *fakeCollector) Collect(ctx context.Context) []Report {
	f.calls.Add(1)
	return f.fn(ctx)
}

// clock is a settable test clock.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

var t0 = time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)

func finding(check string, sev apiv1.SecuritySeverity, resource string) apiv1.SecurityFinding {
	return apiv1.SecurityFinding{Check: check, Severity: sev, Category: apiv1.SecurityCategoryWorkload, Title: check, Resource: resource}
}

func TestReportCachesAndThrottlesRefresh(t *testing.T) {
	clk := &clock{t: t0}
	src := Source{Name: "kubernetes", Provider: apiv1.SecurityProviderGcp, Environment: "gke"}
	c := &fakeCollector{srcs: []Source{src}, fn: func(context.Context) []Report {
		return []Report{{Source: src, Status: apiv1.SecuritySourceStatusOk, Findings: []apiv1.SecurityFinding{finding("k8s.privileged", apiv1.SecuritySeverityHigh, "shop/Deployment/web/app")}}}
	}}
	s := New([]Collector{c}, Options{Now: clk.now})
	ctx := t.Context()

	rep, err := s.Report(ctx, false)
	if err != nil || len(rep.Findings) != 1 || c.calls.Load() != 1 {
		t.Fatalf("first report: %v %+v calls=%d", err, rep, c.calls.Load())
	}
	f := rep.Findings[0]
	if f.Environment != "gke" || f.Source != "kubernetes" || f.Provider != apiv1.SecurityProviderGcp || f.ObservedAt.IsZero() ||
		f.Id != FindingID("gke", "kubernetes", "k8s.privileged", "shop/Deployment/web/app") {
		t.Fatalf("finding not filled from its source: %+v", f)
	}
	if rep.Sources[0].Findings != 1 || rep.Summary.BySeverity.High != 1 || rep.Summary.ByEnvironment["gke"].Total != 1 {
		t.Fatalf("counts: %+v %+v", rep.Sources, rep.Summary)
	}

	clk.add(5 * time.Second)
	if _, _ = s.Report(ctx, false); c.calls.Load() != 1 {
		t.Fatal("cached report was collected again")
	}
	if _, _ = s.Report(ctx, true); c.calls.Load() != 1 {
		t.Fatal("refresh within 10s collected again")
	}
	clk.add(6 * time.Second)
	if _, _ = s.Report(ctx, true); c.calls.Load() != 2 {
		t.Fatalf("refresh after 11s: calls=%d", c.calls.Load())
	}
	clk.add(59 * time.Second)
	if _, _ = s.Report(ctx, false); c.calls.Load() != 2 {
		t.Fatal("collected again before the TTL")
	}
	clk.add(2 * time.Second)
	if _, _ = s.Report(ctx, false); c.calls.Load() != 3 {
		t.Fatalf("not collected after the TTL: calls=%d", c.calls.Load())
	}
}

func TestConcurrentCallersShareOneCollection(t *testing.T) {
	release := make(chan struct{})
	src := Source{Name: "x", Provider: apiv1.SecurityProviderPlatform, Environment: "platform"}
	c := &fakeCollector{srcs: []Source{src}, fn: func(context.Context) []Report {
		<-release
		return []Report{{Source: src, Status: apiv1.SecuritySourceStatusOk}}
	}}
	s := New([]Collector{c}, Options{})
	var wg sync.WaitGroup
	for range 5 {
		wg.Go(func() { _, _ = s.Report(context.Background(), true) })
	}
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()
	if n := c.calls.Load(); n != 1 {
		t.Fatalf("%d collections for 5 concurrent callers, want 1", n)
	}

	// A caller that gives up gets its context error; the collection goes on.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := New([]Collector{c}, Options{}).Report(ctx, false); err == nil {
		t.Fatal("cancelled caller got no error")
	}
}

func TestSlowPanickingAndSilentCollectors(t *testing.T) {
	slow := Source{Name: "slow", Provider: apiv1.SecurityProviderAws, Environment: EnvAWSAccount}
	boom := Source{Name: "boom", Provider: apiv1.SecurityProviderAws, Environment: EnvAWSAccount}
	quiet := Source{Name: "quiet", Provider: apiv1.SecurityProviderPlatform, Environment: EnvPlatform}
	block := make(chan struct{})
	defer close(block)
	s := New([]Collector{
		&fakeCollector{srcs: []Source{slow}, fn: func(context.Context) []Report { <-block; return nil }}, // ignores ctx
		&fakeCollector{srcs: []Source{boom}, fn: func(context.Context) []Report { panic("bug") }},
		&fakeCollector{srcs: []Source{quiet}, fn: func(context.Context) []Report { return nil }}, // forgets its source
	}, Options{Timeout: 20 * time.Millisecond, Log: slog.New(slog.DiscardHandler)})
	rep, err := s.Report(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]apiv1.SecuritySourceStatus{
		"slow": apiv1.SecuritySourceStatusUnavailable, "boom": apiv1.SecuritySourceStatusError, "quiet": apiv1.SecuritySourceStatusUnavailable,
	}
	if len(rep.Sources) != 3 {
		t.Fatalf("sources: %+v", rep.Sources)
	}
	for _, src := range rep.Sources {
		if src.Status != want[src.Name] || src.Message == nil || *src.Message == "" {
			t.Errorf("source %s: status %s, message %v; want %s with a message", src.Name, src.Status, src.Message, want[src.Name])
		}
	}
}

func TestFilter(t *testing.T) {
	gke := Source{Name: "kubernetes", Provider: apiv1.SecurityProviderGcp, Environment: "gke"}
	aws := Source{Name: "aws-iam", Provider: apiv1.SecurityProviderAws, Environment: EnvAWSAccount}
	cfg := Source{Name: "platform-config", Provider: apiv1.SecurityProviderPlatform, Environment: EnvPlatform}
	rep := Filter(assemble(t0, []Report{
		{Source: gke, Status: apiv1.SecuritySourceStatusOk, CheckedAt: t0, Findings: []apiv1.SecurityFinding{
			finding("a", apiv1.SecuritySeverityLow, "r1"), finding("b", apiv1.SecuritySeverityHigh, "r2"),
		}},
		{Source: aws, Status: apiv1.SecuritySourceStatusOk, CheckedAt: t0, Findings: []apiv1.SecurityFinding{
			finding("c", apiv1.SecuritySeverityCritical, "r3"), finding("d", apiv1.SecuritySeverityInfo, "r4"),
		}},
		{Source: cfg, Status: apiv1.SecuritySourceStatusOk, CheckedAt: t0},
	}), "", "")

	var order []string
	for _, f := range rep.Findings {
		order = append(order, f.Check)
	}
	if got := join(order); got != "c,b,a,d" {
		t.Fatalf("order %s, want most severe first", got)
	}
	if e := rep.Summary.ByEnvironment[EnvPlatform]; e.Total != 0 {
		t.Fatalf("platform counts %+v", e)
	}
	if _, ok := rep.Summary.ByEnvironment[EnvPlatform]; !ok {
		t.Fatal("an environment without findings is missing from the summary")
	}

	high := Filter(rep, "", apiv1.SecuritySeverityHigh)
	if len(high.Findings) != 2 || high.Summary.BySeverity.Total != 2 || high.Sources[0].Findings != 1 || high.Sources[1].Findings != 1 {
		t.Fatalf("severity filter: %+v", high)
	}
	only := Filter(rep, "gke", "")
	if len(only.Findings) != 2 || len(only.Sources) != 1 || len(only.Summary.ByEnvironment) != 1 || only.Summary.BySeverity.Low != 1 {
		t.Fatalf("environment filter: %+v", only)
	}
	if len(rep.Findings) != 4 {
		t.Fatal("Filter modified its input")
	}
	if got := join(Environments(rep)); got != "gke,aws-account,platform" {
		t.Fatalf("environments %s", got)
	}
}

func join(ss []string) string {
	out := ""
	for i, s := range ss {
		if i > 0 {
			out += ","
		}
		out += s
	}
	return out
}

func TestNotConfigured(t *testing.T) {
	src := Source{Name: "gcp-scc", Provider: apiv1.SecurityProviderGcp, Environment: "gcp-project"}
	reps := NotConfigured(src, "needs a project id").Collect(t.Context())
	if len(reps) != 1 || reps[0].Status != apiv1.SecuritySourceStatusNotConfigured || reps[0].Message != "needs a project id" || len(reps[0].Findings) != 0 {
		t.Fatalf("%+v", reps)
	}
}
