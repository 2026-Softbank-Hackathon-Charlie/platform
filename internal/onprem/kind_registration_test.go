//go:build onpremkind

package onprem

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	authnv1 "k8s.io/api/authentication/v1"
	authzv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// This validates the real local bootstrap RBAC/admission/image/HTTP path. It
// deliberately does not start the Tailscale Deployment or claim cloud/Argo proof.
func TestRealBootstrapOnDisposableKind(t *testing.T) {
	path := os.Getenv("HOPCHARLIE_ONPREM_TEST_KUBECONFIG")
	if path == "" {
		t.Skip("requires explicit disposable kind kubeconfig")
	}
	raw, err := clientcmd.LoadFromFile(path)
	if err != nil {
		t.Fatal("test kubeconfig load failed")
	}
	if raw.CurrentContext != "kind-onprem-registration" {
		t.Fatal("only the dedicated kind-onprem-registration context is allowed")
	}
	adminConfig, err := clientcmd.NewNonInteractiveClientConfig(*raw, raw.CurrentContext, &clientcmd.ConfigOverrides{}, nil).ClientConfig()
	if err != nil {
		t.Fatal("test client config failed")
	}
	adminConfig.Timeout = 20 * time.Second
	admin, err := kubernetes.NewForConfig(adminConfig)
	if err != nil {
		t.Fatal(err)
	}
	dc, err := dynamic.NewForConfig(adminConfig)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	random := make([]byte, 12)
	if _, err = rand.Read(random); err != nil {
		t.Fatal(err)
	}
	id := "enr_" + hex.EncodeToString(random)
	suffix := strings.TrimPrefix(id, "enr_")
	r := testKubeRuntime(t)
	manifest, err := BootstrapManifest(BootstrapConfig{EnrollmentID: id, Origin: "https://api.invalid.test", ConnectionToken: "onc_" + strings.Repeat("x", 43), Hostname: RegistrationHostname(id), TailnetAuthKey: "tskey-auth-test-only-not-issued", AgentImage: r.cfg.HelloImage, TailscaleImage: r.cfg.HelloImage, AgentArch: "amd64", AgentURL: "https://api.invalid.test/v1/onprem/agent.bin?arch=amd64", AgentSHA256: strings.Repeat("0", 64)})
	if err != nil {
		t.Fatal(err)
	}
	var list struct {
		Items []map[string]any `json:"items"`
	}
	if json.Unmarshal(manifest, &list) != nil {
		t.Fatal("bootstrap List decode failed")
	}
	access := map[string]schema.GroupVersionResource{
		"Namespace": {Version: "v1", Resource: "namespaces"}, "ServiceAccount": {Version: "v1", Resource: "serviceaccounts"},
		"Role": {Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "roles"}, "RoleBinding": {Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "rolebindings"},
		"ClusterRole": {Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles"}, "ClusterRoleBinding": {Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterrolebindings"},
		"ValidatingAdmissionPolicy": {Group: "admissionregistration.k8s.io", Version: "v1", Resource: "validatingadmissionpolicies"}, "ValidatingAdmissionPolicyBinding": {Group: "admissionregistration.k8s.io", Version: "v1", Resource: "validatingadmissionpolicybindings"},
	}
	// Only access objects are installed: no fake Tailnet key/connection Secret or Deployment.
	for _, item := range list.Items {
		obj := &unstructured.Unstructured{Object: item}
		gvr, ok := access[obj.GetKind()]
		if !ok {
			continue
		}
		var api dynamic.ResourceInterface = dc.Resource(gvr)
		if obj.GetNamespace() != "" {
			api = dc.Resource(gvr).Namespace(obj.GetNamespace())
		}
		if obj.GetKind() == "Namespace" {
			if _, getErr := api.Get(ctx, obj.GetName(), metav1.GetOptions{}); getErr == nil {
				continue
			} else if !apierrors.IsNotFound(getErr) {
				t.Fatal("test namespace lookup failed")
			}
		}
		created, err := api.Create(ctx, obj, metav1.CreateOptions{})
		if err != nil {
			t.Fatalf("bootstrap access create %s failed: %v", obj.GetKind(), err)
		}
		apiCopy, name, uid := api, created.GetName(), created.GetUID()
		t.Cleanup(func() {
			cleanupCtx, stop := context.WithTimeout(context.Background(), 15*time.Second)
			defer stop()
			_ = apiCopy.Delete(cleanupCtx, name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}})
		})
	}
	system, err := admin.CoreV1().Namespaces().Get(ctx, "kube-system", metav1.GetOptions{})
	if err != nil {
		t.Fatal("cluster identity read failed")
	}
	// Admin bootstraps only the local agent identity. The delegated tokens
	// must be requested using that identity, proving the actual renewal RBAC.
	ttl := int64(3600)
	token := func(client kubernetes.Interface, name string) string {
		v, err := client.CoreV1().ServiceAccounts("charlie-system").CreateToken(ctx, name, &authnv1.TokenRequest{Spec: authnv1.TokenRequestSpec{ExpirationSeconds: &ttl}}, metav1.CreateOptions{})
		if err != nil {
			t.Fatalf("delegated TokenRequest failed: %v", err)
		}
		return v.Status.Token
	}
	ca := adminConfig.CAData
	if len(ca) == 0 && adminConfig.CAFile != "" {
		ca, err = os.ReadFile(adminConfig.CAFile)
		if err != nil {
			t.Fatal("kind CA could not be read")
		}
	}
	clientForToken := func(value string) kubernetes.Interface {
		// Construct from scratch: copying the admin config would retain its
		// client certificate and silently test as administrator instead.
		client, err := kubernetes.NewForConfig(&rest.Config{Host: adminConfig.Host, BearerToken: value, Timeout: 20 * time.Second, TLSClientConfig: rest.TLSClientConfig{CAData: ca}})
		if err != nil {
			t.Fatal("delegated client configuration failed")
		}
		return client
	}
	agentClient := clientForToken(token(admin, "hop-"+suffix+"-agent"))
	platformToken, argoToken := token(agentClient, "hop-"+suffix+"-platform"), token(agentClient, "hop-"+suffix+"-argo")
	third, err := admin.CoreV1().ServiceAccounts("charlie-system").Create(ctx, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "hop-" + suffix + "-third"}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal("third identity fixture failed")
	}
	t.Cleanup(func() {
		stopCtx, stop := context.WithTimeout(context.Background(), 15*time.Second)
		defer stop()
		uid := third.UID
		_ = admin.CoreV1().ServiceAccounts("charlie-system").Delete(stopCtx, third.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}})
	})
	if _, err := agentClient.CoreV1().ServiceAccounts("charlie-system").CreateToken(ctx, third.Name, &authnv1.TokenRequest{Spec: authnv1.TokenRequestSpec{ExpirationSeconds: &ttl}}, metav1.CreateOptions{}); !apierrors.IsForbidden(err) {
		t.Fatal("agent could mint a third ServiceAccount token, or denial was not RBAC Forbidden")
	}
	t.Log("actual local agent minted only its two delegated tokens; a third TokenRequest was Forbidden")
	state := RegistrationState{EnrollmentID: id, Environment: "kind-edge-" + suffix[:8], Endpoint: r.endpoint(id), Connection: ConnectionRequest{ClusterUID: string(system.UID), CAData: base64.StdEncoding.EncodeToString(ca), PlatformToken: platformToken, ArgoToken: argoToken, TokenExpiresAt: time.Now().Add(time.Hour)}}
	r.cfg.NewRemoteClient = func(requestConfig *rest.Config) (kubernetes.Interface, error) {
		// Change only test routing. Keep the bootstrap token, CA and kube API TLS SAN.
		copy := rest.CopyConfig(requestConfig)
		copy.Host = adminConfig.Host
		copy.Timeout = 20 * time.Second
		if copy.Insecure || copy.ServerName != "kubernetes.default.svc" {
			t.Fatal("test weakened TLS validation")
		}
		return kubernetes.NewForConfig(copy)
	}
	probeNamespace := "hop-check-" + suffix
	t.Cleanup(func() {
		cleanupCtx, stop := context.WithTimeout(context.Background(), 15*time.Second)
		defer stop()
		ns, getErr := admin.CoreV1().Namespaces().Get(cleanupCtx, probeNamespace, metav1.GetOptions{})
		if getErr == nil && ns.Labels[registrationOwnerLabel] == id {
			uid := ns.UID
			_ = admin.CoreV1().Namespaces().Delete(cleanupCtx, probeNamespace, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}})
		}
	})
	// A restricted namespace alone is not ownership. Test against real strong
	// identities in both a foreign namespace and this enrollment's namespace.
	foreignNamespace := probeNamespace + "-foreign"
	for _, entry := range []struct{ name, owner string }{{probeNamespace, id}, {foreignNamespace, "enr_foreign"}} {
		ns, err := admin.CoreV1().Namespaces().Get(ctx, entry.name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			ns, err = admin.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: entry.name, Labels: map[string]string{registrationOwnerLabel: entry.owner, registrationUIDLabel: string(system.UID), "pod-security.kubernetes.io/enforce": "restricted"}}}, metav1.CreateOptions{})
		}
		if err != nil {
			t.Fatal("restricted namespace fixture failed")
		}
		if ns.Labels[registrationOwnerLabel] != entry.owner || ns.Labels["pod-security.kubernetes.io/enforce"] != "restricted" {
			t.Fatal("bootstrap namespace owner or security labels differ")
		}
		if entry.name == foreignNamespace {
			t.Cleanup(func() {
				stopCtx, stop := context.WithTimeout(context.Background(), 15*time.Second)
				defer stop()
				uid := ns.UID
				_ = admin.CoreV1().Namespaces().Delete(stopCtx, ns.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}})
			})
		}
		if _, err = admin.CoreV1().ServiceAccounts(entry.name).Create(ctx, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "strong"}}, metav1.CreateOptions{}); err != nil {
			t.Fatal("strong identity fixture failed")
		}
	}
	strong, err := admin.RbacV1().ClusterRoleBindings().Create(ctx, &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "hop-" + suffix + "-strong-test"}, RoleRef: rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "cluster-admin"}, Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Name: "strong", Namespace: probeNamespace}, {Kind: "ServiceAccount", Name: "strong", Namespace: foreignNamespace}}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal("strong role fixture failed")
	}
	t.Cleanup(func() {
		stopCtx, stop := context.WithTimeout(context.Background(), 15*time.Second)
		defer stop()
		uid := strong.UID
		_ = admin.RbacV1().ClusterRoleBindings().Delete(stopCtx, strong.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}})
	})
	deployer := clientForToken(argoToken)
	for _, entry := range []struct {
		namespace, name string
		allowed         bool
	}{{probeNamespace, "hello:8080", true}, {probeNamespace, "other:8080", false}, {"kube-system", "hello:8080", false}, {foreignNamespace, "hello:8080", false}} {
		review, err := deployer.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, &authzv1.SelfSubjectAccessReview{Spec: authzv1.SelfSubjectAccessReviewSpec{ResourceAttributes: &authzv1.ResourceAttributes{Verb: "get", Resource: "pods", Subresource: "proxy", Namespace: entry.namespace, Name: entry.name}}}, metav1.CreateOptions{})
		if err != nil || review.Status.EvaluationError != "" || review.Status.Allowed != entry.allowed {
			t.Fatal("Pod proxy access is not limited to this enrollment's named hello Pod")
		}
	}
	t.Log("real RBAC allowed only the enrollment's hello Pod proxy; other Pod, foreign namespace and kube-system proxy were denied")
	no, yes, uid := false, true, int64(10001)
	basePod := func(namespace string) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "denied-check", Namespace: namespace}, Spec: corev1.PodSpec{AutomountServiceAccountToken: &no, SecurityContext: &corev1.PodSecurityContext{RunAsNonRoot: &yes, RunAsUser: &uid, SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}}, Containers: []corev1.Container{{Name: "check", Image: r.cfg.HelloImage, SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: &no, Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}}}}}}
	}
	foreignPod := basePod(foreignNamespace)
	foreignPod.Spec.ServiceAccountName = "strong"
	foreignDefaultPod := basePod(foreignNamespace)
	ownedStrongPod := basePod(probeNamespace)
	ownedStrongPod.Spec.ServiceAccountName = "strong"
	projectedPod := basePod(probeNamespace)
	projectedPod.Spec.Volumes = []corev1.Volume{{Name: "credential", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{Sources: []corev1.VolumeProjection{{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{Path: "token", ExpirationSeconds: &ttl}}}}}}}
	automountPod := basePod(probeNamespace)
	automountPod.Spec.AutomountServiceAccountToken = nil
	for _, entry := range []struct {
		name   string
		pod    *corev1.Pod
		policy string
	}{{"foreign restricted namespace with default identity", foreignDefaultPod, "hop-" + suffix + "-argo"}, {"foreign restricted namespace with strong identity", foreignPod, "hop-" + suffix + "-argo"}, {"owned namespace with non-default identity", ownedStrongPod, "hop-" + suffix + "-argo-workloads"}, {"explicit projected token", projectedPod, "hop-" + suffix + "-argo-workloads"}, {"implicit token automount", automountPod, "hop-" + suffix + "-argo-workloads"}} {
		// Prove that RBAC permits this operation first. A generic Forbidden
		// would not prove the owner/credential admission policy was installed.
		access, accessErr := deployer.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, &authzv1.SelfSubjectAccessReview{Spec: authzv1.SelfSubjectAccessReviewSpec{ResourceAttributes: &authzv1.ResourceAttributes{Verb: "create", Resource: "pods", Namespace: entry.pod.Namespace}}}, metav1.CreateOptions{})
		if accessErr != nil || !access.Status.Allowed || access.Status.EvaluationError != "" {
			t.Fatal("negative admission fixture does not have Pod-create RBAC")
		}
		deadline := time.Now().Add(30 * time.Second)
		for {
			_, denyErr := deployer.CoreV1().Pods(entry.pod.Namespace).Create(ctx, entry.pod, metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}})
			if denyErr != nil && (apierrors.IsForbidden(denyErr) || apierrors.IsInvalid(denyErr)) && (strings.Contains(denyErr.Error(), "'"+entry.policy+"'") || strings.Contains(denyErr.Error(), "\""+entry.policy+"\"")) {
				break
			}
			if time.Now().After(deadline) || ctx.Err() != nil {
				t.Fatalf("admission did not reject %s with the enrollment policy", entry.name)
			}
			select {
			case <-ctx.Done():
				t.Fatal("admission fixture timed out")
			case <-time.After(time.Second):
			}
		}
		t.Log("real admission rejected " + entry.name)
	}
	for {
		observed, inspectErr := r.Inspect(ctx, state)
		if inspectErr == nil && allChecksPassed(observed) {
			t.Log("real bootstrap RBAC, protected-namespace admission denial, image download and Pod-UID HTTP proof passed; Tailnet/management Argo not exercised")
			break
		}
		if ctx.Err() != nil {
			t.Fatalf("real kind registration did not verify: checks=%+v error=%v", observed, inspectErr)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("real kind registration timed out: checks=%+v error=%v", observed, inspectErr)
		case <-time.After(2 * time.Second):
		}
	}
}
