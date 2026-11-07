package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/aiobs"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
)

type AIServicesReader interface {
	AIServices(context.Context, string, time.Time, time.Time, ...string) (aiobs.AIServiceTopology, error)
}

func (s *Server) getAIServices(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	name := r.PathValue("name")
	if _, ok := s.fileApp(name); !ok {
		if _, err := s.d.Store.GetApp(r.Context(), name); errors.Is(err, store.ErrNotFound) {
			s.fail(w, r, 404, "app_not_found", "app not found")
			return
		} else if err != nil {
			s.internal(w, r, err)
			return
		}
	}
	minutes := 5
	if v := r.URL.Query().Get("minutes"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 60 {
			s.fail(w, r, 400, "invalid_request", "minutes must be between 1 and 60")
			return
		}
		minutes = n
	}
	now := s.now().UTC()
	out := aiobs.AIServiceTopology{App: name, GeneratedAt: now, Window: aiobs.AIServiceWindow{From: now.Add(-time.Duration(minutes) * time.Minute), To: now}, Sources: []aiobs.AIServiceSource{}, Services: []aiobs.AIServiceObservation{}}
	if s.d.AIServices == nil {
		out.Sources = append(out.Sources, aiobs.AIServiceSource{Name: "hyperdx", Status: "not_configured", Message: "AI dependency observations are not configured; unknown does not mean the app has no AI service."})
		writeJSON(w, 200, out)
		return
	}
	exclude, err := s.longerAppNames(r.Context(), name)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	observed, err := s.d.AIServices.AIServices(r.Context(), name, out.Window.From, now, exclude...)
	if err != nil {
		out.Sources = append(out.Sources, aiobs.AIServiceSource{Name: "hyperdx", Status: "unavailable", Message: "AI dependency observations could not be read; existing activity and placement are unknown."})
	} else {
		out = observed
	}
	w.Header().Set("Cache-Control", "no-store")
	noteAudit(r, "app", name)
	writeJSON(w, 200, out)
}
