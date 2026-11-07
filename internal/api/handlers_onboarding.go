package api

import (
	"net/http"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

// onboardingRoutes registers the onboarding API (ADR 0042) with the roles
// of its contract. Every handler answers 501 until the implementation.
func (s *Server) onboardingRoutes(viewer, operator apiv1.Role) {
	s.route("POST /v1/onboardings", operator, notImplemented)
	s.route("GET /v1/onboardings", viewer, notImplemented)
	s.route("GET /v1/onboardings/{id}", viewer, notImplemented)
	s.route("DELETE /v1/onboardings/{id}", operator, notImplemented)
	s.route("GET /v1/onboardings/{id}/events", viewer, notImplemented)
	s.route("POST /v1/onboardings/{id}/approve", operator, notImplemented)
	s.route("POST /v1/onboardings/{id}/cutover", operator, notImplemented)
	s.route("POST /v1/onboardings/{id}/decommission", operator, notImplemented)
	s.route("POST /v1/onboardings/{id}/resume", operator, notImplemented)
}

func notImplemented(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotImplemented, "not_implemented", r.Pattern+" is in the API contract (ADR 0042) and not built yet")
}
