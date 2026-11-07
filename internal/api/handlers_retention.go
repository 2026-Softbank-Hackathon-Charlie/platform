package api

import (
	"net/http"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/retention"
)

func (s *Server) sourceRetention(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("runId")
	j, err := s.d.Runs.SourceRetention(r.Context(), id)
	if err != nil {
		s.runError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Job *retention.Job `json:"job,omitempty"`
	}{j})
}

func (s *Server) preserveSource(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RetainUntil *time.Time `json:"retainUntil,omitempty"`
		Keep        bool       `json:"keep,omitempty"`
	}
	if !s.decode(w, r, &req, true) {
		return
	}
	noteAudit(r, "runId", r.PathValue("runId"))
	noteAudit(r, "keep", req.Keep)
	if req.RetainUntil != nil {
		noteAudit(r, "retainUntil", req.RetainUntil.UTC().Format(time.RFC3339))
	}
	j, err := s.d.Runs.PreserveSource(r.Context(), r.PathValue("runId"), req.RetainUntil, req.Keep)
	if err != nil {
		s.runError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Job *retention.Job `json:"job,omitempty"`
	}{j})
}
