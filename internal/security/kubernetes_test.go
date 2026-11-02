package security

import (
	"errors"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/version"
	fakediscovery "k8s.io/client-go/discovery/fake"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/envs"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

var gkeEnv = envs.EnvConfig{Name: "gke", Provider: "gcp", KubeContext: "gke-ctx"}

func yes() *bool { b := true; return &b }

func limits() corev1.ResourceRequirements {
	return corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("256Mi")}}
}

// pod builds a running pod owned by a ReplicaSet of deployment, the way the
// Deployment controller names them.
func pod(ns, deployment, name string, spec corev1.PodSpec) *corev1.Pod {
	ctrl := true
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: map[string]string{"pod-template-hash": "5d8f"}},
		Spec:       spec,
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	if deployment != "" {
		p.OwnerReferences = []metav1.OwnerReference{{Kind: "ReplicaSet", Name: deployment + "-5d8f", Controller: &ctrl}}
	}
	return p
}

func newFakeKube(objs ...runtime.Object) *fake.Clientset {
	cs := fake.NewClientset(objs...)
	cs.Discovery().(*fakediscovery.FakeDiscovery).FakedServerVersion = &version.Info{GitVersion: "v1.33.2"}
	return cs
}

func collectKube(t *testing.T, kube kubernetes.Interface) Report {
	t.Helper()
	c := newKubeCollector(gkeEnv, func() (kubernetes.Interface, error) { return kube, nil })
	reps := c.Collect(t.Context())
	if len(reps) != 1 || reps[0].Source != c.Sources()[0] {
		t.Fatalf("reports: %+v", reps)
	}
	return reps[0]
}

func checks(r Report) []string {
	var out []string
	for _, f := range r.Findings {
		out = append(out, string(f.Severity)+" "+f.Check+" "+f.Resource)
	}
	slices.Sort(out)
	return out
}

func TestKubeChecks(t *testing.T) {
	hardened := corev1.Container{Name: "app", Resources: limits(), SecurityContext: &corev1.SecurityContext{RunAsNonRoot: yes()}}
	web := corev1.PodSpec{Containers: []corev1.Container{{Name: "app", SecurityContext: &corev1.SecurityContext{Privileged: yes()}}}}
	kube := newFakeKube(
		// Two replicas of one Deployment give one finding per check.
		pod("shop", "web", "web-5d8f-a", web),
		pod("shop", "web", "web-5d8f-b", web),
		pod("shop", "agent", "agent-5d8f-a", corev1.PodSpec{HostNetwork: true, HostPID: true,
			SecurityContext: &corev1.PodSecurityContext{RunAsNonRoot: yes()},
			Containers:      []corev1.Container{{Name: "agent", Resources: limits()}},
			Volumes:         []corev1.Volume{{Name: "logs", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/var/log"}}}}}),
		// Hardened, in a namespace with a policy: nothing to report.
		pod("payments", "api", "api-5d8f-a", corev1.PodSpec{Containers: []corev1.Container{hardened}}),
		&networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Namespace: "payments", Name: "default-deny"}},
		// System namespaces and finished pods are skipped.
		pod("kube-system", "coredns", "coredns-5d8f-a", web),
		pod("kube-public", "x", "x-5d8f-a", web),
		func() *corev1.Pod { p := pod("batch", "", "done", web); p.Status.Phase = corev1.PodSucceeded; return p }(),
		&rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "cluster-admin"}, RoleRef: rbacv1.RoleRef{Kind: "ClusterRole", Name: "cluster-admin"},
			Subjects: []rbacv1.Subject{{Kind: "Group", Name: "system:masters"}}},
		&rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "ci-admin"}, RoleRef: rbacv1.RoleRef{Kind: "ClusterRole", Name: "cluster-admin"},
			Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Namespace: "ci", Name: "deployer"}, {Kind: "ServiceAccount", Namespace: "kube-system", Name: "helm"}}},
		&rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "oops"}, RoleRef: rbacv1.RoleRef{Kind: "ClusterRole", Name: "cluster-admin"},
			Subjects: []rbacv1.Subject{{Kind: "Group", Name: "system:authenticated"}}},
		&rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "view-all"}, RoleRef: rbacv1.RoleRef{Kind: "ClusterRole", Name: "view"},
			Subjects: []rbacv1.Subject{{Kind: "Group", Name: "devs"}}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web"}, Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer,
			Ports: []corev1.ServicePort{{Port: 80, NodePort: 31080, Protocol: corev1.ProtocolTCP}}}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "debug"}, Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeNodePort}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "db"}, Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "kube-system", Name: "traefik"}, Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer}},
	)
	r := collectKube(t, kube)
	if r.Status != apiv1.SecuritySourceStatusOk || !strings.Contains(r.Message, "v1.33.2") || !strings.Contains(r.Message, "4 running pods") {
		t.Fatalf("status %s: %s", r.Status, r.Message)
	}
	want := []string{
		"critical k8s.cluster-admin ClusterRoleBinding/oops/Group system:authenticated",
		"high k8s.cluster-admin ClusterRoleBinding/ci-admin/ServiceAccount ci/deployer",
		"high k8s.host-pid shop/Deployment/agent",
		"high k8s.privileged shop/Deployment/web/app",
		"low k8s.no-limits shop/Deployment/web/app",
		"low k8s.node-port shop/Service/debug",
		"low k8s.run-as-non-root shop/Deployment/web/app",
		"medium k8s.host-network shop/Deployment/agent",
		"medium k8s.host-path shop/Deployment/agent/logs",
		"medium k8s.load-balancer shop/Service/web",
		"medium k8s.no-network-policy shop",
	}
	if got := checks(r); !slices.Equal(got, want) {
		t.Fatalf("findings:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for _, f := range r.Findings {
		if f.Title == "" || f.Detail == "" || f.Recommendation == "" || !f.Category.Valid() {
			t.Errorf("incomplete finding %+v", f)
		}
	}
}

func TestKubeUnreachableAndPartial(t *testing.T) {
	down := newFakeKube()
	down.Discovery().(*fakediscovery.FakeDiscovery).PrependReactor("get", "version", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("dial tcp 10.0.0.1:6443: connect: connection refused")
	})
	if r := collectKube(t, down); r.Status != apiv1.SecuritySourceStatusUnavailable || !strings.Contains(r.Message, "connection refused") || len(r.Findings) != 0 {
		t.Fatalf("unreachable: %+v", r)
	}

	partial := newFakeKube(&corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web"}, Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeNodePort}})
	partial.PrependReactor("list", "clusterrolebindings", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "rbac.authorization.k8s.io", Resource: "clusterrolebindings"}, "", errors.New("no"))
	})
	r := collectKube(t, partial)
	if r.Status != apiv1.SecuritySourceStatusError || r.Message != "list cluster role bindings: forbidden by RBAC" || len(r.Findings) != 1 {
		t.Fatalf("partial: %+v", r)
	}

	c := newKubeCollector(gkeEnv, func() (kubernetes.Interface, error) {
		return nil, errors.New(`kube context "gke-ctx" not found in kubeconfig`)
	})
	if r := c.Collect(t.Context())[0]; r.Status != apiv1.SecuritySourceStatusNotConfigured || !strings.Contains(r.Message, "gke-ctx") {
		t.Fatalf("no kubeconfig: %+v", r)
	}
}

func no() *bool { b := false; return &b }

func uid(n int64) *int64 { return &n }

// TestKubeHardening covers one rule per case; only the findings of the
// checks named in want are compared.
func TestKubeHardening(t *testing.T) {
	admin := func(name string, subjects ...rbacv1.Subject) *rbacv1.ClusterRoleBinding {
		return &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: name}, RoleRef: rbacv1.RoleRef{Kind: "ClusterRole", Name: "cluster-admin"}, Subjects: subjects}
	}
	ctr := func(name string, sc *corev1.SecurityContext) corev1.Container {
		return corev1.Container{Name: name, Resources: limits(), SecurityContext: sc}
	}
	for _, c := range []struct {
		name   string
		checks []string
		objs   []runtime.Object
		want   []string
	}{
		{"broad subjects are not system subjects", []string{"k8s.cluster-admin"}, []runtime.Object{
			admin("all-sa", rbacv1.Subject{Kind: "Group", Name: "system:serviceaccounts"}),
			admin("shop-sa", rbacv1.Subject{Kind: "Group", Name: "system:serviceaccounts:shop"}),
			admin("everyone", rbacv1.Subject{Kind: "Group", Name: "system:authenticated"}, rbacv1.Subject{Kind: "Group", Name: "system:unauthenticated"}),
			admin("ci", rbacv1.Subject{Kind: "User", Name: "system:serviceaccount:default:ci"}),
			admin("system", rbacv1.Subject{Kind: "Group", Name: "system:masters"}, rbacv1.Subject{Kind: "User", Name: "system:kube-controller-manager"},
				rbacv1.Subject{Kind: "User", Name: "system:serviceaccount:kube-system:helm"}, rbacv1.Subject{Kind: "ServiceAccount", Namespace: "kube-system", Name: "helm"}),
		}, []string{
			"critical k8s.cluster-admin ClusterRoleBinding/all-sa/Group system:serviceaccounts",
			"critical k8s.cluster-admin ClusterRoleBinding/everyone/Group system:authenticated",
			"critical k8s.cluster-admin ClusterRoleBinding/everyone/Group system:unauthenticated",
			"critical k8s.cluster-admin ClusterRoleBinding/shop-sa/Group system:serviceaccounts:shop",
			"high k8s.cluster-admin ClusterRoleBinding/ci/User system:serviceaccount:default:ci",
		}},
		{"container runAsNonRoot false overrides the pod", []string{"k8s.run-as-non-root"}, []runtime.Object{
			pod("shop", "web", "web-5d8f-a", corev1.PodSpec{SecurityContext: &corev1.PodSecurityContext{RunAsNonRoot: yes()},
				Containers: []corev1.Container{ctr("app", nil), ctr("sidecar", &corev1.SecurityContext{RunAsNonRoot: no()})}}),
		}, []string{"low k8s.run-as-non-root shop/Deployment/web/sidecar"}},
		{"a non-zero runAsUser is non-root", []string{"k8s.run-as-non-root"}, []runtime.Object{
			pod("shop", "web", "web-5d8f-a", corev1.PodSpec{SecurityContext: &corev1.PodSecurityContext{RunAsUser: uid(1000)},
				Containers: []corev1.Container{ctr("app", nil), ctr("root", &corev1.SecurityContext{RunAsUser: uid(0)})}}),
			pod("shop", "api", "api-5d8f-a", corev1.PodSpec{Containers: []corev1.Container{ctr("app", &corev1.SecurityContext{RunAsUser: uid(10001), RunAsNonRoot: no()})}}),
		}, []string{"low k8s.run-as-non-root shop/Deployment/web/root"}},
		{"init and ephemeral containers", []string{"k8s.privileged", "k8s.run-as-non-root"}, []runtime.Object{
			pod("shop", "web", "web-5d8f-a", corev1.PodSpec{SecurityContext: &corev1.PodSecurityContext{RunAsNonRoot: yes()},
				InitContainers: []corev1.Container{ctr("migrate", &corev1.SecurityContext{Privileged: yes()})},
				Containers:     []corev1.Container{ctr("app", nil)},
				EphemeralContainers: []corev1.EphemeralContainer{{EphemeralContainerCommon: corev1.EphemeralContainerCommon{Name: "debugger",
					SecurityContext: &corev1.SecurityContext{Privileged: yes(), RunAsNonRoot: no()}}}}}),
		}, []string{
			"high k8s.privileged shop/Deployment/web/debugger",
			"high k8s.privileged shop/Deployment/web/migrate",
			"low k8s.run-as-non-root shop/Deployment/web/debugger",
		}},
		{"host IPC", []string{"k8s.host-ipc"}, []runtime.Object{
			pod("shop", "agent", "agent-5d8f-a", corev1.PodSpec{HostIPC: true, Containers: []corev1.Container{ctr("agent", nil)}}),
		}, []string{"high k8s.host-ipc shop/Deployment/agent"}},
		{"system namespaces are an exact list", []string{"k8s.privileged", "k8s.cluster-admin"}, []runtime.Object{
			pod("kube-system", "dns", "dns-5d8f-a", corev1.PodSpec{Containers: []corev1.Container{ctr("dns", &corev1.SecurityContext{Privileged: yes()})}}),
			pod("kube-node-lease", "x", "x-5d8f-a", corev1.PodSpec{Containers: []corev1.Container{ctr("x", &corev1.SecurityContext{Privileged: yes()})}}),
			pod("kube-tools", "miner", "miner-5d8f-a", corev1.PodSpec{Containers: []corev1.Container{ctr("miner", &corev1.SecurityContext{Privileged: yes()})}}),
			pod("gke-extras", "x", "x-5d8f-a", corev1.PodSpec{Containers: []corev1.Container{ctr("x", &corev1.SecurityContext{Privileged: yes()})}}),
			admin("tools", rbacv1.Subject{Kind: "ServiceAccount", Namespace: "kube-tools", Name: "miner"}),
		}, []string{
			"high k8s.cluster-admin ClusterRoleBinding/tools/ServiceAccount kube-tools/miner",
			"high k8s.privileged gke-extras/Deployment/x/x",
			"high k8s.privileged kube-tools/Deployment/miner/miner",
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := collectKube(t, newFakeKube(c.objs...))
			if r.Status != apiv1.SecuritySourceStatusOk {
				t.Fatalf("status %s: %s", r.Status, r.Message)
			}
			var got []string
			for _, f := range checks(r) {
				if check := strings.Fields(f)[1]; slices.Contains(c.checks, check) {
					got = append(got, f)
				}
			}
			if !slices.Equal(got, c.want) {
				t.Fatalf("findings:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(c.want, "\n"))
			}
		})
	}
}
