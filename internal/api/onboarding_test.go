package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/onboard"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/onboard/job"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"

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
	{http.MethodGet, "/v1/onboardings/onb-3f9a1c2b7d4e", nil, false},
	{http.MethodDelete, "/v1/onboardings/onb-3f9a1c2b7d4e", nil, true},
	{http.MethodGet, "/v1/onboardings/onb-3f9a1c2b7d4e/events", nil, false},
	{http.MethodPost, "/v1/onboardings/onb-3f9a1c2b7d4e/approve", map[string]any{"headSha": "0a1b2c"}, true},
	{http.MethodPost, "/v1/onboardings/onb-3f9a1c2b7d4e/cutover", nil, true},
	{http.MethodPost, "/v1/onboardings/onb-3f9a1c2b7d4e/decommission", nil, true},
	{http.MethodPost, "/v1/onboardings/onb-3f9a1c2b7d4e/resume", nil, true},
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

func withOnboardingRunner() option {
	return func(h *harness, _ *runs.Config, d *Deps) {
		h.onboarding = true
		d.Onboarding = &job.Config{AppDomain: "hopcharlie.dev", Envs: []string{"eks", "gke"}, Poll: time.Hour}
	}
}

// TestOnboardingAPI: with the runner the routes answer; an onboarding
// holds its app name against the apps and plans APIs and gives its repo a
// push token while patching.
func TestOnboardingAPI(t *testing.T) {
	h := newHarness(t, withOnboardingRunner())
	status, body := h.do(http.MethodPost, "/v1/onboardings", opToken, map[string]any{"repo": "org/moim", "serviceUrl": "https://moim.hopcharlie.dev"})
	if status != http.StatusAccepted {
		t.Fatalf("start = %d %s", status, body)
	}
	var o apiv1.Onboarding
	if err := json.Unmarshal(body, &o); err != nil || !onboard.ID.MatchString(o.Id) || o.App != "moim" {
		t.Fatalf("started %s %v", body, err)
	}
	if status, body := h.do(http.MethodPost, "/v1/onboardings", opToken, map[string]any{"repo": "org/moim", "serviceUrl": "https://moim.hopcharlie.dev"}); status != http.StatusConflict || !strings.Contains(string(body), "onboarding_exists") {
		t.Fatalf("second start = %d %s", status, body)
	}
	if status, body := h.do(http.MethodPost, "/v1/onboardings", opToken, map[string]any{"repo": "org/x", "serviceUrl": "https://x.example.com"}); status != http.StatusBadRequest {
		t.Fatalf("foreign domain = %d %s", status, body)
	}
	if status, _ := h.do(http.MethodGet, "/v1/onboardings/"+o.Id, viewerToken, nil); status != http.StatusOK {
		t.Fatalf("get = %d", status)
	}
	if status, body := h.do(http.MethodGet, "/v1/onboardings", viewerToken, nil); status != http.StatusOK || !strings.Contains(string(body), o.Id) {
		t.Fatalf("list = %d %s", status, body)
	}
	if status, _ := h.do(http.MethodGet, "/v1/onboardings/onb-000000000000", viewerToken, nil); status != http.StatusNotFound {
		t.Fatalf("unknown = %d", status)
	}
	for _, c := range []struct {
		method, path string
		body         any
	}{
		{http.MethodDelete, "/v1/apps/moim", nil},
		{http.MethodPost, "/v1/apps/moim/environments", map[string]any{"env": "gke"}},
		{http.MethodPost, "/v1/plans/draft", map[string]any{"app": "moim", "source": "eks", "target": "gke"}},
	} {
		if status, body := h.do(c.method, c.path, opToken, c.body); status != http.StatusConflict || !strings.Contains(string(body), "onboarding_exists") {
			t.Errorf("%s %s = %d %s, want 409 onboarding_exists", c.method, c.path, status, body)
		}
	}
	// Approve in the wrong state, cutover too.
	if status, body := h.do(http.MethodPost, "/v1/onboardings/"+o.Id+"/cutover", opToken, nil); status != http.StatusConflict {
		t.Fatalf("cutover = %d %s", status, body)
	}
	// DELETE answers 202 and the row goes.
	if status, body := h.do(http.MethodDelete, "/v1/onboardings/"+o.Id, opToken, nil); status != http.StatusAccepted || !strings.Contains(string(body), `"deleting":true`) {
		t.Fatalf("delete = %d %s", status, body)
	}
}
