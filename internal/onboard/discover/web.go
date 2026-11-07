package discover

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/onboard/facts"
)

// Web resolves and probes the service address.
type Web interface {
	// Resolve returns the name's IPv4 addresses and the smallest TTL of
	// their records. A name without A records returns none and no error.
	Resolve(ctx context.Context, name string) (addrs []string, ttl int, err error)
	// Probe connects to ip:443 with SNI host and asks for paths; it fills
	// Responds, Status, CertNames, CertNotAfter and HealthCandidates.
	Probe(ctx context.Context, host, ip string, paths []string) (facts.HTTPS, error)
}

// HealthPaths are the paths probed for platform.yaml's health check, best
// first.
var HealthPaths = []string{"/healthz", "/health", "/readyz", "/api/health", "/actuator/health", "/"}

// probeHTTPS fills Facts.HTTPS: the addresses, whether they are an
// individual record (not the zone's wildcard, which a random label also
// gets) and the probe of the first address.
func probeHTTPS(ctx context.Context, w Web, host, zone string) (facts.HTTPS, error) {
	addrs, ttl, err := w.Resolve(ctx, host)
	if err != nil {
		return facts.HTTPS{}, err
	}
	out := facts.HTTPS{Host: host, Addresses: addrs, TTLSeconds: ttl, CertNames: []string{}, HealthCandidates: []string{}}
	if out.Addresses == nil {
		out.Addresses = []string{}
	}
	if len(addrs) == 0 {
		return out, nil
	}
	if zone != "" {
		b := make([]byte, 6)
		_, _ = rand.Read(b)
		wild, _, err := w.Resolve(ctx, "hc-probe-"+hex.EncodeToString(b)+"."+zone)
		if err != nil {
			return out, err
		}
		if !sameSet(wild, addrs) {
			out.Record = &facts.DNSRecord{Name: host, Type: "A", Value: addrs[0]}
		}
	}
	p, err := w.Probe(ctx, host, addrs[0], HealthPaths)
	if err != nil {
		return out, err
	}
	out.Responds, out.Status, out.CertNames, out.CertNotAfter, out.HealthCandidates = p.Responds, p.Status, p.CertNames, p.CertNotAfter, p.HealthCandidates
	return out, nil
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	a, b = slices.Clone(a), slices.Clone(b)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(a, b)
}

// NetWeb is Web over DNS-over-HTTPS (Cloudflare's JSON API, as the API
// Pod reaches only TCP 80, 443 and 5432) and a TLS dial.
type NetWeb struct {
	HTTP *http.Client // default 10s timeout
	// DoH is the JSON DNS endpoint (default https://cloudflare-dns.com/dns-query).
	DoH string
}

func (n NetWeb) client() *http.Client {
	if n.HTTP != nil {
		return n.HTTP
	}
	return &http.Client{Timeout: 10 * time.Second}
}

// Resolve asks the DoH endpoint for A records.
func (n NetWeb) Resolve(ctx context.Context, name string) ([]string, int, error) {
	doh := n.DoH
	if doh == "" {
		doh = "https://cloudflare-dns.com/dns-query"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, doh+"?type=A&name="+url.QueryEscape(name), nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "application/dns-json")
	res, err := n.client().Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("dns: resolve %s: %w", name, err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, 0, fmt.Errorf("dns: resolve %s: HTTP %d", name, res.StatusCode)
	}
	var ans struct {
		Status int `json:"Status"`
		Answer []struct {
			Type int    `json:"type"`
			TTL  int    `json:"TTL"`
			Data string `json:"data"`
		} `json:"Answer"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&ans); err != nil {
		return nil, 0, fmt.Errorf("dns: resolve %s: %w", name, err)
	}
	if ans.Status != 0 && ans.Status != 3 { // NOERROR, NXDOMAIN
		return nil, 0, fmt.Errorf("dns: resolve %s: rcode %d", name, ans.Status)
	}
	var addrs []string
	ttl := 0
	for _, a := range ans.Answer {
		if a.Type != 1 || net.ParseIP(a.Data).To4() == nil {
			continue
		}
		addrs = append(addrs, a.Data)
		if ttl == 0 || a.TTL < ttl {
			ttl = a.TTL
		}
	}
	return addrs, ttl, nil
}

// Probe dials ip:443 with SNI host, verifying the certificate for host.
// A connection or certificate failure is Responds false, not an error.
func (n NetWeb) Probe(ctx context.Context, host, ip string, paths []string) (facts.HTTPS, error) {
	out := facts.HTTPS{CertNames: []string{}, HealthCandidates: []string{}}
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	tr := &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, net.JoinHostPort(ip, "443"))
		},
		TLSClientConfig:     &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout: 5 * time.Second,
		DisableKeepAlives:   false,
	}
	defer tr.CloseIdleConnections()
	c := &http.Client{Transport: tr, Timeout: 10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for i, p := range paths {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+host+p, nil)
		if err != nil {
			return out, err
		}
		req.Header.Set("User-Agent", "hopcharlie-onboarding")
		res, err := c.Do(req)
		if err != nil {
			if i == 0 {
				var cv *tls.CertificateVerificationError
				if errors.As(err, &cv) || strings.Contains(err.Error(), "x509") {
					return out, nil
				}
			}
			continue
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 64<<10))
		res.Body.Close()
		if !out.Responds {
			out.Responds = true
			if res.TLS != nil && len(res.TLS.PeerCertificates) > 0 {
				leaf := res.TLS.PeerCertificates[0]
				out.CertNames = append(out.CertNames, leaf.DNSNames...)
				out.CertNotAfter = leaf.NotAfter.UTC()
			}
		}
		if p == "/" {
			out.Status = res.StatusCode
		}
		if res.StatusCode >= 200 && res.StatusCode < 300 {
			out.HealthCandidates = append(out.HealthCandidates, p)
		}
	}
	return out, nil
}
