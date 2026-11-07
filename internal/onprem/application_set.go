package onprem

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

const enrollmentOwnerLabel = "hopcharlie.dev/onprem-enrollment"

func registrationResourceName(id string) string {
	return "hop-onprem-" + strings.TrimPrefix(id, "enr_")
}

// EnsureEnvironmentApplications gives one new environment its own constrained
// ApplicationSet. Existing EKS/GKE/onprem generators and projects are untouched.
func EnsureEnvironmentApplications(ctx context.Context, client dynamic.Interface, namespace string, state RegistrationState, profile map[string]any) error {
	if client == nil || namespace == "" || !namePattern.MatchString(state.Environment) || !idPattern.MatchString(state.EnrollmentID) {
		return ErrInvalid
	}
	project, set, err := environmentApplications(namespace, state, profile)
	if err != nil {
		return err
	}
	for _, obj := range []*unstructured.Unstructured{project, set} {
		resource := "appprojects"
		if obj.GetKind() == "ApplicationSet" {
			resource = "applicationsets"
		}
		api := client.Resource(schema.GroupVersionResource{Group: "argoproj.io", Version: "v1alpha1", Resource: resource}).Namespace(namespace)
		old, err := api.Get(ctx, obj.GetName(), metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			_, err = api.Create(ctx, obj, metav1.CreateOptions{})
			if err != nil {
				return errors.New("onprem Argo resource creation failed")
			}
			continue
		}
		if err != nil {
			return errors.New("onprem Argo resource could not be read")
		}
		if old.GetLabels()[enrollmentOwnerLabel] != state.EnrollmentID {
			return errors.New("onprem Argo resource is owned by another registration")
		}
		// Resource-version CAS prevents a stale heartbeat from replacing newer policy.
		obj.SetResourceVersion(old.GetResourceVersion())
		if _, err = api.Update(ctx, obj, metav1.UpdateOptions{}); err != nil {
			return errors.New("onprem Argo resource changed concurrently")
		}
	}
	return nil
}

func environmentApplications(namespace string, state RegistrationState, profile map[string]any) (*unstructured.Unstructured, *unstructured.Unstructured, error) {
	if len(profile) == 0 {
		return nil, nil, errors.New("onprem app profile is not configured")
	}
	// Deep-copy trusted server configuration before forcing restricted PodSecurity.
	b, err := json.Marshal(profile)
	if err != nil {
		return nil, nil, err
	}
	p := map[string]any{}
	if json.Unmarshal(b, &p) != nil {
		return nil, nil, ErrInvalid
	}
	p["podSecurity"] = "restricted"
	p["onpremRegistration"] = map[string]any{"enrollmentID": state.EnrollmentID, "clusterUID": state.Connection.ClusterUID}
	profileJSON, err := json.Marshal(map[string]any{"profile": p})
	if err != nil {
		return nil, nil, err
	}
	name := registrationResourceName(state.EnrollmentID)
	meta := func() map[string]any {
		return map[string]any{"name": name, "namespace": namespace, "labels": map[string]any{enrollmentOwnerLabel: state.EnrollmentID, "hopcharlie.dev/env": state.Environment, "hopcharlie.dev/onprem-cluster-uid": state.Connection.ClusterUID}}
	}
	cluster := name
	destinations := []any{map[string]any{"name": cluster, "namespace": "*"}}
	for _, ns := range []string{"!kube-*", "!argocd", "!charlie-system", "!default", "!hopcharlie-*"} {
		destinations = append(destinations, map[string]any{"name": cluster, "namespace": ns})
	}
	project := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "argoproj.io/v1alpha1", "kind": "AppProject", "metadata": meta(), "spec": map[string]any{
		"sourceRepos":                []any{"https://github.com/2026-Softbank-Hackathon-Charlie/infra.git", "https://github.com/2026-Softbank-Hackathon-Charlie/gitops.git"},
		"destinations":               destinations,
		"clusterResourceWhitelist":   []any{map[string]any{"group": "", "kind": "Namespace"}},
		"namespaceResourceBlacklist": []any{map[string]any{"group": "rbac.authorization.k8s.io", "kind": "*"}, map[string]any{"group": "", "kind": "Secret"}},
	}}}
	sources := []any{
		map[string]any{"repoURL": "https://github.com/2026-Softbank-Hackathon-Charlie/infra.git", "targetRevision": "main", "path": "helm/charts/hopcharlie-app", "helm": map[string]any{"releaseName": "{{ .namespace }}", "valueFiles": []any{"$gitops/{{ .path.path }}/{{ .path.filename }}"}, "values": string(profileJSON)}},
		map[string]any{"repoURL": "https://github.com/2026-Softbank-Hackathon-Charlie/gitops.git", "targetRevision": "main", "ref": "gitops"},
	}
	set := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "argoproj.io/v1alpha1", "kind": "ApplicationSet", "metadata": meta(), "spec": map[string]any{
		"goTemplate": true, "goTemplateOptions": []any{"missingkey=error"},
		"generators": []any{map[string]any{"git": map[string]any{"repoURL": "https://github.com/2026-Softbank-Hackathon-Charlie/gitops.git", "revision": "main", "files": []any{map[string]any{"path": "apps/*/" + state.Environment + ".yaml"}}, "requeueAfterSeconds": int64(30)}}},
		"template": map[string]any{"metadata": map[string]any{"name": "{{ .namespace }}-" + state.Environment, "labels": map[string]any{"hopcharlie.dev/app": "{{ .app }}", "hopcharlie.dev/instance": "{{ .namespace }}", "hopcharlie.dev/env": state.Environment, enrollmentOwnerLabel: state.EnrollmentID}}, "spec": map[string]any{
			"project": name, "sources": sources, "destination": map[string]any{"name": cluster, "namespace": "{{ .namespace }}"},
			"syncPolicy": map[string]any{"automated": map[string]any{"prune": true, "selfHeal": true}, "retry": map[string]any{"limit": int64(10), "backoff": map[string]any{"duration": "5s", "factor": int64(2), "maxDuration": "3m"}}},
		}},
	}}}
	return project, set, nil
}
