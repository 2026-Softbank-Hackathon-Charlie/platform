package onprem

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/envs"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	authv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	ktesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/clientcmd"
)

const runtimeTestID = "enr_0123456789abcdef01234567"

func testKubeRuntime(t *testing.T) *KubeRuntime {
	t.Helper()
	registry, err := envs.New(envs.Config{Environments: []envs.EnvConfig{{Name: "static", Provider: "onprem", KubeContext: "static"}}}, envs.Options{})
	if err != nil {
		t.Fatal(err)
	}
	kube := kubefake.NewSimpleClientset()
	kube.PrependReactor("create", "services", func(a ktesting.Action) (bool, runtime.Object, error) {
		if !dryRunCreate(a) {
			return false, nil, nil
		}
		obj := a.(ktesting.CreateAction).GetObject().(*corev1.Service)
		if len(obj.Spec.Ports) == 1 && obj.Spec.Ports[0].Port == 6443 {
			return true, obj.DeepCopy(), nil
		}
		if len(obj.Spec.Ports) != 1 || obj.Spec.Ports[0].Port != 22 {
			t.Fatal("management route readiness probe changed unexpected fields")
		}
		return true, nil, apierrors.NewForbidden(corev1.Resource("services"), obj.Name, errors.New("ValidatingAdmissionPolicy hopcharlie-onprem-api-route denied forbidden port"))
	})
	kube.PrependReactor("create", "secrets", func(a ktesting.Action) (bool, runtime.Object, error) {
		if !dryRunCreate(a) {
			return false, nil, nil
		}
		obj := a.(ktesting.CreateAction).GetObject().(*corev1.Secret)
		if obj.Annotations["hopcharlie.dev/registration-version"] != "1" || obj.Type != corev1.SecretTypeOpaque {
			t.Fatal("management credential probe is not the expected TLS mutation")
		}
		if strings.Contains(string(obj.Data["config"]), `"serverName":"kubernetes.default.svc"`) {
			return true, obj.DeepCopy(), nil
		}
		if !strings.Contains(string(obj.Data["config"]), `"serverName":"attacker.invalid"`) {
			t.Fatal("unexpected TLS mutation")
		}
		return true, nil, apierrors.NewForbidden(corev1.Resource("secrets"), obj.Name, errors.New("admission webhook \"hopcharlie-onprem-secret.hopcharlie.dev\" denied invalid TLS name"))
	})
	kube.PrependReactor("create", "selfsubjectaccessreviews", func(a ktesting.Action) (bool, runtime.Object, error) {
		review := a.(ktesting.CreateAction).GetObject().(*authv1.SelfSubjectAccessReview).DeepCopy()
		r := review.Spec.ResourceAttributes
		review.Status.Allowed = !(r.Resource == "secrets" && (r.Verb == "get" || r.Verb == "list" || r.Verb == "watch"))
		return true, review, nil
	})
	// Fake the admission ownership boundary, not GET access to Argo credentials.
	kube.PrependReactor("update", "secrets", func(a ktesting.Action) (bool, runtime.Object, error) {
		desired := a.(ktesting.UpdateAction).GetObject().(*corev1.Secret)
		object, err := kube.Tracker().Get(corev1.SchemeGroupVersion.WithResource("secrets"), a.GetNamespace(), desired.Name)
		if err != nil {
			return false, nil, nil
		}
		old := object.(*corev1.Secret)
		if old.Labels[registrationOwnerLabel] != desired.Labels[registrationOwnerLabel] || old.Labels[registrationUIDLabel] != desired.Labels[registrationUIDLabel] || string(old.Data["server"]) != string(desired.Data["server"]) {
			return true, nil, apierrors.NewForbidden(corev1.Resource("secrets"), desired.Name, errors.New("ownership admission denied"))
		}
		return false, nil, nil
	})
	kube.PrependReactor("delete", "secrets", func(a ktesting.Action) (bool, runtime.Object, error) {
		name := a.(ktesting.DeleteAction).GetName()
		object, err := kube.Tracker().Get(corev1.SchemeGroupVersion.WithResource("secrets"), a.GetNamespace(), name)
		if err != nil {
			return false, nil, nil
		}
		old := object.(*corev1.Secret)
		if old.Labels[registrationOwnerLabel] != "enr_"+strings.TrimPrefix(name, "hop-onprem-") {
			return true, nil, apierrors.NewForbidden(corev1.Resource("secrets"), name, errors.New("ownership admission denied"))
		}
		return false, nil, nil
	})
	dc := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	for _, resource := range []string{"appprojects", "applicationsets"} {
		dc.PrependReactor("create", resource, func(a ktesting.Action) (bool, runtime.Object, error) {
			if !dryRunCreate(a) {
				return false, nil, nil
			}
			obj := a.(ktesting.CreateAction).GetObject()
			body, err := json.Marshal(obj)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(body), "attacker.invalid") {
				return true, obj.DeepCopyObject(), nil
			}
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "argoproj.io", Resource: a.GetResource().Resource}, "management-readiness-probe", errors.New("ValidatingAdmissionPolicy hopcharlie-onprem-argo denied untrusted repo"))
		})
	}
	return NewKubeRuntime(KubeRuntimeConfig{Kube: kube, Dynamic: dc, Envs: registry, TailnetDomain: "tailtest.ts.net", RouteNamespace: "hopcharlie-db-network", ArgoNamespace: "argocd", ProxyClass: "hopcharlie-onprem-api-egress-management", HelloImage: "docker.io/library/python:3.13-alpine@sha256:1e1f6334529d1fb26a2ef619d7cffb30a7c7d7cc1cc7338ebee283a58d1aeaca", Profile: map[string]any{"podSecurity": "restricted"}})
}

func dryRunCreate(a ktesting.Action) bool {
	opts, ok := a.(interface{ GetCreateOptions() metav1.CreateOptions })
	if !ok {
		return false
	}
	for _, mode := range opts.GetCreateOptions().DryRun {
		if mode == metav1.DryRunAll {
			return true
		}
	}
	return false
}

func TestManagementAdmissionReadinessRequiresEveryPolicy(t *testing.T) {
	resources := []string{"services", "secrets", "appprojects", "applicationsets"}
	for _, resource := range resources {
		for _, mode := range []string{"missing", "rbac-only", "wrong-policy", "always-deny"} {
			t.Run(resource+"/"+mode, func(t *testing.T) {
				k := testKubeRuntime(t)
				reactor := func(a ktesting.Action) (bool, runtime.Object, error) {
					if !dryRunCreate(a) {
						return false, nil, nil
					}
					obj := a.(ktesting.CreateAction).GetObject()
					if mode == "missing" {
						return true, obj.DeepCopyObject(), nil
					}
					message := "User cannot create this resource"
					if mode == "wrong-policy" || mode == "always-deny" {
						policy := "hopcharlie-onprem-argo"
						if resource == "services" {
							policy = "hopcharlie-onprem-api-route"
						}
						if resource == "secrets" {
							policy = "hopcharlie-onprem-secret.hopcharlie.dev"
						}
						message = policy + "-disabled denied"
						if mode == "always-deny" {
							message = policy + " denied every object"
						}
					}
					return true, nil, apierrors.NewForbidden(a.GetResource().GroupResource(), "readiness-probe", errors.New(message))
				}
				if resource == "services" || resource == "secrets" {
					k.cfg.Kube.(*kubefake.Clientset).PrependReactor("create", resource, reactor)
				} else {
					k.cfg.Dynamic.(*dynamicfake.FakeDynamicClient).PrependReactor("create", resource, reactor)
				}
				if err := k.Configured(t.Context()); !errors.Is(err, ErrUnavailable) {
					t.Fatalf("%s accepted %s admission state: %v", resource, mode, err)
				}
			})
		}
	}
	if err := testKubeRuntime(t).Configured(t.Context()); err != nil {
		t.Fatal("all four exact-policy denials rejected", err)
	}
}

func TestManagementAdmissionReadinessUsesOnlyDryRuns(t *testing.T) {
	k := testKubeRuntime(t)
	if err := k.Configured(t.Context()); err != nil {
		t.Fatal(err)
	}
	actions := append(k.cfg.Kube.(*kubefake.Clientset).Actions(), k.cfg.Dynamic.(*dynamicfake.FakeDynamicClient).Actions()...)
	count := 0
	for _, a := range actions {
		resource := a.GetResource().Resource
		if resource == "services" || resource == "secrets" || resource == "appprojects" || resource == "applicationsets" {
			if a.GetVerb() != "create" || !dryRunCreate(a) {
				t.Fatalf("readiness performed a real resource mutation: %s %s", a.GetVerb(), resource)
			}
			count++
		}
	}
	if count != 8 {
		t.Fatalf("expected four valid and four invalid dry runs, got %d", count)
	}
}

func TestKubeRuntimeNeverReadsArgoCredentials(t *testing.T) {
	k := testKubeRuntime(t)
	s := runtimeState(t, k)
	kube := k.cfg.Kube.(*kubefake.Clientset)
	for range 2 {
		if err := k.Publish(t.Context(), s); err != nil {
			t.Fatal(err)
		}
	}
	if err := k.Remove(t.Context(), s.EnrollmentID, s.Environment); err != nil {
		t.Fatal(err)
	}
	for _, a := range kube.Actions() {
		if a.GetResource().Resource == "secrets" && (a.GetVerb() == "get" || a.GetVerb() == "list" || a.GetVerb() == "watch") {
			t.Fatalf("registrar reads existing Argo credentials: %s", a.GetVerb())
		}
	}
}

func TestKubeRuntimeRejectsCredentialReadPermissions(t *testing.T) {
	k := testKubeRuntime(t)
	k.cfg.Kube.(*kubefake.Clientset).PrependReactor("create", "selfsubjectaccessreviews", func(a ktesting.Action) (bool, runtime.Object, error) {
		review := a.(ktesting.CreateAction).GetObject().(*authv1.SelfSubjectAccessReview).DeepCopy()
		review.Status.Allowed = true
		return true, review, nil
	})
	if err := k.Configured(t.Context()); !errors.Is(err, ErrUnavailable) {
		t.Fatal("registrar can read every cloud credential", err)
	}
}

// Opt-in only: this must point at the disposable kind cluster provisioned by CI,
// never at the developer's current/default kubeconfig or a production context.
func TestUnconditionalSecretUpdateOnDisposableKind(t *testing.T) {
	path := os.Getenv("HOPCHARLIE_ONPREM_TEST_KUBECONFIG")
	if path == "" {
		t.Skip("requires explicit disposable kind kubeconfig")
	}
	raw, err := clientcmd.LoadFromFile(path)
	if err != nil {
		t.Fatal("test kubeconfig could not be loaded")
	}
	if !strings.HasPrefix(raw.CurrentContext, "kind-") || !strings.Contains(raw.CurrentContext, "onprem") {
		t.Fatal("unconditional update test requires an onprem-named disposable kind context")
	}
	rc, err := clientcmd.NewNonInteractiveClientConfig(*raw, raw.CurrentContext, &clientcmd.ConfigOverrides{}, nil).ClientConfig()
	if err != nil {
		t.Fatal("test client config could not be built")
	}
	client, err := kubernetes.NewForConfig(rc)
	if err != nil {
		t.Fatal(err)
	}
	ns, err := client.CoreV1().Namespaces().Create(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "hop-onprem-secret-update-test-", Labels: map[string]string{"hopcharlie.dev/disposable-test": "true"}}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		uid := ns.UID
		_ = client.CoreV1().Namespaces().Delete(context.Background(), ns.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}})
	})
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "no-get-refresh", Namespace: ns.Name}, Data: map[string][]byte{"credential": []byte("test-only-first")}}
	if _, err = client.CoreV1().Secrets(ns.Name).Create(t.Context(), secret, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	// Deliberately do not GET and do not populate ResourceVersion.
	secret.Data["credential"] = []byte("test-only-refreshed")
	updated, err := client.CoreV1().Secrets(ns.Name).Update(t.Context(), secret, metav1.UpdateOptions{})
	if err != nil || string(updated.Data["credential"]) != "test-only-refreshed" {
		t.Fatal("Kubernetes rejected unconditional Secret credential renewal", err)
	}
}

func TestKubeRuntimeObservesAdmissionAndHTTPProof(t *testing.T) {
	for _, tc := range []struct {
		name          string
		policy, proof bool
		want          string
	}{{"verified", true, true, ""}, {"admission absent", false, true, "permissions"}, {"wrong HTTP proof", true, false, "hello_world"}} {
		t.Run(tc.name, func(t *testing.T) {
			k := testKubeRuntime(t)
			s := runtimeState(t, k)
			probeNS := "hop-check-" + strings.TrimPrefix(s.EnrollmentID, "enr_")
			labels := map[string]string{registrationOwnerLabel: s.EnrollmentID, registrationUIDLabel: s.Connection.ClusterUID}
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				write := func(v any) {
					// Return the real Kubernetes wire type, including GVK.
					switch object := v.(type) {
					case corev1.Namespace:
						object.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "Namespace"})
						v = object
					case corev1.NodeList:
						object.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "NodeList"})
						v = object
					case corev1.ConfigMap:
						object.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"})
						v = object
					case corev1.Pod:
						object.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "Pod"})
						v = object
					case metav1.Status:
						object.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "Status"})
						v = object
					}
					if err := json.NewEncoder(w).Encode(v); err != nil {
						t.Error(err)
					}
				}
				switch {
				case r.URL.Path == "/version":
					write(map[string]string{"major": "1", "minor": "36", "gitVersion": "v1.36.5"})
				case r.URL.Path == "/api/v1/namespaces/kube-system":
					write(corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: types.UID(s.Connection.ClusterUID)}})
				case r.URL.Path == "/api/v1/nodes":
					write(corev1.NodeList{Items: []corev1.Node{{Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}}})
				case strings.HasSuffix(r.URL.Path, "/selfsubjectaccessreviews"):
					var review struct {
						Spec struct {
							ResourceAttributes struct {
								Resource    string `json:"resource"`
								Namespace   string `json:"namespace"`
								Subresource string `json:"subresource"`
							} `json:"resourceAttributes"`
						} `json:"spec"`
					}
					if err := json.NewDecoder(r.Body).Decode(&review); err != nil {
						t.Error(err)
					}
					attributes := review.Spec.ResourceAttributes
					allowed := attributes.Resource != "secrets" && attributes.Resource != "clusterrolebindings" && !(attributes.Resource == "pods" && attributes.Subresource == "proxy" && attributes.Namespace == "kube-system")
					write(map[string]any{"apiVersion": "authorization.k8s.io/v1", "kind": "SelfSubjectAccessReview", "status": map[string]bool{"allowed": allowed}})
				case r.URL.Path == "/api/v1/namespaces/default/configmaps":
					if r.URL.Query().Get("dryRun") != "All" {
						t.Error("protected namespace probe attempted a write")
					}
					if tc.policy {
						w.WriteHeader(http.StatusForbidden)
						write(metav1.Status{Status: "Failure", Code: 403, Reason: metav1.StatusReasonForbidden, Message: "ValidatingAdmissionPolicy hop-" + strings.TrimPrefix(s.EnrollmentID, "enr_") + "-argo denied protected namespace"})
					} else {
						write(corev1.ConfigMap{})
					}
				case r.URL.Path == "/api/v1/namespaces/"+probeNS:
					write(corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: probeNS, Labels: labels}})
				case r.URL.Path == "/api/v1/namespaces/"+probeNS+"/pods/hello":
					write(corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "hello", UID: types.UID("observed-pod-uid"), Labels: labels}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "hello", Image: k.cfg.HelloImage}}}, Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "hello", Ready: true, ImageID: "docker.io/python@sha256:downloaded"}}}})
				case strings.Contains(r.URL.Path, "/pods/hello:8080/proxy"):
					proof := "Hello World! " + s.EnrollmentID + " observed-pod-uid\n"
					if !tc.proof {
						proof = "Hello World! wrong-device\n"
					}
					_, _ = w.Write([]byte(proof))
				default:
					t.Errorf("unexpected Kubernetes API request %s", r.URL.Path)
					w.WriteHeader(404)
				}
			})
			server := httptest.NewTLSServer(handler)
			defer server.Close()
			ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
			k.cfg.NewRemoteClient = func(rc *rest.Config) (kubernetes.Interface, error) {
				copy := rest.CopyConfig(rc)
				copy.ContentType = "application/json"
				copy.AcceptContentTypes = "application/json"
				copy.Host = server.URL
				copy.CAData = ca
				copy.ServerName = ""
				return kubernetes.NewForConfig(copy)
			}
			checks, err := k.Inspect(t.Context(), s)
			if tc.want == "" {
				if err != nil || !allChecksPassed(checks) {
					t.Fatal("actual HTTP flow did not pass", checks, err)
				}
			} else {
				if err == nil || len(checks) == 0 || checks[len(checks)-1].ID != tc.want || checks[len(checks)-1].Status != "failed" {
					t.Fatal("unsafe state accepted", checks, err)
				}
			}
		})
	}
}
func runtimeState(t *testing.T, k *KubeRuntime) RegistrationState {
	t.Helper()
	server := httptest.NewTLSServer(nil)
	defer server.Close()
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	return RegistrationState{EnrollmentID: runtimeTestID, Environment: "edge", Endpoint: k.endpoint(runtimeTestID), Connection: ConnectionRequest{ClusterUID: "cluster-uid-one", CAData: base64.StdEncoding.EncodeToString(ca), PlatformToken: "platform-token", ArgoToken: "argo-token"}}
}

func TestKubeRuntimeRouteOwnershipAndEndpoint(t *testing.T) {
	k := testKubeRuntime(t)
	ctx := t.Context()
	e := store.OnpremEnrollment{ID: runtimeTestID, Environment: "edge", ClusterUID: "cluster-uid-one"}
	endpoint, err := k.Route(ctx, e, RegistrationHostname(e.ID))
	if err != nil || endpoint != k.endpoint(e.ID) {
		t.Fatal("route", endpoint, err)
	}
	route, err := k.cfg.Kube.CoreV1().Services(k.cfg.RouteNamespace).Get(ctx, RegistrationHostname(e.ID), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if route.Spec.Ports[0].Port != 6443 || route.Annotations["tailscale.com/tags"] != "tag:hop-onprem-api-client" {
		t.Fatal("unrestricted route")
	}
	// The operator replaces ExternalName with its cluster-local proxy target.
	route.Spec.ExternalName = "ts-egress.hopcharlie-db-network.svc.cluster.local"
	route.Finalizers = []string{"tailscale.com/finalizer"}
	if _, err = k.cfg.Kube.CoreV1().Services(k.cfg.RouteNamespace).Update(ctx, route, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err = k.Route(ctx, e, RegistrationHostname(e.ID)); err != nil {
		t.Fatal(err)
	}
	route, err = k.cfg.Kube.CoreV1().Services(k.cfg.RouteNamespace).Get(ctx, RegistrationHostname(e.ID), metav1.GetOptions{})
	if err != nil || route.Spec.ExternalName != "ts-egress.hopcharlie-db-network.svc.cluster.local" || len(route.Finalizers) != 1 {
		t.Fatal("heartbeat rewound operator route", err)
	}
	if _, err = k.Route(ctx, e, "other-device"); !errors.Is(err, ErrInvalid) {
		t.Fatal("client endpoint accepted", err)
	}
	route.Labels[registrationOwnerLabel] = "different-owner"
	if _, err = k.cfg.Kube.CoreV1().Services(k.cfg.RouteNamespace).Update(ctx, route, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err = k.Route(ctx, e, RegistrationHostname(e.ID)); !errors.Is(err, store.ErrConflict) {
		t.Fatal("foreign route replaced", err)
	}
	if err = k.Remove(ctx, e.ID, e.Environment); !errors.Is(err, store.ErrConflict) {
		t.Fatal("foreign route removed", err)
	}
}

func TestKubeRuntimePublishRestoresAndRefreshesCredentials(t *testing.T) {
	k := testKubeRuntime(t)
	s := runtimeState(t, k)
	ctx := t.Context()
	if err := k.Publish(ctx, s); err != nil {
		t.Fatal(err)
	}
	secret, err := k.cfg.Kube.CoreV1().Secrets("argocd").Get(ctx, registrationResourceName(s.EnrollmentID), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if string(secret.Data["name"]) != registrationResourceName(s.EnrollmentID) {
		t.Fatal("Argo destination name mismatch")
	}
	s.Connection.PlatformToken = "rotated-token"
	s.Connection.ArgoToken = "rotated-argo-token"
	if err = k.Publish(ctx, s); err != nil {
		t.Fatal(err)
	}
	cfg, ok := k.cfg.Envs.Config("edge")
	if !ok || cfg.RESTConfig.BearerToken != "rotated-token" {
		t.Fatal("runtime credentials not refreshed")
	}
	secret.Labels[registrationOwnerLabel] = "foreign"
	if err = k.cfg.Kube.(*kubefake.Clientset).Tracker().Update(corev1.SchemeGroupVersion.WithResource("secrets"), secret, "argocd"); err != nil {
		t.Fatal(err)
	}
	if err = k.Publish(ctx, s); !errors.Is(err, store.ErrConflict) {
		t.Fatal("foreign Argo credentials replaced", err)
	}
}

func TestKubeRuntimeInspectPinsUIDAndRejectsArbitraryEndpoint(t *testing.T) {
	k := testKubeRuntime(t)
	s := runtimeState(t, k)
	calls := 0
	k.cfg.NewRemoteClient = func(rc *rest.Config) (kubernetes.Interface, error) {
		calls++
		if rc.Insecure || rc.ServerName != "kubernetes.default.svc" || len(rc.CAData) == 0 {
			t.Fatal("TLS verification weakened")
		}
		return kubefake.NewSimpleClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: types.UID("other-cluster")}}), nil
	}
	checks, err := k.Inspect(t.Context(), s)
	if err == nil || len(checks) != 2 || checks[1].Status != "failed" || calls != 1 {
		t.Fatal("different cluster UID accepted", checks, err)
	}
	s.Endpoint = "https://169.254.169.254:6443"
	calls = 0
	if _, err = k.Inspect(t.Context(), s); err == nil || calls != 0 {
		t.Fatal("arbitrary endpoint dialed")
	}
}

func TestKubeRuntimeCreatesRestrictedOwnedProbe(t *testing.T) {
	k := testKubeRuntime(t)
	s := runtimeState(t, k)
	client := kubefake.NewSimpleClientset()
	ns := "hop-check-0123456789abcdef01234567"
	if err := k.ensureProbe(context.Background(), client, s, ns); err != nil {
		t.Fatal(err)
	}
	pod, err := client.CoreV1().Pods(ns).Get(t.Context(), "hello", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken || !(*pod.Spec.SecurityContext.RunAsNonRoot) || pod.Spec.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault || pod.Spec.Containers[0].ImagePullPolicy != corev1.PullAlways {
		t.Fatal("probe privileges or pull policy unsafe")
	}
	namespace, err := client.CoreV1().Namespaces().Get(t.Context(), ns, metav1.GetOptions{})
	if err != nil || namespace.Labels["pod-security.kubernetes.io/enforce"] != "restricted" {
		t.Fatal("probe namespace not restricted", err)
	}
	namespace.Labels[registrationOwnerLabel] = "foreign"
	if _, err = client.CoreV1().Namespaces().Update(t.Context(), namespace, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err = k.ensureProbe(t.Context(), client, s, ns); !errors.Is(err, store.ErrConflict) {
		t.Fatal("foreign probe namespace reused", err)
	}
}
