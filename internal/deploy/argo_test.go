package deploy

import (
	"errors"
	"io/fs"
	"slices"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
)

func application(name string, status map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "argoproj.io/v1alpha1", "kind": "Application",
		"metadata": map[string]any{"name": name, "namespace": "argocd"},
		"status":   status,
	}}
}

func TestArgoCDClient(t *testing.T) {
	gitops := strings.Repeat("a", 40)
	multi := application("shop-gke", map[string]any{
		"sync":   map[string]any{"status": "Synced", "revisions": []any{strings.Repeat("c", 40), gitops}},
		"health": map[string]any{"status": "Degraded", "message": "back-off restarting"},
		"operationState": map[string]any{"phase": "Failed", "message": "one or more objects failed to apply",
			"syncResult": map[string]any{"revisions": []any{strings.Repeat("c", 40), gitops}}},
	})
	single := application("shop-eks", map[string]any{
		"sync":           map[string]any{"status": "OutOfSync", "revision": gitops},
		"health":         map[string]any{"status": "Progressing"},
		"operationState": map[string]any{"phase": "Running", "operation": map[string]any{"sync": map[string]any{"revision": gitops}}},
	})
	appSet := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "argoproj.io/v1alpha1", "kind": "ApplicationSet",
		"metadata": map[string]any{"name": "hopcharlie-apps", "namespace": "argocd"},
	}}
	client := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), multi, single, appSet)
	a := &ArgoCDClient{Client: client, Namespace: "argocd", AppSet: "hopcharlie-apps"}
	ctx := t.Context()

	st, err := a.Status(ctx, "shop-gke")
	if err != nil || !st.Exists || !slices.Contains(st.Revisions, gitops) || len(st.Revisions) != 2 || st.Sync != "Synced" || st.Health != "Degraded" ||
		st.HealthMessage != "back-off restarting" || !st.OperationFailed() || !slices.Contains(st.OperationRevisions, gitops) {
		t.Fatalf("multi-source status %+v, %v", st, err)
	}
	st, err = a.Status(ctx, "shop-eks")
	if err != nil || !slices.Equal(st.Revisions, []string{gitops}) || st.OperationFailed() || !slices.Equal(st.OperationRevisions, []string{gitops}) {
		t.Fatalf("single-source status %+v, %v", st, err)
	}
	if st, err := a.Status(ctx, "shop-onprem"); err != nil || st.Exists {
		t.Fatalf("missing Application: %+v, %v", st, err)
	}

	if err := a.Refresh(ctx, "shop-gke"); err != nil {
		t.Fatal(err)
	}
	got, _ := client.Resource(ApplicationGVR).Namespace("argocd").Get(ctx, "shop-gke", metav1.GetOptions{})
	if got.GetAnnotations()[RefreshAnnotation] != "normal" {
		t.Fatalf("annotations %v", got.GetAnnotations())
	}
	if err := a.Refresh(ctx, "shop-onprem"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("refresh of a missing Application: %v", err)
	}
	if err := a.RefreshApplicationSet(ctx); err != nil {
		t.Fatal(err)
	}
	set, _ := client.Resource(ApplicationSetGVR).Namespace("argocd").Get(ctx, "hopcharlie-apps", metav1.GetOptions{})
	if set.GetAnnotations()[AppSetRefreshAnnotation] != "true" {
		t.Fatalf("ApplicationSet annotations %v", set.GetAnnotations())
	}
}

func TestDeployMultiSourceApplications(t *testing.T) {
	te := newTestEnv(t)
	te.argo.multiSource = true
	te.argo.appSetErr = errors.New(`applicationsets.argoproj.io "hopcharlie-apps" is forbidden`)
	te.register("shop", "gke")
	te.start()
	d := te.deployment("shop", sha("c1"), "gke")
	te.enqueue(d.ID)
	if got := te.wait(d.ID); got.Status != store.DeployHealthy {
		t.Fatalf("deployment %s %q\n%s", got.Status, got.Error, te.dump(d.ID))
	}
	if te.argo.appSetRefreshes != 1 || !hasEvent(te.events(d.ID), EventLog, "could not ask the Argo CD ApplicationSet to refresh") {
		t.Errorf("ApplicationSet refreshes %d, events:\n%s", te.argo.appSetRefreshes, te.dump(d.ID))
	}
}
