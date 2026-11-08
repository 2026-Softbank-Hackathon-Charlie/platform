package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/llmobs"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
)

type LLMObserver interface {
	Report(context.Context, string, time.Time, time.Time) llmobs.Report
}

// Registered app identity and the provider binding are independent checks.
// Project IDs, destination URLs and metadata filters are never caller inputs.
func (s *Server) getLLMObservability(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if _, known := s.fileApp(name); !known {
		if _, err := s.d.Store.GetApp(r.Context(), name); errors.Is(err, store.ErrNotFound) {
			s.fail(w, r, 404, "app_not_found", "app not found")
			return
		} else if err != nil {
			s.internal(w, r, err)
			return
		}
	}
	minutes := 60
	for key := range r.URL.Query() {
		if key != "minutes" {
			s.fail(w, r, 400, "invalid_request", "only minutes may be selected")
			return
		}
	}
	if value := r.URL.Query().Get("minutes"); value != "" {
		n, err := strconv.Atoi(value)
		if err != nil || n < 1 || n > 1440 {
			s.fail(w, r, 400, "invalid_request", "minutes must be between 1 and 1440")
			return
		}
		minutes = n
	}
	now := s.now()
	from := now.Add(-time.Duration(minutes) * time.Minute)
	out := llmobs.Empty(name, from, now, "not_configured", "langfuse_not_configured")
	if s.d.LLMObservability != nil {
		out = s.d.LLMObservability.Report(r.Context(), name, from, now)
	}
	w.Header().Set("Cache-Control", "no-store")
	noteAudit(r, "app", name)
	writeJSON(w, http.StatusOK, out)
}
