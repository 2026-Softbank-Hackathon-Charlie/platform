package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/onprem"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
)

func (s *Server) onpremLimit(w http.ResponseWriter, r *http.Request) bool {
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	_, ipOK := s.enrollmentIP.Attempt(ip, s.now())
	_, globalOK := s.enrollmentGlobal.Attempt("all", s.now())
	if !ipOK || !globalOK {
		w.Header().Set("Retry-After", "10")
		s.fail(w, r, 429, "too_many_attempts", "onprem connection rate exceeded")
		return false
	}
	return true
}

func (s *Server) onpremSession(w http.ResponseWriter, r *http.Request) (store.OnpremEnrollment, string, bool) {
	w.Header().Set("Cache-Control", "no-store")
	if !s.onpremLimit(w, r) {
		return store.OnpremEnrollment{}, "", false
	}
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") || len(h) > 256 {
		s.enrollmentError(w, r, onprem.ErrUnauthorized)
		return store.OnpremEnrollment{}, "", false
	}
	token := strings.TrimPrefix(h, "Bearer ")
	e, err := s.enrollments.AuthenticateConnection(r.Context(), r.PathValue("id"), token)
	if err != nil {
		s.enrollmentError(w, r, err)
		return e, "", false
	}
	if s.d.OnpremConnector == nil {
		s.enrollmentError(w, r, onprem.ErrUnavailable)
		return e, "", false
	}
	noteAudit(r, "enrollment", e.ID)
	noteAudit(r, "environment", e.Environment)
	return e, token, true
}

func (s *Server) onpremBootstrap(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	r = r.WithContext(ctx)
	e, token, ok := s.onpremSession(w, r)
	if !ok {
		return
	}
	select {
	case s.enrollmentSlots <- struct{}{}:
		defer func() { <-s.enrollmentSlots }()
	default:
		s.fail(w, r, 429, "too_many_attempts", "onprem bootstrap capacity exceeded")
		return
	}
	if err := s.auditBeforeAnswer(r, "GET /v1/onprem/enrollments/{id}/bootstrap.yaml", 200); err != nil {
		s.fail(w, r, 503, "audit_unavailable", "bootstrap not returned because its audit could not be saved")
		return
	}
	manifest, err := s.d.OnpremConnector.Bootstrap(ctx, e, token, r.URL.Query().Get("arch"))
	if err != nil {
		s.enrollmentError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(200)
	_, _ = w.Write(manifest)
}

func (s *Server) onpremConnectionStatus(w http.ResponseWriter, r *http.Request) {
	_, _, ok := s.onpremSession(w, r)
	if !ok {
		return
	}
	v, err := s.enrollments.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		s.enrollmentError(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"enrollment": v})
}

func (s *Server) onpremConnection(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	r = r.WithContext(ctx)
	e, _, ok := s.onpremSession(w, r)
	if !ok {
		return
	}
	select {
	case s.enrollmentSlots <- struct{}{}:
		defer func() { <-s.enrollmentSlots }()
	default:
		s.fail(w, r, 429, "too_many_attempts", "onprem connection capacity exceeded")
		return
	}
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(5 * time.Second))
	r.Body = http.MaxBytesReader(w, r.Body, 65536)
	defer r.Body.Close()
	var req onprem.ConnectionRequest
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if d.Decode(&req) != nil || d.Decode(&struct{}{}) != io.EOF {
		s.fail(w, r, 400, "invalid_request", "invalid onprem connection body")
		return
	}
	_ = http.NewResponseController(w).SetReadDeadline(time.Time{})
	if err := s.auditBeforeAnswer(r, "POST /v1/onprem/enrollments/{id}/connection", 202); err != nil {
		s.fail(w, r, 503, "audit_unavailable", "connection not applied because its audit could not be saved")
		return
	}
	_, err := s.d.OnpremConnector.Connect(ctx, e, req)
	if errors.Is(err, store.ErrConflict) {
		current, readErr := s.d.Store.GetOnpremEnrollment(ctx, e.ID, s.now())
		if readErr == nil && current.ClusterUID == req.ClusterUID && current.LeaseExpiresAt.After(s.now()) && current.Status != "revoked" {
			w.Header().Set("Retry-After", "10")
			s.fail(w, r, 429, "registration_busy", "this device registration is already being checked")
			return
		}
	}
	if err != nil {
		s.enrollmentError(w, r, err)
		return
	}
	v, err := s.enrollments.Get(ctx, e.ID)
	if err != nil {
		s.enrollmentError(w, r, err)
		return
	}
	status := 202
	if v.Status == "ready" {
		status = 200
	}
	writeJSON(w, status, map[string]any{"enrollment": v})
}

func (s *Server) onpremAgentBinary(w http.ResponseWriter, r *http.Request) {
	if !s.onpremLimit(w, r) {
		return
	}
	if s.d.OnpremConnector == nil {
		s.enrollmentError(w, r, onprem.ErrUnavailable)
		return
	}
	select {
	case s.enrollmentSlots <- struct{}{}:
		defer func() { <-s.enrollmentSlots }()
	default:
		s.fail(w, r, 429, "too_many_attempts", "onprem download capacity exceeded")
		return
	}
	path, digest, err := onprem.AgentBinary(s.d.OnpremConnector.AgentDirectory, r.URL.Query().Get("arch"))
	if err != nil {
		s.enrollmentError(w, r, onprem.ErrUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("ETag", "\""+digest+"\"")
	w.Header().Set("Cache-Control", "public, max-age=300")
	http.ServeFile(w, r, path)
}
