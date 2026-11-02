package api

import (
	"context"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/security"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

// SecurityReporter builds the security and governance view (security.Service).
type SecurityReporter interface {
	Report(ctx context.Context, refresh bool) (apiv1.SecurityReport, error)
}

// GET /: a browser pointed at the API host gets a name and links, not a 404.
func (s *Server) root(w http.ResponseWriter, _ *http.Request) {
	info := apiv1.ServiceInfo{Name: "hopcharlie-platform"}
	info.Links.Openapi, info.Links.Health = "/openapi.yaml", "/healthz"
	writeJSON(w, http.StatusOK, info)
}

// GET /v1/security?environment=&severity=&refresh=
func (s *Server) security(w http.ResponseWriter, r *http.Request) {
	if s.d.Security == nil {
		s.fail(w, r, http.StatusServiceUnavailable, "security_not_configured", "this server has no security collectors configured")
		return
	}
	q := r.URL.Query()
	atLeast := apiv1.SecuritySeverity(q.Get("severity"))
	if atLeast != "" && !atLeast.Valid() {
		s.fail(w, r, http.StatusBadRequest, "invalid_request", "severity must be critical, high, medium, low or info")
		return
	}
	refresh := false
	if v := q.Get("refresh"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			s.fail(w, r, http.StatusBadRequest, "invalid_request", "refresh must be true or false")
			return
		}
		refresh = b
	}
	rep, err := s.d.Security.Report(r.Context(), refresh)
	if err != nil {
		return // the client went away; the collection goes on for the cache
	}
	env := q.Get("environment")
	if known := security.Environments(rep); env != "" && !slices.Contains(known, env) {
		s.fail(w, r, http.StatusBadRequest, "invalid_request", "unknown environment "+strconv.Quote(env)+"; known: "+strings.Join(known, ", "))
		return
	}
	writeJSON(w, http.StatusOK, security.Filter(rep, env, atLeast))
}
