package client

import (
	"context"
	"net/http"
	"net/url"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

// SecurityOptions filter the security view. Zero values mean no filter.
type SecurityOptions struct {
	// Environment keeps one environment, e.g. "gke", "aws-account" or "platform".
	Environment string
	// Severity is the minimum severity.
	Severity apiv1.SecuritySeverity
	// Refresh collects again instead of serving the cached view (the server
	// does it at most once every 10 seconds).
	Refresh bool
}

// GetSecurity returns the security and governance view of every environment.
func (c *Client) GetSecurity(ctx context.Context, o SecurityOptions) (*apiv1.SecurityReport, error) {
	q := url.Values{}
	if o.Environment != "" {
		q.Set("environment", o.Environment)
	}
	if o.Severity != "" {
		q.Set("severity", string(o.Severity))
	}
	if o.Refresh {
		q.Set("refresh", "true")
	}
	return call[apiv1.SecurityReport](ctx, c, request{method: http.MethodGet, path: "/v1/security", query: q})
}
