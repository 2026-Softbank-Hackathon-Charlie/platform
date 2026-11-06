package api

import (
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/security"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

// Security exceptions (SEC-08, ADR 0034): an operator accepts the risk of
// one finding or vulnerability for a bounded time. They never change
// severity; reports show the active one of each item.
const (
	maxExceptionReason = 500
	maxExceptionTTL    = 30 * 24 * time.Hour
	maxKEVExceptionTTL = 7 * 24 * time.Hour
)

// GET /v1/security/exceptions: every exception, newest first.
func (s *Server) listSecurityExceptions(w http.ResponseWriter, r *http.Request) {
	if !s.exceptionStore(w, r) {
		return
	}
	rows, err := s.d.Store.ListSecurityExceptions(r.Context())
	if err != nil {
		s.internal(w, r, err)
		return
	}
	out := apiv1.SecurityExceptionList{Exceptions: make([]apiv1.SecurityException, 0, len(rows))}
	for _, e := range rows {
		out.Exceptions = append(out.Exceptions, security.ExceptionFromStore(e))
	}
	writeJSON(w, http.StatusOK, out)
}

// POST /v1/security/exceptions
func (s *Server) createSecurityException(w http.ResponseWriter, r *http.Request) {
	if !s.exceptionStore(w, r) {
		return
	}
	var in apiv1.SecurityExceptionRequest
	if !s.decode(w, r, &in, true) {
		return
	}
	now := s.now().UTC()
	in.Reason = strings.TrimSpace(in.Reason)
	if msg := s.checkException(in, now); msg != "" {
		s.fail(w, r, http.StatusBadRequest, "invalid_request", msg)
		return
	}
	e := store.SecurityException{ID: "sx_" + randomHex(8), TargetType: string(in.TargetType), TargetID: in.TargetId, Reason: in.Reason,
		CreatedBy: actor(r), ExpiresAt: in.ExpiresAt.UTC(), CreatedAt: now}
	noteAudit(r, "exception", map[string]any{"targetType": e.TargetType, "targetId": e.TargetID, "reason": e.Reason, "expiresAt": e.ExpiresAt})
	err := s.d.Store.CreateSecurityException(r.Context(), e)
	switch {
	case errors.Is(err, store.ErrConflict):
		s.fail(w, r, http.StatusConflict, "exception_active", "this "+e.TargetType+" already has an active exception")
	case err != nil:
		s.internal(w, r, err)
	default:
		writeJSON(w, http.StatusCreated, security.ExceptionFromStore(e))
	}
}

// checkException returns why in is refused, or "". in.Reason is trimmed.
func (s *Server) checkException(in apiv1.SecurityExceptionRequest, now time.Time) string {
	switch {
	case !in.TargetType.Valid():
		return "targetType must be finding or vulnerability"
	case strings.TrimSpace(in.TargetId) == "":
		return "targetId is required"
	case in.Reason == "":
		return "reason is required"
	case utf8.RuneCountInString(in.Reason) > maxExceptionReason:
		return "reason is longer than 500 characters"
	case in.ExpiresAt.IsZero():
		return "expiresAt is required"
	case !in.ExpiresAt.After(now):
		return "expiresAt must be in the future"
	case in.ExpiresAt.Sub(now) > maxExceptionTTL:
		return "expiresAt must be at most 30 days from now"
	}
	if in.TargetType != apiv1.SecurityExceptionTargetVulnerability {
		return ""
	}
	v, ok := s.latestVulnerability(in.TargetId)
	if !ok {
		return "unknown vulnerability " + in.TargetId
	}
	if cve := security.VulnerabilityCVE(v); s.d.Exploits.KEVListed(cve) && in.ExpiresAt.Sub(now) > maxKEVExceptionTTL {
		return cve + " is in the CISA KEV catalog: expiresAt must be at most 7 days from now"
	}
	return ""
}

// latestVulnerability finds id among the vulnerabilities of the stored scans.
func (s *Server) latestVulnerability(id string) (apiv1.SecurityVulnerability, bool) {
	if s.d.ImageScans == nil {
		return apiv1.SecurityVulnerability{}, false
	}
	_, vulns, err := s.d.ImageScans.Latest()
	if err != nil {
		s.cfg.Log.Warn("security exception: reading image scans failed", "err", err)
		return apiv1.SecurityVulnerability{}, false
	}
	for _, v := range vulns {
		if v.Id == id {
			return v, true
		}
	}
	return apiv1.SecurityVulnerability{}, false
}

// DELETE /v1/security/exceptions/{id}: revokes the exception now.
func (s *Server) revokeSecurityException(w http.ResponseWriter, r *http.Request) {
	if !s.exceptionStore(w, r) {
		return
	}
	id := r.PathValue("id")
	e, err := s.d.Store.RevokeSecurityException(r.Context(), id, actor(r), s.now().UTC())
	switch {
	case errors.Is(err, store.ErrNotFound):
		s.fail(w, r, http.StatusNotFound, "exception_not_found", "no security exception "+id)
	case errors.Is(err, store.ErrConflict):
		s.fail(w, r, http.StatusConflict, "exception_revoked", "security exception "+id+" is already revoked")
	case err != nil:
		s.internal(w, r, err)
	default:
		noteAudit(r, "exception", map[string]any{"id": e.ID, "targetType": e.TargetType, "targetId": e.TargetID})
		writeJSON(w, http.StatusOK, security.ExceptionFromStore(e))
	}
}

// exceptionStore answers 503 when the server has no store.
func (s *Server) exceptionStore(w http.ResponseWriter, r *http.Request) bool {
	if s.d.Store == nil {
		s.fail(w, r, http.StatusServiceUnavailable, "store_not_configured", "this server has no store for security exceptions")
		return false
	}
	return true
}

// activeExceptions reads the exceptions active now for a report. A read
// error is logged and the report goes without exceptions.
func (s *Server) activeExceptions(r *http.Request) map[string]apiv1.SecurityException {
	if s.d.Store == nil {
		return nil
	}
	rows, err := s.d.Store.ListSecurityExceptions(r.Context())
	if err != nil {
		s.cfg.Log.Warn("security report without exceptions: reading them failed", "err", err)
		return nil
	}
	all := make([]apiv1.SecurityException, 0, len(rows))
	for _, e := range rows {
		all = append(all, security.ExceptionFromStore(e))
	}
	return security.ActiveExceptions(all, s.now())
}
