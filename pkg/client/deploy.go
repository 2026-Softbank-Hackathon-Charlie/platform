package client

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

// AppSpecError is a 422 invalid_app_spec: platform.yaml could not be read or
// breaks a rule. Validation lists what failed. It wraps the *APIError, so
// ErrorCode and errors.As(err, **APIError) work as for any other error.
type AppSpecError struct {
	*APIError
	Validation apiv1.AppSpecValidation
}

func (e *AppSpecError) Unwrap() error { return e.APIError }

// CreateApp registers an app from a GitHub repository and queues its first
// deployment. An invalid platform.yaml is an *AppSpecError.
func (c *Client) CreateApp(ctx context.Context, req apiv1.AppCreateRequest) (*apiv1.AppCreated, error) {
	return call[apiv1.AppCreated](ctx, c, request{method: http.MethodPost, path: "/v1/apps", body: req})
}

// GetApp returns one app, from the apps file, registered, or both.
func (c *Client) GetApp(ctx context.Context, name string) (*apiv1.App, error) {
	return call[apiv1.App](ctx, c, request{method: http.MethodGet, path: "/v1/apps/%s", params: []string{name}})
}

// DeleteApp unregisters an app; Argo CD then removes its workloads.
func (c *Client) DeleteApp(ctx context.Context, name string) error {
	resp, err := c.send(ctx, request{method: http.MethodDelete, path: "/v1/apps/%s", params: []string{name}})
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// CreateAppInstance deploys another branch of a registered app as its own
// instance and queues the instance's first deployment. An invalid
// platform.yaml on the branch is an *AppSpecError.
func (c *Client) CreateAppInstance(ctx context.Context, app string, req apiv1.AppInstanceCreateRequest) (*apiv1.AppInstanceCreated, error) {
	return call[apiv1.AppInstanceCreated](ctx, c, request{method: http.MethodPost, path: "/v1/apps/%s/instances", params: []string{app}, body: req})
}

// DeleteAppInstance removes an added instance of an app; Argo CD then
// removes its workloads. The default instance goes only with the app.
func (c *Client) DeleteAppInstance(ctx context.Context, app, instance string) error {
	resp, err := c.send(ctx, request{method: http.MethodDelete, path: "/v1/apps/%s/instances/%s", params: []string{app, instance}})
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// ValidateAppSpec checks a platform.yaml, from a repository branch or given
// as content, without registering anything. The result says whether it is
// valid; an invalid file is not an error.
func (c *Client) ValidateAppSpec(ctx context.Context, req apiv1.AppSpecValidateRequest) (*apiv1.AppSpecValidation, error) {
	return call[apiv1.AppSpecValidation](ctx, c, request{method: http.MethodPost, path: "/v1/apps/validate", body: req})
}

// CreateDeployment deploys a registered app now. An empty request deploys the
// app's branch into its default instance, to all its environments. With
// Redeploy set it deploys the instance's last healthy deployment again with
// the current secrets, without a build (ErrorCode nothing_to_redeploy when
// there is none). An invalid platform.yaml at the ref is an *AppSpecError.
func (c *Client) CreateDeployment(ctx context.Context, app string, req apiv1.DeploymentCreateRequest) (*apiv1.Deployment, error) {
	return call[apiv1.Deployment](ctx, c, request{method: http.MethodPost, path: "/v1/apps/%s/deployments", params: []string{app}, body: req})
}

// ListDeployments returns an app's deployments to all its instances, newest
// first. limit <= 0 uses the server's default.
func (c *Client) ListDeployments(ctx context.Context, app string, limit int) (*apiv1.DeploymentList, error) {
	return c.ListBranchDeployments(ctx, app, "", limit)
}

// ListBranchDeployments returns the deployments of an app's branch's
// instance, newest first; branch "" means every instance, as
// ListDeployments. limit <= 0 uses the server's default.
func (c *Client) ListBranchDeployments(ctx context.Context, app, branch string, limit int) (*apiv1.DeploymentList, error) {
	q := url.Values{}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	if branch != "" {
		q.Set("branch", branch)
	}
	return call[apiv1.DeploymentList](ctx, c, request{method: http.MethodGet, path: "/v1/apps/%s/deployments", params: []string{app}, query: q})
}

// GetDeployment returns one deployment.
func (c *Client) GetDeployment(ctx context.Context, id string) (*apiv1.Deployment, error) {
	return call[apiv1.Deployment](ctx, c, request{method: http.MethodGet, path: "/v1/deployments/%s", params: []string{id}})
}

// ListGitHubRepos returns the repositories the platform's GitHub App
// installation can see.
func (c *Client) ListGitHubRepos(ctx context.Context) (*apiv1.GitHubRepoList, error) {
	return call[apiv1.GitHubRepoList](ctx, c, request{method: http.MethodGet, path: "/v1/github/repos"})
}

// SendGitHubWebhook delivers a GitHub webhook the way GitHub does: no bearer
// token, payload signed with secret in X-Hub-Signature-256. It is for tests
// and tools that replay a push. The result lists the deployments the server
// queued; it is empty when the server had nothing to do (204).
func (c *Client) SendGitHubWebhook(ctx context.Context, event, deliveryID string, payload []byte, secret string) (*apiv1.DeploymentList, error) {
	if payload == nil {
		payload = []byte{}
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	h := http.Header{}
	h.Set("X-GitHub-Event", event)
	h.Set("X-GitHub-Delivery", deliveryID)
	h.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	r := request{method: http.MethodPost, path: "/v1/webhooks/github", body: payload, header: h, public: true}
	resp, err := c.send(ctx, r)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	out := &apiv1.DeploymentList{Deployments: []apiv1.Deployment{}}
	if resp.StatusCode == http.StatusNoContent {
		return out, nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return nil, fmt.Errorf("client: decode %s %s response: %w", r.method, r.path, err)
	}
	return out, nil
}

// DeploymentMessage is one message of a deployment's event stream.
type DeploymentMessage struct {
	// Name is the SSE event name: "snapshot", or the event type.
	Name string
	// Seq is the event seq; 0 for a snapshot.
	Seq int64
	// Snapshot is the deployment; set when Name is "snapshot".
	Snapshot *apiv1.Deployment
	// Event is set for every other message.
	Event *apiv1.DeploymentEvent
}

// StreamDeploymentEvents reads a deployment's event stream once and calls fn
// for each message, like StreamEvents does for a run.
func (c *Client) StreamDeploymentEvents(ctx context.Context, id string, after int64, fn func(DeploymentMessage) error) error {
	h := http.Header{"Accept": {"text/event-stream"}, "Cache-Control": {"no-cache"}}
	if after > 0 {
		h.Set("Last-Event-ID", strconv.FormatInt(after, 10))
	}
	resp, err := c.send(ctx, request{method: http.MethodGet, path: "/v1/deployments/%s/events", params: []string{id}, header: h})
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	defer resp.Body.Close()
	if mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); mt != "text/event-stream" {
		return &protocolError{fmt.Errorf("client: deployment event stream answered with Content-Type %q", resp.Header.Get("Content-Type"))}
	}
	var fnErr error
	err = readSSE(resp.Body, func(ev sseEvent) error {
		m, err := toDeploymentMessage(ev)
		if err != nil {
			return err
		}
		if err := fn(m); err != nil {
			fnErr = err
			return err
		}
		return nil
	})
	switch {
	case fnErr != nil:
		return fnErr
	case ctx.Err() != nil:
		return ctx.Err()
	case errors.Is(err, bufio.ErrTooLong):
		return &protocolError{fmt.Errorf("client: deployment event stream line exceeds %d bytes", maxSSELine)}
	case err != nil:
		var pe *protocolError
		if errors.As(err, &pe) {
			return err
		}
		return fmt.Errorf("client: read deployment event stream: %w", err)
	}
	return nil
}

func toDeploymentMessage(ev sseEvent) (DeploymentMessage, error) {
	m := DeploymentMessage{Name: ev.name}
	if ev.name == "snapshot" {
		var d apiv1.Deployment
		if err := json.Unmarshal([]byte(ev.data), &d); err != nil {
			return m, &protocolError{fmt.Errorf("client: decode deployment snapshot: %w", err)}
		}
		m.Snapshot = &d
		return m, nil
	}
	var e apiv1.DeploymentEvent
	if err := json.Unmarshal([]byte(ev.data), &e); err != nil {
		return m, &protocolError{fmt.Errorf("client: decode %q deployment event: %w", ev.name, err)}
	}
	m.Seq, m.Event = e.Seq, &e
	return m, nil
}

// deploymentFinished reports a final deployment status.
func deploymentFinished(s apiv1.DeploymentStatus) bool {
	return s == apiv1.DeploymentStatusHealthy || s == apiv1.DeploymentStatusFailed || s == apiv1.DeploymentStatusSuperseded
}

// WatchDeployment follows a deployment until it is healthy, failed or
// superseded and calls fn for each message. It reconnects the way Watch
// does: with backoff, sending the last delivered seq as Last-Event-ID, never
// delivering a seq twice. It returns nil once a snapshot shows the
// deployment finished and that stream has closed, fn's error as is, and
// ctx.Err() when ctx is done.
func (c *Client) WatchDeployment(ctx context.Context, id string, fn func(DeploymentMessage) error) error {
	var (
		lastSeq   int64
		connected bool
		empty     int
		fnErr     error
	)
	delay := c.retryMin
	for {
		progressed, ended := false, false
		err := c.StreamDeploymentEvents(ctx, id, lastSeq, func(m DeploymentMessage) error {
			progressed, connected = true, true
			if m.Event != nil && m.Seq > 0 {
				if m.Seq <= lastSeq {
					return nil // replayed after a reconnect
				}
				lastSeq = m.Seq
			}
			if m.Snapshot != nil {
				ended = deploymentFinished(m.Snapshot.Status)
			}
			if err := fn(m); err != nil {
				fnErr = err
				return err
			}
			return nil
		})
		switch {
		case fnErr != nil:
			return fnErr
		case ctx.Err() != nil:
			return ctx.Err()
		case err == nil && ended:
			return nil
		case err != nil && (!connected || !retryable(err)):
			return err
		case err == nil && !progressed:
			if !connected {
				return fmt.Errorf("client: event stream of deployment %s closed before its first message", id)
			}
			if empty++; empty >= maxEmptyStreams {
				return fmt.Errorf("client: event stream of deployment %s closed without a message %d times in a row", id, empty)
			}
		}
		if progressed {
			delay, empty = c.retryMin, 0
		}
		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
		if !progressed {
			delay = min(2*delay, c.retryMax)
		}
	}
}
