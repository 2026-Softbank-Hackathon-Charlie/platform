package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

// Client talks to the gateway admin API. Its per-app calls (Health, State,
// SetReadWeights, Route, HoldWrites, ReleaseWrites) act on the default app;
// App returns a client whose calls act on another app.
type Client struct {
	BaseURL string
	Token   string
	// HTTP is the client to use. Nil means a client with a 60s timeout,
	// which outlasts the default 30s drain of a hold.
	HTTP *http.Client
}

var defaultHTTP = &http.Client{Timeout: 60 * time.Second}

// APIError is a non-2xx answer from the admin API.
type APIError struct {
	Status  int    // HTTP status
	Code    string // Error.error.code, e.g. "drain_timeout"
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("gateway: %d %s: %s", e.Status, e.Code, e.Message)
}

// Health returns nil when the admin API answers /healthz with 200.
func (c *Client) Health(ctx context.Context) error {
	return c.do(ctx, http.MethodGet, "/healthz", nil, nil)
}

// State returns routing and request stats over window (0 means the gateway's
// default of 10s).
func (c *Client) State(ctx context.Context, window time.Duration) (apiv1.TrafficState, error) {
	path := "/v1/state"
	if window > 0 {
		path += "?window=" + url.QueryEscape(window.String())
	}
	var st apiv1.TrafficState
	err := c.do(ctx, http.MethodGet, path, nil, &st)
	return st, err
}

// SetReadWeights sets the share of reads per backend (summing to 100).
func (c *Client) SetReadWeights(ctx context.Context, weights map[string]int) error {
	return c.do(ctx, http.MethodPost, "/v1/weights", apiv1.WeightsRequest{ReadWeights: weights}, nil)
}

// Route sends writes and all reads to backend.
func (c *Client) Route(ctx context.Context, backend string) error {
	return c.do(ctx, http.MethodPost, "/v1/route", apiv1.RouteRequest{Backend: backend}, nil)
}

// HoldWrites returns once new writes are held and in-flight writes drained.
// After an error writes are not held by this call: a hold that does not drain
// in time is reverted (504 drain_timeout), and one released while it drained
// answers 409 hold_cancelled.
func (c *Client) HoldWrites(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/v1/writes/hold", nil, nil)
}

// ReleaseWrites returns once held writes were dispatched to the write backend.
func (c *Client) ReleaseWrites(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/v1/writes/release", nil, nil)
}

// App returns a client bound to the app registered as name (the default
// app's name works too). Its per-app calls act on that app alone; the app's
// admin base URL is BaseURL + "/apps/" + name. On an unknown app they fail
// with 404 unknown_app. Apps, PutApp and DeleteApp do not apply to it.
func (c *Client) App(name string) *Client {
	app := *c
	app.BaseURL = strings.TrimRight(c.BaseURL, "/") + "/apps/" + url.PathEscape(name)
	return &app
}

// Apps lists the default app and the registered apps.
func (c *Client) Apps(ctx context.Context) (AppList, error) {
	var list AppList
	err := c.do(ctx, http.MethodGet, "/apps", nil, &list)
	return list, err
}

// PutApp registers an app, or replaces its backends and routing. It fails
// with 409 writes_held while the app's writes are held.
func (c *Client) PutApp(ctx context.Context, name string, spec AppSpec) error {
	return c.do(ctx, http.MethodPut, "/apps/"+url.PathEscape(name), spec, nil)
}

// DeleteApp removes a registered app. It fails with 409 writes_held while the
// app's writes are held.
func (c *Client) DeleteApp(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodDelete, "/apps/"+url.PathEscape(name), nil, nil)
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.BaseURL, "/")+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	hc := c.HTTP
	if hc == nil {
		hc = defaultHTTP
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("gateway %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		e := &APIError{Status: resp.StatusCode}
		var eb apiv1.Error
		if json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&eb) == nil {
			e.Code, e.Message = eb.Error.Code, eb.Error.Message
		}
		if e.Message == "" {
			e.Message = http.StatusText(resp.StatusCode)
		}
		return e
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("gateway %s %s: decode response: %w", method, path, err)
	}
	return nil
}
