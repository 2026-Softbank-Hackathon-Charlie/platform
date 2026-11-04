package bucketprobe

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/preflight"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/preflight/residency"
)

// DefaultTimeout bounds one whole probe of one bucket.
const DefaultTimeout = 30 * time.Second

// maxListPages bounds a prefix listing: objects directly under the path are
// paged through too, and a listing this long is reported, not truncated.
const maxListPages = 200

// Result explains how complete an observation is.
type Result struct {
	Code string // empty when the observation is complete
}

// Options selects the parts of a probe. Location and endpoint are always read.
type Options struct {
	Prefixes    bool // list first-level prefixes (the copy source)
	Replication bool // read replication rules (the copy target)
}

// Resolver resolves an endpoint host.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// Prober measures buckets. The zero value reads the process environment,
// resolves with net.DefaultResolver and uses http.DefaultClient's transport.
type Prober struct {
	Getenv     func(string) string
	Resolver   Resolver
	HTTPClient *http.Client
	Timeout    time.Duration
	// gcsBase overrides https://storage.googleapis.com (tests).
	gcsBase string
}

// store is one backend's metadata calls.
type store interface {
	// location returns the lowercase location and its type (region,
	// dual-region, multi-region; "" when the store has no such notion).
	location(ctx context.Context) (string, string, error)
	// endpointHost is the host the copy connects to.
	endpointHost() string
	// prefixes lists first-level prefixes under path ("" for direct objects).
	prefixes(ctx context.Context, path string) ([]string, string)
	replicas(ctx context.Context) ([]string, error)
	tiered(ctx context.Context) (bool, error)
}

// Probe measures bucket ("remote:bucket[/path]"). The observation is
// complete only when Result.Code is empty; otherwise it must not be used.
func (p Prober) Probe(ctx context.Context, bucket string, opt Options) (preflight.BucketObservation, Result) {
	obs := preflight.BucketObservation{EndpointIPs: []string{}, Prefixes: []string{}, Replicas: []string{}}
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	fail := func(code string) (preflight.BucketObservation, Result) {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			code = CodeTimeout
		}
		return preflight.BucketObservation{EndpointIPs: []string{}, Prefixes: []string{}, Replicas: []string{}}, Result{Code: code}
	}

	getenv := p.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	r, code := readRemote(getenv, bucket)
	if code != "" {
		return fail(code)
	}
	hc := p.HTTPClient
	if hc == nil {
		hc = &http.Client{}
	}
	var s store
	switch r.typ {
	case typeS3:
		st, code := newS3(ctx, r, hc)
		if code != "" {
			return fail(code)
		}
		s = st
	case typeGCS:
		if r.endpoint != "" {
			if host, _, ok := endpointHost(r.endpoint); !ok || host != gcsHost {
				return fail(CodeUnsupportedBackend)
			}
		}
		st, code := newGCS(ctx, r, hc, p.gcsBase)
		if code != "" {
			return fail(code)
		}
		s = st
	default:
		return fail(CodeUnsupportedBackend)
	}

	obs.Hosting = preflight.HostingSelf
	if r.providerHosted() {
		obs.Hosting = preflight.HostingProvider
	}
	loc, locType, err := s.location(ctx)
	if err != nil {
		return fail(CodeLocationFailed)
	}
	switch {
	case obs.Hosting == preflight.HostingProvider:
		if !residency.ValidLocation(loc) || locType == "" {
			return fail(CodeLocationFailed)
		}
		obs.Location, obs.LocationType = loc, locType
	case residency.ValidLocation(loc):
		// A self-hosted store's region is recorded as evidence only.
		obs.Location, obs.LocationType = loc, "region"
	}

	ips, code := p.resolve(ctx, s.endpointHost())
	if code != "" {
		return fail(code)
	}
	obs.EndpointIPs = ips

	if opt.Prefixes {
		prefixes, code := s.prefixes(ctx, r.path)
		if code != "" {
			return fail(code)
		}
		obs.Prefixes = prefixes
	}
	if opt.Replication {
		replicas, err := s.replicas(ctx)
		if err != nil {
			return fail(CodeReplicationFailed)
		}
		obs.Replicas = replicas
	}
	if obs.Hosting == preflight.HostingSelf {
		t, err := s.tiered(ctx)
		if err != nil {
			return fail(CodeLifecycleFailed)
		}
		obs.Tiered = t
	}
	return obs, Result{}
}

// resolve returns the sorted, de-duplicated addresses of host. The copy
// resolves the host again later; the policy's networks should therefore be
// ones whose DNS the operator controls.
func (p Prober) resolve(ctx context.Context, host string) ([]string, string) {
	if host == "" {
		return nil, CodeResolveFailed
	}
	var addrs []netip.Addr
	if a, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil {
		addrs = []netip.Addr{a}
	} else {
		res := p.Resolver
		if res == nil {
			res = net.DefaultResolver
		}
		addrs, err = res.LookupNetIP(ctx, "ip", host)
		if err != nil || len(addrs) == 0 {
			return nil, CodeResolveFailed
		}
	}
	out := make([]string, 0, len(addrs))
	for _, a := range addrs {
		s := a.Unmap().WithZone("").String()
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	slices.Sort(out)
	if len(out) > 32 {
		return nil, CodeResolveFailed
	}
	return out, ""
}

// addPrefix records one listed prefix (already relative to the path) and
// reports a code when the listing cannot be represented in the policy.
func addPrefix(out []string, p string) ([]string, string) {
	if p != "" && !residency.ValidPrefix(p) {
		return nil, CodeUnsupportedPrefix
	}
	if slices.Contains(out, p) {
		return out, ""
	}
	if len(out) >= residency.MaxPrefixes {
		return nil, CodeTooManyPrefixes
	}
	return append(out, p), ""
}
