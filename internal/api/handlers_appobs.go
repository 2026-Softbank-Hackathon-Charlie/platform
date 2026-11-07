package api

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

// getAppObservability summarises one registered app's logs
// (GET /v1/apps/{name}/observability). Only a registered app is answered, so the
// name cannot be used to read another service's logs (the platform's own, say).
// It returns counts, the services, a five-minute series and the repeated errors
// as redacted patterns, never an ordinary log line.
func (s *Server) getAppObservability(w http.ResponseWriter, r *http.Request) {
	if s.d.AppObs == nil {
		s.fail(w, r, http.StatusServiceUnavailable, "observability_not_configured", "this server cannot read the apps' logs (PLATFORM_AIOBS_CLICKHOUSE_URL)")
		return
	}
	name := r.PathValue("name")
	if _, inFile := s.fileApp(name); !inFile {
		if _, err := s.d.Store.GetApp(r.Context(), name); errors.Is(err, store.ErrNotFound) {
			s.fail(w, r, http.StatusNotFound, "app_not_found", "app not found")
			return
		} else if err != nil {
			s.internal(w, r, err)
			return
		}
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
	o, err := s.d.AppObs.App(r.Context(), name, since)
	if err != nil {
		s.cfg.Log.Warn("app observability: ClickHouse query failed", "app", name, "err", err)
		s.fail(w, r, http.StatusBadGateway, "observability_unavailable", "the app's logs could not be read")
		return
	}
	out := apiv1.AppObservation{
		Since: o.Since, Total: o.Total, Errors: o.Errors, Warnings: o.Warnings,
		Buckets:  make([]apiv1.AppObservationBucket, 0, len(o.Buckets)),
		Services: make([]apiv1.AppObservationService, 0, len(o.Services)),
		Patterns: make([]apiv1.AppObservationPattern, 0, len(o.Patterns)),
	}
	for _, b := range o.Buckets {
		out.Buckets = append(out.Buckets, apiv1.AppObservationBucket{Time: b.Time, Total: b.Total, Errors: b.Errors, Warnings: b.Warnings})
	}
	for _, x := range o.Services {
		out.Services = append(out.Services, apiv1.AppObservationService{Name: x.Name, Total: x.Total, Errors: x.Errors, Warnings: x.Warnings, LastSeen: x.LastSeen})
	}
	for _, p := range o.Patterns {
		out.Patterns = append(out.Patterns, apiv1.AppObservationPattern{Severity: apiv1.AppObservationPatternSeverity(p.Severity), Service: p.Service, Text: p.Text, Count: p.Count, LastSeen: p.LastSeen})
	}
	noteAudit(r, "app", name)
	writeJSON(w, http.StatusOK, out)
}
