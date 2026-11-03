package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// Argo reads and refreshes the Argo CD Applications <namespace>-<env> that
// the ApplicationSet generates from the gitops files.
type Argo interface {
	// RefreshApplicationSet asks the ApplicationSet to read the gitops
	// repository now, so a new app's Applications appear without waiting
	// for its next run.
	RefreshApplicationSet(ctx context.Context) error
	// Refresh asks Argo CD to compare the Application with git now. A
	// missing Application is an error matching fs.ErrNotExist.
	Refresh(ctx context.Context, name string) error
	// Status reads the Application. A missing one has Exists false and no
	// error: the ApplicationSet has not generated it yet.
	Status(ctx context.Context, name string) (AppStatus, error)
}

// Argo CD sync and health statuses the executor acts on.
const (
	SyncSynced     = "Synced"
	HealthHealthy  = "Healthy"
	HealthDegraded = "Degraded"
)

// AppStatus is the part of an Application's status the executor reads.
type AppStatus struct {
	Exists bool
	// Revisions are the commits the sync status is about: status.sync.revisions
	// of a multi-source Application (the chart's commit and the gitops
	// commit, in source order), or status.sync.revision of a single-source one.
	Revisions     []string
	Sync          string // status.sync.status: Synced, OutOfSync, Unknown
	Health        string // status.health.status: Healthy, Progressing, Degraded, ...
	HealthMessage string
	// The last sync operation (status.operationState).
	OperationPhase     string // Running, Succeeded, Failed, Error, Terminating
	OperationRevisions []string
	OperationMessage   string
}

// OperationFailed reports whether the last sync operation failed.
func (s AppStatus) OperationFailed() bool {
	return s.OperationPhase == "Failed" || s.OperationPhase == "Error"
}

// ApplicationGVR is Argo CD's Application resource.
var ApplicationGVR = schema.GroupVersionResource{Group: "argoproj.io", Version: "v1alpha1", Resource: "applications"}

// RefreshAnnotation asks Argo CD for a refresh; it removes it when done.
const RefreshAnnotation = "argocd.argoproj.io/refresh"

// ApplicationSetGVR is Argo CD's ApplicationSet resource.
var ApplicationSetGVR = schema.GroupVersionResource{Group: "argoproj.io", Version: "v1alpha1", Resource: "applicationsets"}

// AppSetRefreshAnnotation asks the ApplicationSet controller to run now.
const AppSetRefreshAnnotation = "argocd.argoproj.io/application-set-refresh"

// Defaults for the Argo CD namespace and the ApplicationSet that makes the
// apps' Applications.
const (
	DefaultArgoNamespace = "argocd"
	DefaultArgoAppSet    = "hopcharlie-apps"
)

// ArgoCDClient reads Applications with the Kubernetes API. The account needs
// get and patch on applications.argoproj.io and patch on
// applicationsets.argoproj.io in Namespace.
type ArgoCDClient struct {
	Client    dynamic.Interface
	Namespace string
	// AppSet is the ApplicationSet that generates the apps' Applications.
	AppSet string
}

// NewArgoCDClient connects to the cluster that runs Argo CD: the kubeconfig
// context kubeContext (default loading rules), or the in-cluster
// configuration when kubeContext is empty.
func NewArgoCDClient(kubeContext, namespace, appSet string) (*ArgoCDClient, error) {
	var rc *rest.Config
	var err error
	if kubeContext == "" {
		rc, err = rest.InClusterConfig()
		if err != nil {
			return nil, fmt.Errorf("argo cd: in-cluster configuration (set PLATFORM_ARGOCD_KUBECONTEXT outside a cluster): %w", err)
		}
	} else {
		rules := clientcmd.NewDefaultClientConfigLoadingRules()
		rc, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{CurrentContext: kubeContext}).ClientConfig()
		if err != nil {
			return nil, fmt.Errorf("argo cd: kube context %q: %w", kubeContext, err)
		}
	}
	rc.UserAgent = "hopcharlie-platform"
	dc, err := dynamic.NewForConfig(rc)
	if err != nil {
		return nil, fmt.Errorf("argo cd: %w", err)
	}
	if namespace == "" {
		namespace = DefaultArgoNamespace
	}
	if appSet == "" {
		appSet = DefaultArgoAppSet
	}
	return &ArgoCDClient{Client: dc, Namespace: namespace, AppSet: appSet}, nil
}

func (a *ArgoCDClient) RefreshApplicationSet(ctx context.Context) error {
	return a.annotate(ctx, ApplicationSetGVR, "ApplicationSet", a.AppSet, AppSetRefreshAnnotation, "true")
}

func (a *ArgoCDClient) Refresh(ctx context.Context, name string) error {
	return a.annotate(ctx, ApplicationGVR, "Application", name, RefreshAnnotation, "normal")
}

func (a *ArgoCDClient) annotate(ctx context.Context, gvr schema.GroupVersionResource, kind, name, key, value string) error {
	patch, _ := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": map[string]string{key: value}}})
	_, err := a.Client.Resource(gvr).Namespace(a.Namespace).Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{})
	if apierrors.IsNotFound(err) {
		return fmt.Errorf("%s %s/%s: %w", kind, a.Namespace, name, fs.ErrNotExist)
	}
	return statusError(err)
}

// statusError keeps the API server's message of a status error; others
// (dial errors) carry the server URL, which is not news to an operator.
func statusError(err error) error {
	var se apierrors.APIStatus
	if errors.As(err, &se) && se.Status().Message != "" {
		return errors.New(se.Status().Message)
	}
	return err
}

func (a *ArgoCDClient) Status(ctx context.Context, name string) (AppStatus, error) {
	u, err := a.Client.Resource(ApplicationGVR).Namespace(a.Namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return AppStatus{}, nil
	}
	if err != nil {
		return AppStatus{}, statusError(err)
	}
	return applicationStatus(u), nil
}

func applicationStatus(u *unstructured.Unstructured) AppStatus {
	str := func(path ...string) string {
		v, _, _ := unstructured.NestedString(u.Object, append([]string{"status"}, path...)...)
		return v
	}
	// revisions reads a multi-source list and a single-source value.
	revisions := func(base ...string) []string {
		list, _, _ := unstructured.NestedStringSlice(u.Object, append(append([]string{"status"}, base...), "revisions")...)
		if one := str(append(base, "revision")...); one != "" {
			list = append(list, one)
		}
		return list
	}
	st := AppStatus{
		Exists:             true,
		Revisions:          revisions("sync"),
		Sync:               str("sync", "status"),
		Health:             str("health", "status"),
		HealthMessage:      str("health", "message"),
		OperationPhase:     str("operationState", "phase"),
		OperationRevisions: revisions("operationState", "syncResult"),
		OperationMessage:   str("operationState", "message"),
	}
	if len(st.OperationRevisions) == 0 {
		st.OperationRevisions = revisions("operationState", "operation", "sync")
	}
	return st
}
