package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
)

func TestKubernetesTimeoutQueryReachesStatelessValidator(t *testing.T) {
	handler := newAdmissionHandler()
	for _, target := range []string{"/v1/onprem/admission/cluster-secret", "/v1/onprem/admission/cluster-secret?timeout=5s"} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(http.MethodPost, target, strings.NewReader(`{"apiVersion":"admission.k8s.io/v1","kind":"AdmissionReview","request":{"uid":"timeout-request-uid"}}`)))
		var response admissionv1.AdmissionReview
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &response) != nil || response.Response == nil || response.Response.Allowed || response.Response.UID != "timeout-request-uid" || response.Request != nil {
			t.Fatal("the API-server timeout query bypassed or failed to reach the validator")
		}
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/onprem/admission/other?timeout=5s", strings.NewReader(`{}`)))
	if w.Code != http.StatusNotFound {
		t.Fatal("timeout compatibility broadened the allowed path")
	}
}
