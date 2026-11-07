package api

import (
	"net/http"
	"testing"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/runs"
)

func withOnboarding() option {
	return func(h *harness, _ *runs.Config, _ *Deps) { h.onboarding = true }
}

// onboardingCalls are the endpoints of ADR 0042 with a body each accepts.
var onboardingCalls = []struct {
	method, path string
	body         any
	operator     bool // needs the operator role
}{
	{http.MethodPost, "/v1/onboardings", map[string]any{"repo": "2026-Softbank-Hackathon-Charlie/moimroom-legacy", "serviceUrl": "https://moimroom-legacy.hopcharlie.dev"}, true},
	{http.MethodGet, "/v1/onboardings", nil, false},
	{http.MethodGet, "/v1/onboardings/onb_3f9a1c2b7d4e", nil, false},
	{http.MethodDelete, "/v1/onboardings/onb_3f9a1c2b7d4e", nil, true},
	{http.MethodGet, "/v1/onboardings/onb_3f9a1c2b7d4e/events", nil, false},
	{http.MethodPost, "/v1/onboardings/onb_3f9a1c2b7d4e/approve", map[string]any{"headSha": "0a1b2c"}, true},
	{http.MethodPost, "/v1/onboardings/onb_3f9a1c2b7d4e/cutover", nil, true},
	{http.MethodPost, "/v1/onboardings/onb_3f9a1c2b7d4e/decommission", nil, true},
	{http.MethodPost, "/v1/onboardings/onb_3f9a1c2b7d4e/resume", nil, true},
}

// With PLATFORM_ONBOARDING=1 the contract's routes are there with their
// roles and answer 501 until they are built.
func TestOnboardingRoutesAnswerNotImplemented(t *testing.T) {
	h := newHarness(t, withOnboarding())
	for _, c := range onboardingCalls {
		status, body := h.do(c.method, c.path, opToken, c.body)
		if status != http.StatusNotImplemented {
			t.Errorf("%s %s = %d %s, want 501", c.method, c.path, status, body)
		}
		if c.operator {
			if status, _ := h.do(c.method, c.path, viewerToken, c.body); status != http.StatusForbidden {
				t.Errorf("%s %s as viewer = %d, want 403", c.method, c.path, status)
			}
		} else if status, _ := h.do(c.method, c.path, viewerToken, c.body); status != http.StatusNotImplemented {
			t.Errorf("%s %s as viewer = %d, want 501", c.method, c.path, status)
		}
	}
}

// Off (the default), the routes are not registered.
func TestOnboardingRoutesOffByDefault(t *testing.T) {
	h := newHarness(t)
	for _, c := range onboardingCalls {
		req, _ := http.NewRequest(c.method, h.srv.URL+c.path, nil)
		req.Header.Set("Authorization", "Bearer "+opToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s %s with the feature off = %d, want no route", c.method, c.path, resp.StatusCode)
		}
	}
}
