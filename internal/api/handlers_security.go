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
	// Collection is the report with the control evaluations and running
	// images it was built from.
	Collection(ctx context.Context, refresh bool) (security.Collection, error)
}

// GET /: a browser pointed at the API host gets a name and links, not a 404.
func (s *Server) root(w http.ResponseWriter, _ *http.Request) {
	info := apiv1.ServiceInfo{Name: "hopcharlie-platform"}
	info.Links.Openapi, info.Links.Health = "/openapi.yaml", "/healthz"
	writeJSON(w, http.StatusOK, info)
}

// GET /v1/security?environment=&severity=&refresh=
func (s *Server) security(w http.ResponseWriter, r *http.Request) {
	if rep, ok := s.securityReport(w, r); ok {
		writeJSON(w, http.StatusOK, rep)
	}
}

// GET /v1/security/ocsf?environment=&severity=&refresh=: the same findings as
// OCSF Detection and Compliance Finding events.
func (s *Server) securityOCSF(w http.ResponseWriter, r *http.Request) {
	if rep, ok := s.securityReport(w, r); ok {
		writeJSON(w, http.StatusOK, security.ToOCSF(rep))
	}
}

// securityReport collects (or reads the cached) report and applies the
// query's filters. It writes the error itself; ok is false then.
func (s *Server) securityReport(w http.ResponseWriter, r *http.Request) (apiv1.SecurityReport, bool) {
	if s.d.Security == nil {
		s.fail(w, r, http.StatusServiceUnavailable, "security_not_configured", "this server has no security collectors configured")
		return apiv1.SecurityReport{}, false
	}
	q := r.URL.Query()
	atLeast := apiv1.SecuritySeverity(q.Get("severity"))
	if atLeast != "" && !atLeast.Valid() {
		s.fail(w, r, http.StatusBadRequest, "invalid_request", "severity must be critical, high, medium, low or info")
		return apiv1.SecurityReport{}, false
	}
	refresh := false
	if v := q.Get("refresh"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			s.fail(w, r, http.StatusBadRequest, "invalid_request", "refresh must be true or false")
			return apiv1.SecurityReport{}, false
		}
		refresh = b
	}
	rep, err := s.d.Security.Report(r.Context(), refresh)
	if err != nil {
		return apiv1.SecurityReport{}, false // the client went away; the collection goes on for the cache
	}
	env := q.Get("environment")
	if known := security.Environments(rep); env != "" && !slices.Contains(known, env) {
		s.fail(w, r, http.StatusBadRequest, "invalid_request", "unknown environment "+strconv.Quote(env)+"; known: "+strings.Join(known, ", "))
		return apiv1.SecurityReport{}, false
	}
	return security.Filter(rep, env, atLeast), true
}

// GET /v1/security/controls?environment=&refresh=: the HopCharlie control
// catalog, each control's evaluations and per-domain coverage (ADR 0034).
func (s *Server) securityControls(w http.ResponseWriter, r *http.Request) {
	if col, env, ok := s.securityCollection(w, r); ok {
		writeJSON(w, http.StatusOK, security.ControlReport(col, env, s.d.ImageScans))
	}
}

// GET /v1/security/vulnerabilities?environment=&refresh=: running images,
// their scan state and the vulnerabilities of stored scans (ADR 0034). It only
// reads stored scans; it never scans. KEV and EPSS are looked up for its CVEs (SEC-07).
func (s *Server) securityVulnerabilities(w http.ResponseWriter, r *http.Request) {
	if col, env, ok := s.securityCollection(w, r); ok {
		rep := security.VulnerabilityReport(col, env, s.d.ImageScans)
		var recs security.DeploymentRecords
		if s.d.Store != nil {
			recs = s.d.Store
		}
		security.CompareDeployed(r.Context(), &rep, recs)
		security.AttachExploitation(r.Context(), &rep, s.d.Exploits)
		writeJSON(w, http.StatusOK, rep)
	}
}

// securityCollection reads the collection and checks the query's environment
// and refresh. It writes the error itself; ok is false then.
func (s *Server) securityCollection(w http.ResponseWriter, r *http.Request) (security.Collection, string, bool) {
	if s.d.Security == nil {
		s.fail(w, r, http.StatusServiceUnavailable, "security_not_configured", "this server has no security collectors configured")
		return security.Collection{}, "", false
	}
	q := r.URL.Query()
	refresh := false
	if v := q.Get("refresh"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			s.fail(w, r, http.StatusBadRequest, "invalid_request", "refresh must be true or false")
			return security.Collection{}, "", false
		}
		refresh = b
	}
	col, err := s.d.Security.Collection(r.Context(), refresh)
	if err != nil {
		return security.Collection{}, "", false // the client went away
	}
	env := q.Get("environment")
	if known := security.Environments(col.Report); env != "" && !slices.Contains(known, env) {
		s.fail(w, r, http.StatusBadRequest, "invalid_request", "unknown environment "+strconv.Quote(env)+"; known: "+strings.Join(known, ", "))
		return security.Collection{}, "", false
	}
	return col, env, true
}
