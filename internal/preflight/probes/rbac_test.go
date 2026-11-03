package probes

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/preflight/parity"
	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	authorizationclient "k8s.io/client-go/kubernetes/typed/authorization/v1"
	"k8s.io/client-go/rest"
)

func target() parity.Target {
	return parity.Target{Environment: "onprem", Namespace: "shop", Application: "shop"}
}

func accessRules() []AccessRule {
	return []AccessRule{
		{ID: "read-own-config", Attributes: authorizationv1.ResourceAttributes{Namespace: "shop", Verb: "get", Resource: "configmaps", Name: "settings"}, WantAllowed: true},
		{ID: "deny-other-secrets", Attributes: authorizationv1.ResourceAttributes{Namespace: "other", Verb: "get", Resource: "secrets"}, WantAllowed: false},
	}
}

func sarClient(t *testing.T, handler http.HandlerFunc) authorizationclient.SubjectAccessReviewInterface {
	t.Helper()
	s := httptest.NewServer(handler)
	t.Cleanup(s.Close)
	client, err := authorizationclient.NewForConfig(&rest.Config{
		Host:          s.URL,
		ContentConfig: rest.ContentConfig{ContentType: "application/json", AcceptContentTypes: "application/json"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return client.SubjectAccessReviews()
}

func TestRBACUsesRealAuthorizationClientAndSAIdentity(t *testing.T) {
	calls := 0
	client := sarClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != "POST" || r.URL.Path != "/apis/authorization.k8s.io/v1/subjectaccessreviews" {
			t.Errorf("unexpected operation %s %s", r.Method, r.URL.Path)
		}
		var review authorizationv1.SubjectAccessReview
		if err := json.NewDecoder(r.Body).Decode(&review); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if review.Spec.ResourceAttributes == nil {
			t.Error("missing resource attributes")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		wantGroups := []string{"system:serviceaccounts", "system:serviceaccounts:shop", "system:authenticated"}
		if review.Spec.User != "system:serviceaccount:shop:app" || !reflect.DeepEqual(review.Spec.Groups, wantGroups) {
			t.Errorf("wrong subject: %+v", review.Spec)
		}
		if review.Spec.ResourceAttributes.Verb != "get" {
			t.Error("requirements mutated")
		}
		allowed := review.Spec.ResourceAttributes.Namespace == "shop"
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(authorizationv1.SubjectAccessReview{TypeMeta: metav1.TypeMeta{APIVersion: "authorization.k8s.io/v1", Kind: "SubjectAccessReview"}, Status: authorizationv1.SubjectAccessReviewStatus{Allowed: allowed, Reason: "TOPSECRET"}})
	})
	rules := accessRules()
	p, err := NewRBAC(target(), client, "app", rules)
	if err != nil {
		t.Fatal(err)
	}
	rules[0].Attributes.Verb = "delete"
	v, err := p.Check(context.Background(), target())
	raw, _ := json.Marshal(v)
	if err != nil || v.Status != parity.Pass || calls != 2 || len(v.Checks) != 2 || strings.Contains(string(raw), "TOPSECRET") {
		t.Fatalf("%s calls=%d err=%v", raw, calls, err)
	}
}

func TestRBACRejectsMismatchAndIndeterminate(t *testing.T) {
	for name, tc := range map[string]struct {
		status     authorizationv1.SubjectAccessReviewStatus
		httpStatus int
		want       parity.Status
	}{
		"too much permission": {authorizationv1.SubjectAccessReviewStatus{Allowed: true}, 201, parity.Fail},
		"missing permission":  {authorizationv1.SubjectAccessReviewStatus{}, 201, parity.Fail},
		"evaluation error":    {authorizationv1.SubjectAccessReviewStatus{EvaluationError: "TOPSECRET"}, 201, parity.Unknown},
		"contradictory":       {authorizationv1.SubjectAccessReviewStatus{Allowed: true, Denied: true}, 201, parity.Unknown},
		"forbidden":           {authorizationv1.SubjectAccessReviewStatus{}, 403, parity.Unknown},
	} {
		t.Run(name, func(t *testing.T) {
			client := sarClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.httpStatus)
				if tc.httpStatus != 201 {
					w.Write([]byte(`{"message":"TOPSECRET"}`))
					return
				}
				json.NewEncoder(w).Encode(authorizationv1.SubjectAccessReview{TypeMeta: metav1.TypeMeta{APIVersion: "authorization.k8s.io/v1", Kind: "SubjectAccessReview"}, Status: tc.status})
			})
			p, err := NewRBAC(target(), client, "app", accessRules())
			if err != nil {
				t.Fatal(err)
			}
			v, err := p.Check(context.Background(), target())
			raw, _ := json.Marshal(v)
			if err != nil || v.Status != tc.want || strings.Contains(string(raw), "TOPSECRET") {
				t.Fatalf("%s %v", raw, err)
			}
		})
	}
}

func TestRBACConfigTargetAndCancellation(t *testing.T) {
	client := sarClient(t, func(http.ResponseWriter, *http.Request) { t.Error("unexpected HTTP request") })
	for _, rules := range [][]AccessRule{nil, accessRules()[:1], accessRules()[1:], {accessRules()[0], accessRules()[0]}} {
		if _, err := NewRBAC(target(), client, "app", rules); err == nil {
			t.Fatal("accepted incomplete rules")
		}
	}
	p, err := NewRBAC(target(), client, "app", accessRules())
	if err != nil {
		t.Fatal(err)
	}
	wrong := target()
	wrong.Environment = "aws"
	if _, err := p.Check(context.Background(), wrong); err != ErrWrongTarget {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	v, err := p.Check(ctx, target())
	if err != nil || v.Status != parity.Unknown {
		t.Fatalf("%+v %v", v, err)
	}
	var zero *RBAC
	if _, err := zero.Check(context.Background(), target()); err == nil {
		t.Fatal("nil probe accepted")
	}
}
