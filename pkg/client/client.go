// Package client is a typed Go client for the HopCharlie platform API
// (api/openapi.yaml). Requests and responses use the generated apiv1 types, so
// the CLI and every other Go client share the server's contract.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

const defaultUserAgent = "hopcharlie-client"

// Client calls one platform API server. It is safe for concurrent use.
type Client struct {
	base  string
	token string
	hc    *http.Client
	ua    string

	// Watch reconnect backoff; tests shorten it.
	retryMin, retryMax time.Duration
}

// Option configures a Client.
type Option func(*Client)

// WithHTTPClient sets the HTTP client. Avoid a client-wide Timeout: it would
// also cut long-lived event streams. Bound unary calls with their context.
func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) {
		if hc != nil {
			c.hc = hc
		}
	}
}

// WithUserAgent sets the User-Agent header.
func WithUserAgent(ua string) Option {
	return func(c *Client) {
		if ua != "" {
			c.ua = ua
		}
	}
}

// New returns a client for the server at baseURL (e.g. http://localhost:8080)
// that authenticates with token.
func New(baseURL, token string, opts ...Option) *Client {
	c := &Client{
		base:     strings.TrimRight(baseURL, "/"),
		token:    token,
		hc:       http.DefaultClient,
		ua:       defaultUserAgent,
		retryMin: 500 * time.Millisecond,
		retryMax: 15 * time.Second,
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// request describes one API call.
type request struct {
	method string
	path   string   // may contain %s placeholders for params
	params []string // path parameters, escaped into path
	query  url.Values
	body   any // JSON-encoded; []byte is sent as is
	header http.Header
	public bool // the route has no security, so no token is sent
}

// send performs r and returns the response when it is 2xx; otherwise it
// returns an *APIError and closes the body.
func (c *Client) send(ctx context.Context, r request) (*http.Response, error) {
	path := r.path
	if len(r.params) > 0 {
		esc := make([]any, len(r.params))
		for i, p := range r.params {
			// An empty segment would address a different route.
			if p == "" {
				return nil, fmt.Errorf("client: %s %s: empty path parameter", r.method, r.path)
			}
			esc[i] = url.PathEscape(p)
		}
		path = fmt.Sprintf(path, esc...)
	}
	u := c.base + path
	if len(r.query) > 0 {
		u += "?" + r.query.Encode()
	}

	var body io.Reader
	if r.body != nil {
		b, ok := r.body.([]byte)
		if !ok {
			var err error
			if b, err = json.Marshal(r.body); err != nil {
				return nil, fmt.Errorf("client: encode %s %s body: %w", r.method, r.path, err)
			}
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, r.method, u, body)
	if err != nil {
		return nil, err
	}
	for k, v := range r.header {
		req.Header[k] = v
	}
	if r.body != nil && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if req.Header.Get("Accept") == "" {
		req.Header.Set("Accept", "application/json")
	}
	if c.token != "" && !r.public {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	req.Header.Set("User-Agent", c.ua)

	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		defer resp.Body.Close()
		return nil, decodeError(resp)
	}
	return resp, nil
}

// call performs r and decodes the JSON response into a new T.
func call[T any](ctx context.Context, c *Client, r request) (*T, error) {
	resp, err := c.send(ctx, r)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out T
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("client: decode %s %s response: %w", r.method, r.path, err)
	}
	return &out, nil
}

// optional returns nil for an empty string, so omitempty drops the field.
func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// Health calls GET /healthz. It sends no token.
func (c *Client) Health(ctx context.Context) (*apiv1.Health, error) {
	return call[apiv1.Health](ctx, c, request{method: http.MethodGet, path: "/healthz", public: true})
}

// Me returns who the token belongs to.
func (c *Client) Me(ctx context.Context) (*apiv1.Principal, error) {
	return call[apiv1.Principal](ctx, c, request{method: http.MethodGet, path: "/v1/me"})
}

// SubmitPlan submits an engine plan JSON as is.
func (c *Client) SubmitPlan(ctx context.Context, raw []byte) (*apiv1.PlanRecord, error) {
	if raw == nil {
		raw = []byte{}
	}
	return call[apiv1.PlanRecord](ctx, c, request{method: http.MethodPost, path: "/v1/plans", body: raw})
}

// ListPlans returns all plans, newest first.
func (c *Client) ListPlans(ctx context.Context) (*apiv1.PlanList, error) {
	return call[apiv1.PlanList](ctx, c, request{method: http.MethodGet, path: "/v1/plans"})
}

// GetPlan returns one plan.
func (c *Client) GetPlan(ctx context.Context, ref string) (*apiv1.PlanRecord, error) {
	return call[apiv1.PlanRecord](ctx, c, request{method: http.MethodGet, path: "/v1/plans/%s", params: []string{ref}})
}

// ApprovePlan approves a pending plan. comment may be empty.
func (c *Client) ApprovePlan(ctx context.Context, ref, comment string) (*apiv1.PlanRecord, error) {
	return call[apiv1.PlanRecord](ctx, c, request{method: http.MethodPost, path: "/v1/plans/%s/approve", params: []string{ref},
		body: apiv1.ReviewRequest{Comment: optional(comment)}})
}

// RejectPlan rejects a pending plan.
func (c *Client) RejectPlan(ctx context.Context, ref, comment string) (*apiv1.PlanRecord, error) {
	return call[apiv1.PlanRecord](ctx, c, request{method: http.MethodPost, path: "/v1/plans/%s/reject", params: []string{ref},
		body: apiv1.ReviewRequest{Comment: optional(comment)}})
}

// StartMigration starts an approved plan. A non-empty idempotencyKey makes a
// retry return the original run instead of starting another.
func (c *Client) StartMigration(ctx context.Context, req apiv1.StartMigrationRequest, idempotencyKey string) (*apiv1.StartMigrationResponse, error) {
	r := request{method: http.MethodPost, path: "/v1/migrations", body: req}
	if idempotencyKey != "" {
		r.header = http.Header{"Idempotency-Key": {idempotencyKey}}
	}
	return call[apiv1.StartMigrationResponse](ctx, c, r)
}

// ListMigrations returns all runs, newest first.
func (c *Client) ListMigrations(ctx context.Context) (*apiv1.RunList, error) {
	return call[apiv1.RunList](ctx, c, request{method: http.MethodGet, path: "/v1/migrations"})
}

// GetMigration returns one run.
func (c *Client) GetMigration(ctx context.Context, runID string) (*apiv1.Run, error) {
	return call[apiv1.Run](ctx, c, request{method: http.MethodGet, path: "/v1/migrations/%s", params: []string{runID}})
}

// SwitchMigration approves the cutover of a run in awaiting_switch.
func (c *Client) SwitchMigration(ctx context.Context, runID string) (*apiv1.Run, error) {
	return call[apiv1.Run](ctx, c, request{method: http.MethodPost, path: "/v1/migrations/%s/switch", params: []string{runID}})
}

// AbortMigration asks a run to stop. reason may be empty.
func (c *Client) AbortMigration(ctx context.Context, runID, reason string) (*apiv1.Run, error) {
	return call[apiv1.Run](ctx, c, request{method: http.MethodPost, path: "/v1/migrations/%s/abort", params: []string{runID},
		body: apiv1.AbortRequest{Reason: optional(reason)}})
}

// ResumeMigration continues an interrupted, failed or aborted run.
func (c *Client) ResumeMigration(ctx context.Context, runID string) (*apiv1.Run, error) {
	return call[apiv1.Run](ctx, c, request{method: http.MethodPost, path: "/v1/migrations/%s/resume", params: []string{runID}})
}

// ListEnvironments returns the registered environments.
func (c *Client) ListEnvironments(ctx context.Context) (*apiv1.EnvironmentList, error) {
	return call[apiv1.EnvironmentList](ctx, c, request{method: http.MethodGet, path: "/v1/envs"})
}

// GetEnvironment returns one environment.
func (c *Client) GetEnvironment(ctx context.Context, name string) (*apiv1.Environment, error) {
	return call[apiv1.Environment](ctx, c, request{method: http.MethodGet, path: "/v1/envs/%s", params: []string{name}})
}

// GetTraffic returns the traffic gateway's state.
func (c *Client) GetTraffic(ctx context.Context) (*apiv1.TrafficState, error) {
	return call[apiv1.TrafficState](ctx, c, request{method: http.MethodGet, path: "/v1/traffic"})
}

// GetMatrix returns the latest finished run per environment pair.
func (c *Client) GetMatrix(ctx context.Context) (*apiv1.Matrix, error) {
	return call[apiv1.Matrix](ctx, c, request{method: http.MethodGet, path: "/v1/matrix"})
}

// ListAudit returns audit entries, newest first. before <= 0 starts at the
// newest entry; limit <= 0 uses the server's default.
func (c *Client) ListAudit(ctx context.Context, before int64, limit int) (*apiv1.AuditList, error) {
	q := url.Values{}
	if before > 0 {
		q.Set("before", strconv.FormatInt(before, 10))
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	return call[apiv1.AuditList](ctx, c, request{method: http.MethodGet, path: "/v1/audit", query: q})
}
