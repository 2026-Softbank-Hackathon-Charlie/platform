package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/retention"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/runs"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

type apiLease struct{}

func (apiLease) Check(context.Context) error { return nil }
func (apiLease) Release()                    {}
func TestRetentionReadAndOperatorPreservation(t *testing.T) {
	h := newHarness(t, func(_ *harness, c *runs.Config, d *Deps) {
		c.RetentionPlan = d.Plans.Load
		c.ClusterLease = func(context.Context, []string) (store.ClusterLease, error) { return apiLease{}, nil }
	})
	ref := h.approved(planJSON("retention", "gke", "eks", false))
	auto := true
	id := h.start(apiv1.StartMigrationRequest{PlanRef: ref, AutoSwitch: &auto})
	h.waitRun(id, func(r apiv1.Run) bool { return r.EndedAt != nil })
	code, body := h.do("GET", "/v1/migrations/"+id+"/retention", viewerToken, nil)
	if code != http.StatusOK || string(body) != "{}\n" {
		t.Fatalf("empty=%d %s", code, body)
	}
	due := time.Now().Add(24 * time.Hour).UTC()
	j := retention.Job{RunID: id, SourceUID: "uid", KeepFor: 24 * time.Hour, DueAt: &due, Status: "scheduled", Pods: []retention.Pod{}}
	b, _ := json.Marshal(j)
	if _, err := h.store.AppendEvent(t.Context(), store.Event{RunID: id, Time: time.Now(), Kind: retention.Event, Data: b}); err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{viewerToken, agentToken} {
		code, _ := h.do("POST", "/v1/migrations/"+id+"/retention", token, map[string]any{"keep": true})
		if code != http.StatusForbidden {
			t.Fatalf("non-operator=%d", code)
		}
	}
	code, _ = h.do("POST", "/v1/migrations/"+id+"/retention", opToken, map[string]any{"retainUntil": due.Add(-time.Hour)})
	if code != 400 {
		t.Fatalf("shortening=%d", code)
	}
	code, body = h.do("POST", "/v1/migrations/"+id+"/retention", opToken, map[string]any{"retainUntil": due.Add(time.Hour)})
	if code != 200 {
		t.Fatalf("extend=%d %s", code, body)
	}
	code, body = h.do("POST", "/v1/migrations/"+id+"/retention", opToken, map[string]any{"keep": true})
	if code != 200 {
		t.Fatalf("keep=%d %s", code, body)
	}
	code, _ = h.do("POST", "/v1/migrations/"+id+"/retention", opToken, map[string]any{"retainUntil": due.Add(2 * time.Hour)})
	if code != 409 {
		t.Fatalf("re-enable cancelled=%d", code)
	}
}
