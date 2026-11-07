package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/onprem"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
)

func (s *Server) decodeEnrollment(w http.ResponseWriter, r *http.Request, v any) bool {
	if r.ContentLength > 4096 {
		s.fail(w, r, 413, "too_large", "enrollment body exceeds 4096 bytes")
		return false
	}
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(time.Now().Add(5 * time.Second))
	defer controller.SetReadDeadline(time.Time{})
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	defer r.Body.Close()
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	err := d.Decode(v)
	if err == nil {
		if next := d.Decode(&struct{}{}); next != io.EOF {
			err = next
			if err == nil {
				err = errors.New("multiple JSON values")
			}
		}
	}
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		s.fail(w, r, 413, "too_large", "enrollment body exceeds 4096 bytes")
		return false
	}
	if err != nil {
		s.fail(w, r, 400, "invalid_request", "invalid enrollment JSON body")
		return false
	}
	return true
}
func (s *Server) enrollmentError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, onprem.ErrInvalid):
		s.fail(w, r, 400, "invalid_request", "invalid environment, install choice or Idempotency-Key")
	case errors.Is(err, onprem.ErrUnauthorized):
		s.fail(w, r, 401, "invalid_enrollment_code", "invalid bootstrap grant")
	case errors.Is(err, store.ErrNotFound):
		s.fail(w, r, 404, "enrollment_not_found", "enrollment not found")
	case errors.Is(err, store.ErrConflict):
		s.fail(w, r, 409, "enrollment_conflict", "grant expired, consumed, revoked, capacity reached, or idempotency key reused")
	case errors.Is(err, onprem.ErrUnavailable):
		s.fail(w, r, 503, "enrollment_issuer_unavailable", "issuer requires a configured HTTPS public origin")
	default:
		s.internal(w, r, errors.New("enrollment persistence failed"))
	}
}
func (s *Server) createOnpremEnrollment(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	r = r.WithContext(ctx)
	w.Header().Set("Cache-Control", "no-store")
	var req struct {
		Environment string `json:"environment"`
		InstallK3s  *bool  `json:"installK3s"`
	}
	if !s.decodeEnrollment(w, r, &req) {
		return
	}
	if req.InstallK3s == nil {
		s.enrollmentError(w, r, onprem.ErrInvalid)
		return
	}
	out, created, err := s.enrollments.Create(r.Context(), actor(r), r.Header.Get("Idempotency-Key"), onprem.CreateRequest{Environment: req.Environment, InstallK3s: *req.InstallK3s})
	if err != nil {
		s.enrollmentError(w, r, err)
		return
	}
	noteAudit(r, "enrollment", out.Enrollment.ID)
	noteAudit(r, "environment", out.Enrollment.Environment)
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	if err = s.auditBeforeAnswer(r, "POST /v1/onprem/enrollments", status); err != nil {
		if created {
			_, _ = s.enrollments.Revoke(r.Context(), out.Enrollment.ID)
		}
		s.fail(w, r, 503, "audit_unavailable", "bootstrap grant not returned because the audit log could not be written")
		return
	}
	writeJSON(w, status, out)
}
func (s *Server) getOnpremEnrollment(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	r = r.WithContext(ctx)
	w.Header().Set("Cache-Control", "no-store")
	e, err := s.enrollments.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		s.enrollmentError(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"enrollment": e})
}
func (s *Server) revokeOnpremEnrollment(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	r = r.WithContext(ctx)
	w.Header().Set("Cache-Control", "no-store")
	e, err := s.enrollments.Revoke(r.Context(), r.PathValue("id"))
	if err != nil {
		s.enrollmentError(w, r, err)
		return
	}
	noteAudit(r, "enrollment", e.ID)
	writeJSON(w, 200, map[string]any{"enrollment": e})
}
func (s *Server) claimOnpremEnrollment(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	r = r.WithContext(ctx)
	w.Header().Set("Cache-Control", "no-store")
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	_, ipOK := s.enrollmentIP.Attempt(ip, s.now())
	_, globalOK := s.enrollmentGlobal.Attempt("all", s.now())
	if !ipOK || !globalOK {
		w.Header().Set("Retry-After", "60")
		s.fail(w, r, 429, "too_many_attempts", "bootstrap claim rate exceeded")
		return
	}
	select {
	case s.enrollmentSlots <- struct{}{}:
		defer func() { <-s.enrollmentSlots }()
	default:
		s.fail(w, r, 429, "too_many_attempts", "bootstrap claim capacity reached")
		return
	}
	var req struct {
		Code        string `json:"code"`
		Environment string `json:"environment"`
		InstallK3s  *bool  `json:"installK3s"`
	}
	if !s.decodeEnrollment(w, r, &req) {
		return
	}
	if req.InstallK3s == nil {
		s.enrollmentError(w, r, onprem.ErrInvalid)
		return
	}
	e, err := s.enrollments.Claim(r.Context(), r.PathValue("id"), req.Code, onprem.ClaimRequest{Environment: req.Environment, InstallK3s: *req.InstallK3s})
	if err != nil {
		s.enrollmentError(w, r, err)
		return
	}
	noteAudit(r, "enrollment", e.ID)
	noteAudit(r, "environment", e.Environment)
	if err = s.auditBeforeAnswer(r, "POST /v1/onprem/enrollments/{id}/claim", 200); err != nil {
		_, _ = s.enrollments.Revoke(r.Context(), e.ID)
		s.fail(w, r, 503, "audit_unavailable", "bootstrap claim revoked because the audit log could not be written")
		return
	}
	writeJSON(w, 200, map[string]any{"enrollment": e})
}
