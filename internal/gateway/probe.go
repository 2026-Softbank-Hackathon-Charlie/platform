package gateway

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ProbeClient is the HTTP client for Prober. It does not follow redirects: a
// redirect (say / to /login) is the answer the gateway counted, and following
// it would count again or leave through the public address.
func ProbeClient() *http.Client {
	return &http.Client{
		Timeout:       10 * time.Second, // each read also has the runner's own, shorter timeout
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// Prober sends synthetic reads through the gateway's public proxy, for the
// canary of an app that has no users (runs.Config.Probe). Each read is a
// GET / like a visitor's first page; the gateway routes it by its read
// weights and counts it, and only 5xx answers count as errors.
type Prober struct {
	// ProxyURL is the gateway's public proxy, e.g. http://hopcharlie-gateway:8000.
	ProxyURL string
	// AppDomain is the gateway's app domain (GATEWAY_APP_DOMAIN): an app
	// scope's reads carry Host <app>.<AppDomain>. Empty sends every read to
	// the default app.
	AppDomain string
	HTTP      *http.Client
}

// Probe sends one read to the app whose admin scope is gatewayURL: the
// gateway root (the default app) or <root>/apps/<name> (ADR 0012).
func (p *Prober) Probe(ctx context.Context, gatewayURL string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(p.ProxyURL, "/")+"/", nil)
	if err != nil {
		return err
	}
	if i := strings.LastIndex(gatewayURL, "/apps/"); i >= 0 && p.AppDomain != "" {
		name, err := url.PathUnescape(strings.TrimRight(gatewayURL[i+len("/apps/"):], "/"))
		if err != nil {
			return err
		}
		req.Host = name + "." + p.AppDomain
	}
	req.Header.Set("User-Agent", "hopcharlie-canary-probe")
	resp, err := p.HTTP.Do(req)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	return resp.Body.Close()
}
