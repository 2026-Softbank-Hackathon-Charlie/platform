package api

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/aiobs"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/runs"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

type fakeAppObs struct {
	obs     aiobs.AppObservation
	err     error
	asked   []string
	since   time.Time
	exclude []string
}

func (f *fakeAppObs) App(_ context.Context, app string, since time.Time, exclude ...string) (aiobs.AppObservation, error) {
	f.asked = append(f.asked, app)
	f.since = since
	f.exclude = exclude
	return f.obs, f.err
}

func withAppObs(f AppObserver) option {
	return func(_ *harness, _ *runs.Config, d *Deps) { d.AppObs = f }
}

func TestAppObservabilityNotConfigured(t *testing.T) {
	h := newHarness(t, withApps(""))
	if code, b := h.do("GET", "/v1/apps/moimroom/observability", viewerToken, nil); code != 503 || errCode(t, b) != "observability_not_configured" {
		t.Fatalf("without ClickHouse: %d %s", code, b)
	}
}

func TestAppObservabilityAnswersARegisteredAppOnly(t *testing.T) {
	t0 := time.Date(2026, 11, 7, 9, 0, 0, 0, time.UTC)
	f := &fakeAppObs{obs: aiobs.AppObservation{
		Since: t0, Total: 1200, Errors: 3, Warnings: 12,
		Buckets:  []aiobs.AppBucket{{Time: t0, Total: 600, Errors: 1, Warnings: 5}},
		Services: []aiobs.AppService{{Name: "moimroom-web", Total: 900, Errors: 3, Warnings: 10, LastSeen: t0}},
		Patterns: []aiobs.AppPattern{{Severity: "error", Service: "moimroom-web", Text: "db error password=«가림» order # failed", Count: 2, LastSeen: t0}},
	}}
	h := newHarness(t, withApps(""), withAppObs(f))
	if code, _ := h.do("GET", "/v1/apps/moimroom/observability", "", nil); code != 401 {
		t.Fatalf("anonymous: %d", code)
	}
	code, b := h.do("GET", "/v1/apps/moimroom/observability?minutes=30", viewerToken, nil)
	if code != 200 {
		t.Fatalf("viewer: %d %s", code, b)
	}
	out := decode[apiv1.AppObservation](t, b)
	if out.Total != 1200 || out.Errors != 3 || len(out.Buckets) != 1 || len(out.Services) != 1 || len(out.Patterns) != 1 || out.Patterns[0].Count != 2 || out.Patterns[0].Severity != apiv1.AppObservationPatternSeverityError {
		t.Fatalf("answer: %s", b)
	}
	if len(f.asked) != 1 || f.asked[0] != "moimroom" || time.Since(f.since) < 29*time.Minute || time.Since(f.since) > 31*time.Minute {
		t.Fatalf("asked %v since %v", f.asked, f.since)
	}
	// an unregistered name must never reach ClickHouse: it could be the platform's own service
	for _, name := range []string{"charlie-platform", "charlie-platform-api", "hopai"} {
		f.asked = nil
		if code, b := h.do("GET", "/v1/apps/"+name+"/observability", viewerToken, nil); code != 404 || errCode(t, b) != "app_not_found" || len(f.asked) != 0 {
			t.Fatalf("%s: %d %s asked=%v", name, code, b, f.asked)
		}
	}
}

func TestAppObservabilityRejectsBadWindowAndHidesErrors(t *testing.T) {
	f := &fakeAppObs{}
	h := newHarness(t, withApps(""), withAppObs(f))
	for _, q := range []string{"minutes=0", "minutes=1441", "minutes=abc"} {
		if code, b := h.do("GET", "/v1/apps/moimroom/observability?"+q, viewerToken, nil); code != 400 || errCode(t, b) != "invalid_request" {
			t.Fatalf("%s: %d %s", q, code, b)
		}
	}
	f.err = errors.New("aiobs: query: dial tcp 10.0.0.5:8123: password=hunter2")
	code, b := h.do("GET", "/v1/apps/moimroom/observability", viewerToken, nil)
	if code != 502 || errCode(t, b) != "observability_unavailable" || strings.Contains(string(b), "hunter2") || strings.Contains(string(b), "10.0.0.5") {
		t.Fatalf("%d %s", code, b)
	}
}

func TestAppObservabilityLeavesOutLongerNamedApps(t *testing.T) {
	f := &fakeAppObs{}
	h := newHarness(t, withApps(""), withAppObs(f))
	for _, name := range []string{"moimroom-mig", "moimroomx", "pxls"} {
		a := store.App{Name: name, Repo: "acme/" + name, Branch: "main", Environments: []string{"gke"},
			Spec: []byte(`{"name":"` + name + `","services":[]}`), SpecCommit: "abc", CreatedBy: "op", CreatedAt: time.Now().UTC()}
		if err := h.store.CreateApp(t.Context(), a); err != nil {
			t.Fatal(err)
		}
	}
	// moimroom (the apps file's) must not count moimroom-mig's services; moimroomx shares no prefix
	if code, b := h.do("GET", "/v1/apps/moimroom/observability", viewerToken, nil); code != 200 {
		t.Fatalf("%d %s", code, b)
	}
	if len(f.exclude) != 1 || f.exclude[0] != "moimroom-mig" {
		t.Fatalf("exclude = %v", f.exclude)
	}
	if code, b := h.do("GET", "/v1/apps/moimroom-mig/observability", viewerToken, nil); code != 200 || len(f.exclude) != 0 {
		t.Fatalf("%d %s exclude = %v", code, b, f.exclude)
	}
}
