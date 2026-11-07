package api

import (
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/onprem"
	admissionv1 "k8s.io/api/admission/v1"
)

// A stateless validator: it does not authenticate into a cluster, access the
// store, reflect input credentials or log AdmissionReview objects. A separate
// capacity limit avoids deadlocking the enrollment requests invoking this hook.
func (s *Server) onpremSecretAdmission(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	select {
	case s.onpremAdmissionSlots <- struct{}{}:
		defer func() { <-s.onpremAdmissionSlots }()
	default:
		s.fail(w, r, 503, "admission_busy", "credential validation capacity exceeded")
		return
	}
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(3 * time.Second))
	defer http.NewResponseController(w).SetReadDeadline(time.Time{})
	r.Body = http.MaxBytesReader(w, r.Body, 256<<10)
	defer r.Body.Close()
	var review admissionv1.AdmissionReview
	decoder := json.NewDecoder(r.Body)
	if decoder.Decode(&review) != nil || decoder.Decode(&struct{}{}) != io.EOF {
		s.fail(w, r, 400, "invalid_admission_review", "invalid credential validation request")
		return
	}
	result := onprem.ValidateClusterSecretAdmission(review, s.now())
	result.Request = nil
	writeJSON(w, 200, result)
}
