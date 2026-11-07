package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
)

func TestOnpremAdmissionDoesNotNeedStoreOrRegistrationCredentials(t *testing.T) {
	server := New(Config{}, Deps{})
	body := `{"apiVersion":"admission.k8s.io/v1","kind":"AdmissionReview","request":{"uid":"review-1234","operation":"CREATE","namespace":"argocd","kind":{"version":"v1","kind":"Secret"},"resource":{"version":"v1","resource":"secrets"},"object":{"apiVersion":"v1","kind":"Secret","metadata":{"name":"untrusted"},"stringData":{"password":"caller-private-token"}}}}`
	r := httptest.NewRequest(http.MethodPost, "https://api.example.test/v1/onprem/admission/cluster-secret", strings.NewReader(body))
	w := httptest.NewRecorder()
	server.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("validator depends on registration setup: HTTP %d", w.Code)
	}
	var result admissionv1.AdmissionReview
	if json.Unmarshal(w.Body.Bytes(), &result) != nil || result.Response == nil || result.Response.Allowed || string(result.Response.UID) != "review-1234" || result.Request != nil {
		t.Fatal("invalid request was not safely denied")
	}
	if strings.Contains(w.Body.String(), "caller-private-token") || strings.Contains(w.Body.String(), "password") {
		t.Fatal("validator echoed input credentials")
	}
}

func TestOnpremAdmissionBoundsUntrustedBody(t *testing.T) {
	server := New(Config{}, Deps{})
	for _, body := range []string{`not json`, `{"request":{"uid":"` + strings.Repeat("a", (256<<10)+1) + `"}}`} {
		w := httptest.NewRecorder()
		server.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/onprem/admission/cluster-secret", strings.NewReader(body)))
		if w.Code != 400 {
			t.Fatalf("unbounded or malformed admission was accepted: %d", w.Code)
		}
	}
}

func TestOnpremAdmissionAcceptsKubernetesTimeoutQuery(t *testing.T) {
	server := New(Config{}, Deps{})
	w := httptest.NewRecorder()
	server.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/onprem/admission/cluster-secret?timeout=5s", strings.NewReader(`{"apiVersion":"admission.k8s.io/v1","kind":"AdmissionReview","request":{"uid":"api-timeout-query-uid"}}`)))
	var response admissionv1.AdmissionReview
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &response) != nil || response.Response == nil || response.Response.Allowed || response.Response.UID != "api-timeout-query-uid" || response.Request != nil {
		t.Fatal("the Kubernetes timeout query did not reach the bounded stateless admission route")
	}
}
