package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/envs"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
)

func TestMigrationSecurityBindsRegisteredAppAndEnvironment(t *testing.T) {
	kube := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != "GET" || req.URL.Path != "/api/v1/namespaces/moimroom-mig/pods" {
			t.Errorf("Kubernetes request = %s %s", req.Method, req.URL.Path)
		}
		if req.Header.Get("Authorization") != "Bearer refreshed-token" {
			t.Error("the assessment did not use the refreshed observer token")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(corev1.PodList{
			TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "PodList"},
			Items: []corev1.Pod{{
				ObjectMeta: metav1.ObjectMeta{Name: "moimroom-mig-web-pod", Namespace: "moimroom-mig",
					Labels: map[string]string{"hopcharlie.dev/app": "moimroom-mig", "hopcharlie.dev/service": "web"}},
				Spec:   corev1.PodSpec{Containers: []corev1.Container{{Name: "web"}}},
				Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}},
			}},
		})
	}))
	defer kube.Close()
	lookup := func(_ context.Context, id string) (apiv1.App, bool, error) {
		return apiv1.App{Name: id, Namespace: "moimroom-mig", Environments: []apiv1.AppEnvironment{{Env: "eks"}, {Env: "onprem"}}}, id == "moimroom-mig", nil
	}
	token, connected := "original-token", true
	env := func(name string) (envs.EnvConfig, bool) {
		return envs.EnvConfig{Name: name, KubeContext: name, GatewayBackend: "backend-" + name, RESTConfig: &rest.Config{Host: kube.URL, BearerToken: token}}, connected
	}
	p := &plan.Plan{Gateway: plan.GatewaySpec{Mode: "http", URL: "http://gateway:9901/apps/moimroom-mig", SourceBackend: "backend-eks", TargetBackend: "backend-onprem"}}
	sourceContext, targetContext := "eks", "onprem"
	doc := apiv1.Run{Source: apiv1.PlanEndpoint{Env: "eks", Namespace: "moimroom-mig", Context: &sourceContext}, Target: apiv1.PlanEndpoint{Env: "onprem", Namespace: "moimroom-mig", Context: &targetContext}}
	bind := func(p *plan.Plan, doc apiv1.Run) bool {
		return migrationSecurityAssessor(t.Context(), p, doc, "http://gateway:9901", lookup, env) != nil
	}
	assess := migrationSecurityAssessor(t.Context(), p, doc, "http://gateway:9901", lookup, env)
	if assess == nil {
		t.Fatal("valid registered migration was not bound")
	}
	token = "refreshed-token"
	if got := assess(t.Context(), doc.Source); got.Result != apiv1.SecurityControlResultPass || got.Reason != apiv1.RunSecurityAssessmentReasonClear {
		t.Fatalf("source result = %+v", got)
	}
	connected = false
	if got := assess(t.Context(), doc.Target); got.Reason != apiv1.RunSecurityAssessmentReasonUnavailable {
		t.Fatalf("removed environment still used its previous credentials: %+v", got)
	}
	connected = true
	wrong := *p
	wrong.Gateway.TargetBackend = "backend-gke"
	if bind(&wrong, doc) {
		t.Error("bound a gateway backend inconsistent with the target environment")
	}
	wrong = *p
	wrong.Gateway.URL = "http://other:9901/apps/moimroom-mig"
	if bind(&wrong, doc) {
		t.Error("bound a gateway outside the configured root")
	}
	unregistered := doc
	unregistered.Target.Env = "gke"
	if bind(p, unregistered) {
		t.Error("bound an environment outside this app's registration")
	}
	missingContext := doc
	missingContext.Target.Context = nil
	if bind(p, missingContext) {
		t.Error("bound a database plan without an explicit Kubernetes context")
	}
	wrongNamespace := doc
	wrongNamespace.Target.Namespace = "other"
	if bind(p, wrongNamespace) {
		t.Error("bound a plan namespace outside the app")
	}
}
