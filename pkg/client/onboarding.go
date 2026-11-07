package client

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"strconv"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

// StartOnboarding starts onboarding an app that runs outside the platform
// (ADR 0042). Discovery runs in the background; the answer is the new
// onboarding.
func (c *Client) StartOnboarding(ctx context.Context, req apiv1.OnboardingRequest) (*apiv1.Onboarding, error) {
	return call[apiv1.Onboarding](ctx, c, request{method: http.MethodPost, path: "/v1/onboardings", body: req})
}

// ListOnboardings returns every onboarding, newest first.
func (c *Client) ListOnboardings(ctx context.Context) (*apiv1.OnboardingList, error) {
	return call[apiv1.OnboardingList](ctx, c, request{method: http.MethodGet, path: "/v1/onboardings"})
}

// GetOnboarding returns one onboarding.
func (c *Client) GetOnboarding(ctx context.Context, id string) (*apiv1.Onboarding, error) {
	return call[apiv1.Onboarding](ctx, c, request{method: http.MethodGet, path: "/v1/onboardings/%s", params: []string{id}})
}

// DeleteOnboarding asks for an onboarding to be stopped and undone. The
// answer has Deleting true; the onboarding is gone when GetOnboarding
// answers 404.
func (c *Client) DeleteOnboarding(ctx context.Context, id string) (*apiv1.Onboarding, error) {
	return call[apiv1.Onboarding](ctx, c, request{method: http.MethodDelete, path: "/v1/onboardings/%s", params: []string{id}})
}

// ApproveOnboarding approves the PR head headSha (Onboarding.Pr.HeadSha as
// the operator reviewed it) and what will be added to the source.
func (c *Client) ApproveOnboarding(ctx context.Context, id, headSha string) (*apiv1.Onboarding, error) {
	return call[apiv1.Onboarding](ctx, c, request{method: http.MethodPost, path: "/v1/onboardings/%s/approve", params: []string{id},
		body: apiv1.OnboardingApproveRequest{HeadSha: headSha}})
}

// CutOverOnboarding starts the onboarding's migration run; RunId is set in
// the answer.
func (c *Client) CutOverOnboarding(ctx context.Context, id string) (*apiv1.Onboarding, error) {
	return call[apiv1.Onboarding](ctx, c, request{method: http.MethodPost, path: "/v1/onboardings/%s/cutover", params: []string{id}})
}

// DecommissionOnboarding confirms stopping the source VM.
func (c *Client) DecommissionOnboarding(ctx context.Context, id string) (*apiv1.Onboarding, error) {
	return call[apiv1.Onboarding](ctx, c, request{method: http.MethodPost, path: "/v1/onboardings/%s/decommission", params: []string{id}})
}

// ResumeOnboarding checks again and carries on, from failed or a state
// waiting for a person.
func (c *Client) ResumeOnboarding(ctx context.Context, id string) (*apiv1.Onboarding, error) {
	return call[apiv1.Onboarding](ctx, c, request{method: http.MethodPost, path: "/v1/onboardings/%s/resume", params: []string{id}})
}

// OnboardingMessage is one message of an onboarding's event stream.
type OnboardingMessage struct {
	// Name is the SSE event name: "snapshot", or the event kind.
	Name string
	// Seq is the event seq; 0 for a snapshot.
	Seq int64
	// Snapshot is the onboarding; set when Name is "snapshot".
	Snapshot *apiv1.Onboarding
	// Event is set for every other message.
	Event *apiv1.OnboardingEvent
}

// StreamOnboardingEvents reads an onboarding's event stream once and calls
// fn for each message, like StreamDeploymentEvents.
func (c *Client) StreamOnboardingEvents(ctx context.Context, id string, after int64, fn func(OnboardingMessage) error) error {
	h := http.Header{"Accept": {"text/event-stream"}, "Cache-Control": {"no-cache"}}
	if after > 0 {
		h.Set("Last-Event-ID", strconv.FormatInt(after, 10))
	}
	resp, err := c.send(ctx, request{method: http.MethodGet, path: "/v1/onboardings/%s/events", params: []string{id}, header: h})
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	defer resp.Body.Close()
	if mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); mt != "text/event-stream" {
		return &protocolError{fmt.Errorf("client: onboarding event stream answered with Content-Type %q", resp.Header.Get("Content-Type"))}
	}
	var fnErr error
	err = readSSE(resp.Body, func(ev sseEvent) error {
		m, err := toOnboardingMessage(ev)
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
		return &protocolError{fmt.Errorf("client: onboarding event stream line exceeds %d bytes", maxSSELine)}
	case err != nil:
		var pe *protocolError
		if errors.As(err, &pe) {
			return err
		}
		return fmt.Errorf("client: read onboarding event stream: %w", err)
	}
	return nil
}

func toOnboardingMessage(ev sseEvent) (OnboardingMessage, error) {
	m := OnboardingMessage{Name: ev.name}
	if ev.name == "snapshot" {
		var o apiv1.Onboarding
		if err := json.Unmarshal([]byte(ev.data), &o); err != nil {
			return m, &protocolError{fmt.Errorf("client: decode onboarding snapshot: %w", err)}
		}
		m.Snapshot = &o
		return m, nil
	}
	var e apiv1.OnboardingEvent
	if err := json.Unmarshal([]byte(ev.data), &e); err != nil {
		return m, &protocolError{fmt.Errorf("client: decode %q onboarding event: %w", ev.name, err)}
	}
	m.Seq, m.Event = e.Seq, &e
	return m, nil
}

// WatchOnboarding follows an onboarding until it is done or deleted and
// calls fn for each message, reconnecting the way WatchDeployment does. It
// returns nil once a snapshot shows the onboarding done and that stream has
// closed, or when the onboarding is gone (404 onboarding_not_found) after a
// snapshot showed it being deleted; fn's error as is; ctx.Err() when ctx is
// done. A failed onboarding is followed on, since resume carries it on.
func (c *Client) WatchOnboarding(ctx context.Context, id string, fn func(OnboardingMessage) error) error {
	var (
		lastSeq   int64
		connected bool
		deleting  bool
		empty     int
		fnErr     error
	)
	delay := c.retryMin
	for {
		progressed, ended := false, false
		err := c.StreamOnboardingEvents(ctx, id, lastSeq, func(m OnboardingMessage) error {
			progressed, connected = true, true
			if m.Event != nil && m.Seq > 0 {
				if m.Seq <= lastSeq {
					return nil // replayed after a reconnect
				}
				lastSeq = m.Seq
			}
			if m.Snapshot != nil {
				ended = m.Snapshot.State == apiv1.OnboardingStateDone
				deleting = m.Snapshot.Deleting
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
		case err != nil && connected && deleting && ErrorCode(err) == "onboarding_not_found":
			return nil
		case err != nil && (!connected || !retryable(err)):
			return err
		case err == nil && !progressed:
			if !connected {
				return fmt.Errorf("client: event stream of onboarding %s closed before its first message", id)
			}
			if empty++; empty >= maxEmptyStreams {
				return fmt.Errorf("client: event stream of onboarding %s closed without a message %d times in a row", id, empty)
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
