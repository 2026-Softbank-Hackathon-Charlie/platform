package api

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/aiobs"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/runs"
)

type fakeAIServices struct {
	err     error
	calls   int
	exclude []string
}

func (f *fakeAIServices) AIServices(_ context.Context, app string, from, to time.Time, exclude ...string) (aiobs.AIServiceTopology, error) {
	f.calls++
	f.exclude = exclude
	return aiobs.AIServiceTopology{App: app, GeneratedAt: to, Window: aiobs.AIServiceWindow{From: from, To: to}, Services: []aiobs.AIServiceObservation{}, Sources: []aiobs.AIServiceSource{{Name: "hyperdx", Status: "unobserved"}}}, f.err
}
func TestAIServicesAPIAuthScopeAndUnavailable(t *testing.T) {
	f := &fakeAIServices{}
	h := newHarness(t, withApps(""), func(_ *harness, _ *runs.Config, d *Deps) { d.AIServices = f })
	if code, _ := h.do("GET", "/v1/apps/moimroom/ai-services", "", nil); code != 401 {
		t.Fatal(code)
	}
	if code, _ := h.do("GET", "/v1/apps/unregistered/ai-services", viewerToken, nil); code != 404 || f.calls != 0 {
		t.Fatal("unregistered app queried")
	}
	for _, window := range []string{"0", "61", "x"} {
		if code, _ := h.do("GET", "/v1/apps/moimroom/ai-services?minutes="+window, viewerToken, nil); code != 400 {
			t.Fatal("bad window accepted")
		}
	}
	if code, b := h.do("GET", "/v1/apps/moimroom/ai-services?minutes=5", viewerToken, nil); code != 200 || !strings.Contains(string(b), `"unobserved"`) {
		t.Fatalf("%d %s", code, b)
	}
	f.err = errors.New("password=SECRET private-host")
	if code, b := h.do("GET", "/v1/apps/moimroom/ai-services", viewerToken, nil); code != 200 || strings.Contains(string(b), "SECRET") || !strings.Contains(string(b), `"unavailable"`) {
		t.Fatalf("unsafe failure: %d %s", code, b)
	}
	h = newHarness(t, withApps(""))
	if code, b := h.do("GET", "/v1/apps/moimroom/ai-services", viewerToken, nil); code != 200 || !strings.Contains(string(b), `"not_configured"`) {
		t.Fatalf("missing configuration: %d %s", code, b)
	}
}
