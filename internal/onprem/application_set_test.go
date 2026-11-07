package onprem

import (
	"encoding/json"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"testing"
)

func TestEnvironmentApplicationsAreScopedAndRestricted(t *testing.T) {
	profile := map[string]any{"podSecurity": "privileged"}
	state := RegistrationState{EnrollmentID: "enr_0123456789abcdef01234567", Environment: "edge-01"}
	project, set, err := environmentApplications("argocd", state, profile)
	if err != nil {
		t.Fatal(err)
	}
	if profile["podSecurity"] != "privileged" {
		t.Fatal("mutated administrator template")
	}
	value, _, _ := unstructured.NestedString(set.Object, "spec", "template", "spec", "destination", "name")
	if value != registrationResourceName(state.EnrollmentID) {
		t.Fatal("destination escaped registration")
	}
	sources, _, _ := unstructured.NestedSlice(set.Object, "spec", "template", "spec", "sources")
	chart := sources[0].(map[string]any)
	helm := chart["helm"].(map[string]any)
	var rendered map[string]any
	if err := json.Unmarshal([]byte(helm["values"].(string)), &rendered); err != nil {
		t.Fatal(err)
	}
	if rendered["profile"].(map[string]any)["podSecurity"] != "restricted" {
		t.Fatal("unsafe pod security")
	}
	allowed, _, _ := unstructured.NestedSlice(project.Object, "spec", "clusterResourceWhitelist")
	if len(allowed) != 1 || allowed[0].(map[string]any)["kind"] != "Namespace" {
		t.Fatal("cluster-wide management was granted")
	}
	if _, _, err := environmentApplications("argocd", state, nil); err == nil {
		t.Fatal("missing profile accepted")
	}
}
