// Package security builds the platform's security and governance view: one
// list of findings, in one schema, across the Kubernetes environments, the AWS
// account and the platform itself.
//
// Each source is read by a Collector, so adding a vendor (GCP Security Command
// Center, Cloudflare) means adding one Collector. Every finding comes from a
// real read. A source that cannot be read is reported with its status and a
// message, and nothing is filled in for it.
package security

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/envs"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

// Environments that are not registered clusters.
const (
	EnvAWSAccount = "aws-account"
	EnvPlatform   = "platform"
)

// Source identifies one source of findings.
type Source struct {
	Name        string // e.g. "kubernetes", "aws-guardduty"
	Provider    apiv1.SecurityProvider
	Environment string // an environment name, EnvAWSAccount or EnvPlatform
}

// Report is what a collector read from one source. Collectors set the
// finding fields that depend on the check (check, category, severity, title,
// resource, detail, recommendation and, when the source reports one,
// observedAt); the Service fills in the rest from Source.
type Report struct {
	Source
	Status    apiv1.SecuritySourceStatus
	Message   string
	CheckedAt time.Time // zero: when the collector returned
	Findings  []apiv1.SecurityFinding
}

// Collector reads one or more sources.
type Collector interface {
	// Sources lists the sources Collect reports on, so a collector that does
	// not answer in time still shows up.
	Sources() []Source
	// Collect reads the sources and returns one Report per source. A source
	// it could not read is reported with a status and a message. It must
	// return soon after ctx ends.
	Collect(ctx context.Context) []Report
}

// Defaults for Options.
const (
	DefaultTimeout    = 25 * time.Second
	DefaultTTL        = 60 * time.Second
	DefaultMinRefresh = 10 * time.Second
)

// Options tunes a Service. Zero values select the defaults.
type Options struct {
	// Timeout bounds one collector (default 25s).
	Timeout time.Duration
	// TTL is how long a collection is served (default 60s).
	TTL time.Duration
	// MinRefresh is the shortest time between two collections asked for with
	// refresh (default 10s).
	MinRefresh time.Duration
	Now        func() time.Time
	Log        *slog.Logger
}

// Service collects all sources concurrently and caches the result. It is
// safe for concurrent use.
type Service struct {
	collectors []Collector
	opt        Options

	mu      sync.Mutex
	latest  *apiv1.SecurityReport // nil before the first collection
	started time.Time             // when the latest collection started
	expires time.Time
	flight  *flight
}

// flight is one collection shared by every caller that asked while it ran.
type flight struct {
	done   chan struct{}
	report apiv1.SecurityReport // set before done is closed
}

// New returns a Service over collectors. It reads nothing until asked.
func New(collectors []Collector, opt Options) *Service {
	if opt.Timeout <= 0 {
		opt.Timeout = DefaultTimeout
	}
	if opt.TTL <= 0 {
		opt.TTL = DefaultTTL
	}
	if opt.MinRefresh <= 0 {
		opt.MinRefresh = DefaultMinRefresh
	}
	if opt.Now == nil {
		opt.Now = func() time.Time { return time.Now().UTC() }
	}
	if opt.Log == nil {
		opt.Log = slog.Default()
	}
	return &Service{collectors: collectors, opt: opt}
}

// Report returns the latest collection while it is younger than TTL, and
// collects otherwise. refresh collects again unless the latest collection
// started less than MinRefresh ago. Callers that ask while a collection runs
// share it. The result is unfiltered (see Filter) and must not be modified.
// The only error is ctx's.
func (s *Service) Report(ctx context.Context, refresh bool) (apiv1.SecurityReport, error) {
	s.mu.Lock()
	now := s.opt.Now()
	f := s.flight
	if f == nil {
		cached := s.latest != nil && now.Before(s.expires)
		throttled := s.latest != nil && now.Sub(s.started) < s.opt.MinRefresh
		if cached && (!refresh || throttled) {
			rep := *s.latest
			s.mu.Unlock()
			return rep, nil
		}
		f = &flight{done: make(chan struct{})}
		s.flight, s.started = f, now
		go s.collect(f)
	}
	s.mu.Unlock()

	select {
	case <-f.done:
		return f.report, nil
	case <-ctx.Done():
		// The collection keeps running for the other callers and the cache.
		return apiv1.SecurityReport{}, ctx.Err()
	}
}

// collect runs every collector concurrently, detached from the callers, and
// publishes the result.
func (s *Service) collect(f *flight) {
	generated := s.opt.Now()
	results := make([][]Report, len(s.collectors))
	var wg sync.WaitGroup
	for i, c := range s.collectors {
		wg.Go(func() { results[i] = s.run(c) })
	}
	wg.Wait()
	rep := Filter(assemble(generated, slices.Concat(results...)), "", "")

	s.mu.Lock()
	s.latest, s.expires, s.flight = &rep, s.opt.Now().Add(s.opt.TTL), nil
	s.mu.Unlock()
	f.report = rep
	close(f.done)
}

// run calls one collector within Timeout. A collector that panics or does
// not answer has its sources reported as failed instead.
func (s *Service) run(c Collector) []Report {
	ctx, cancel := context.WithTimeout(context.Background(), s.opt.Timeout)
	defer cancel()
	res := make(chan []Report, 1)
	go func() {
		defer func() {
			if v := recover(); v != nil {
				s.opt.Log.Error("security collector panicked", "sources", c.Sources(), "panic", v)
				res <- failed(c.Sources(), apiv1.SecuritySourceStatusError, "the collector failed; see the server log")
			}
		}()
		res <- c.Collect(ctx)
	}()
	var reps []Report
	select {
	case reps = <-res:
	case <-ctx.Done():
		select { // a collector that honours ctx answers right away with its own errors
		case reps = <-res:
		case <-time.After(time.Second):
		}
	}
	at := s.opt.Now()
	byName := map[Source]bool{}
	for i := range reps {
		if reps[i].CheckedAt.IsZero() {
			reps[i].CheckedAt = at
		}
		byName[reps[i].Source] = true
	}
	for _, src := range c.Sources() {
		if !byName[src] {
			reps = append(reps, Report{Source: src, Status: apiv1.SecuritySourceStatusUnavailable,
				Message: fmt.Sprintf("no answer within %s", s.opt.Timeout), CheckedAt: at})
		}
	}
	return reps
}

// failed reports every source with the same status and message.
func failed(srcs []Source, status apiv1.SecuritySourceStatus, msg string) []Report {
	out := make([]Report, len(srcs))
	for i, src := range srcs {
		out[i] = Report{Source: src, Status: status, Message: msg}
	}
	return out
}

// assemble fills in the finding fields that come from the source and builds
// the unfiltered report. Sources keep collector order; findings are sorted.
func assemble(generated time.Time, reps []Report) apiv1.SecurityReport {
	out := apiv1.SecurityReport{GeneratedAt: generated, Sources: []apiv1.SecuritySource{}, Findings: []apiv1.SecurityFinding{}}
	for _, r := range reps {
		src := apiv1.SecuritySource{Name: r.Name, Provider: r.Provider, Environment: r.Environment, Status: r.Status, CheckedAt: r.CheckedAt}
		if r.Message != "" {
			msg := r.Message
			src.Message = &msg
		}
		out.Sources = append(out.Sources, src)
		for _, f := range r.Findings {
			f.Environment, f.Provider, f.Source = r.Environment, r.Provider, r.Name
			f.Id = FindingID(r.Environment, r.Name, f.Check, f.Resource)
			if f.ObservedAt.IsZero() {
				f.ObservedAt = r.CheckedAt
			}
			out.Findings = append(out.Findings, f)
		}
	}
	slices.SortFunc(out.Findings, func(a, b apiv1.SecurityFinding) int {
		return cmp.Or(
			cmp.Compare(Rank(b.Severity), Rank(a.Severity)),
			cmp.Compare(a.Environment, b.Environment),
			cmp.Compare(a.Source, b.Source),
			cmp.Compare(a.Title, b.Title),
			cmp.Compare(a.Resource, b.Resource),
			cmp.Compare(a.Id, b.Id),
		)
	})
	return out
}

// FindingID is stable across collections. The environment is part of it, so
// the same workload in two clusters gives two findings.
func FindingID(environment, source, check, resource string) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{environment, source, check, resource}, "\x00")))
	return "sf_" + hex.EncodeToString(sum[:8])
}

var rank = map[apiv1.SecuritySeverity]int{
	apiv1.SecuritySeverityInfo: 1, apiv1.SecuritySeverityLow: 2, apiv1.SecuritySeverityMedium: 3,
	apiv1.SecuritySeverityHigh: 4, apiv1.SecuritySeverityCritical: 5,
}

// Rank orders severities: info 1 up to critical 5; unknown values are 0.
func Rank(s apiv1.SecuritySeverity) int { return rank[s] }

// Environments returns the environments that have a source in rep, in order.
func Environments(rep apiv1.SecurityReport) []string {
	var out []string
	for _, s := range rep.Sources {
		if !slices.Contains(out, s.Environment) {
			out = append(out, s.Environment)
		}
	}
	return out
}

// Filter keeps the findings of environment (all when empty) with at least
// severity atLeast (all when empty), keeps the sources of environment, and
// recomputes the source counts and the summary. It does not modify rep.
func Filter(rep apiv1.SecurityReport, environment string, atLeast apiv1.SecuritySeverity) apiv1.SecurityReport {
	out := apiv1.SecurityReport{
		GeneratedAt: rep.GeneratedAt,
		Sources:     []apiv1.SecuritySource{},
		Findings:    []apiv1.SecurityFinding{},
		Summary:     apiv1.SecuritySummary{ByEnvironment: map[string]apiv1.SeverityCounts{}},
	}
	type key struct{ env, source string }
	perSource := map[key]int{}
	for _, f := range rep.Findings {
		if (environment != "" && f.Environment != environment) || Rank(f.Severity) < Rank(atLeast) {
			continue
		}
		out.Findings = append(out.Findings, f)
		perSource[key{f.Environment, f.Source}]++
		add(&out.Summary.BySeverity, f.Severity)
		c := out.Summary.ByEnvironment[f.Environment]
		add(&c, f.Severity)
		out.Summary.ByEnvironment[f.Environment] = c
	}
	for _, s := range rep.Sources {
		if environment != "" && s.Environment != environment {
			continue
		}
		s.Findings = perSource[key{s.Environment, s.Name}]
		out.Sources = append(out.Sources, s)
		if _, ok := out.Summary.ByEnvironment[s.Environment]; !ok {
			out.Summary.ByEnvironment[s.Environment] = apiv1.SeverityCounts{}
		}
	}
	return out
}

func add(c *apiv1.SeverityCounts, s apiv1.SecuritySeverity) {
	c.Total++
	switch s {
	case apiv1.SecuritySeverityCritical:
		c.Critical++
	case apiv1.SecuritySeverityHigh:
		c.High++
	case apiv1.SecuritySeverityMedium:
		c.Medium++
	case apiv1.SecuritySeverityLow:
		c.Low++
	case apiv1.SecuritySeverityInfo:
		c.Info++
	}
}

// Config selects the collectors of a server.
type Config struct {
	// Environments are the registered clusters; each gets a Kubernetes collector.
	Environments []envs.EnvConfig
	// AWSRegion is the region the AWS account checks read (PLATFORM_AWS_REGION).
	AWSRegion string
	// Audit is the platform's audit log.
	Audit AuditReader
	// DSNHostSuffixes and GatewayConfigured describe the server's own policy.
	DSNHostSuffixes   []string
	GatewayConfigured bool
}

// Collectors returns the collectors for cfg: every environment's cluster, the
// AWS account, GCP and Cloudflare (not connected yet), and the platform.
func Collectors(cfg Config) []Collector {
	var out []Collector
	for _, e := range cfg.Environments {
		out = append(out, NewKubeCollector(e))
	}
	out = append(out,
		NewAWSCollector(cfg.AWSRegion),
		NotConfigured(Source{Name: "gcp-scc", Provider: apiv1.SecurityProviderGcp, Environment: "gcp-project"},
			"Security Command Center is not connected: this server has no GCP organization or project id and no credentials with roles/securitycenter.findingsViewer. The GKE cluster itself is checked through Kubernetes."),
		NotConfigured(Source{Name: "cloudflare", Provider: apiv1.SecurityProviderCloudflare, Environment: "cloudflare-account"},
			"Cloudflare is not connected: this server has no Cloudflare API token with Account Settings:Read and Zone WAF:Read, so the tunnel and zone settings are not checked."),
		NewPlatformCollector(cfg.Audit, cfg.DSNHostSuffixes, cfg.GatewayConfigured),
	)
	return out
}

// NotConfigured is a collector for a source this server cannot read yet. It
// reports the source as not_configured with msg, which says what is missing.
func NotConfigured(src Source, msg string) Collector { return notConfigured{src, msg} }

type notConfigured struct {
	src Source
	msg string
}

func (n notConfigured) Sources() []Source { return []Source{n.src} }

func (n notConfigured) Collect(context.Context) []Report {
	return []Report{{Source: n.src, Status: apiv1.SecuritySourceStatusNotConfigured, Message: n.msg}}
}

// many renders a count with its noun: "1 trail", "3 trails".
func many(n int, one, more string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + more
}

// shorten keeps the first line of s and caps its length, for messages built
// from provider errors.
func shorten(s string, n int) string {
	s, _, _ = strings.Cut(s, "\n")
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > n {
		s = string(r[:n]) + "..."
	}
	return s
}
