package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/aiobs"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

// getAIActivity answers what the AI runner did lately (GET /v1/ai-activity),
// read from the observability stack's ClickHouse. It returns parsed fields
// only, so a log line's payload (cluster specs and the like) never reaches the
// console.
func (s *Server) getAIActivity(w http.ResponseWriter, r *http.Request) {
	if s.d.AIActivity == nil {
		s.fail(w, r, http.StatusServiceUnavailable, "observability_not_configured", "this server cannot read the AI runner's logs (PLATFORM_AIOBS_CLICKHOUSE_URL)")
		return
	}
	minutes := 60
	if v := r.URL.Query().Get("minutes"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 1440 {
			s.fail(w, r, http.StatusBadRequest, "invalid_request", "minutes must be between 1 and 1440")
			return
		}
		minutes = n
	}
	since := s.now().Add(-time.Duration(minutes) * time.Minute)
	events, err := s.d.AIActivity.Events(r.Context(), since, 500)
	if err != nil {
		s.cfg.Log.Warn("ai activity: ClickHouse query failed", "err", err)
		s.fail(w, r, http.StatusBadGateway, "observability_unavailable", "the AI runner's logs could not be read")
		return
	}
	sum := aiobs.Summarize(events)
	out := apiv1.AIActivity{
		Since:  since.UTC(),
		Events: make([]apiv1.AIActivityEvent, 0, len(events)),
		Summary: apiv1.AIActivitySummary{
			ModelCalls: sum.ModelCalls, Model: strPtr(sum.Model), TokensIn: sum.TokensIn, TokensOut: sum.TokensOut,
			Tools: sum.Tools, Blocked: sum.Blocked, Rehearsals: sum.Rehearsals, LastEvent: sum.LastEvent,
		},
	}
	for _, e := range events {
		x := apiv1.AIActivityEvent{
			Time: e.Time, Kind: apiv1.AIActivityEventKind(e.Kind), Tool: strPtr(e.Tool), Outcome: strPtr(e.Outcome),
			Model: strPtr(e.Model), StopReason: strPtr(e.StopReason), Passed: e.Passed,
			TokensIn: intPtr(e.TokensIn), TokensOut: intPtr(e.TokensOut), Seconds: intPtr(e.Seconds),
		}
		if len(e.ToolCalls) > 0 {
			calls := e.ToolCalls
			x.ToolCalls = &calls
		}
		out.Events = append(out.Events, x)
	}
	noteAudit(r, "events", len(out.Events))
	writeJSON(w, http.StatusOK, out)
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func intPtr(n int) *int {
	if n == 0 {
		return nil
	}
	return &n
}
