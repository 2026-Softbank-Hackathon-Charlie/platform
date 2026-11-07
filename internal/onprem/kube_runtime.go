package onprem

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/envs"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	authv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const registrationOwnerLabel = "hopcharlie.dev/onprem-enrollment"
const registrationUIDLabel = "hopcharlie.dev/onprem-cluster-uid"

type KubeRuntimeConfig struct {
	Kube                                                                 kubernetes.Interface
	Dynamic                                                              dynamic.Interface
	Envs                                                                 *envs.Service
	TailnetDomain, RouteNamespace, ArgoNamespace, ProxyClass, HelloImage string
	Profile                                                              map[string]any
	NewRemoteClient                                                      func(*rest.Config) (kubernetes.Interface, error)
}
type KubeRuntime struct{ cfg KubeRuntimeConfig }

func NewKubeRuntime(cfg KubeRuntimeConfig) *KubeRuntime { return &KubeRuntime{cfg: cfg} }

var tailnetDomainPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]*\.ts\.net$`)
var pinnedImagePattern = regexp.MustCompile(`^[A-Za-z0-9./:_-]+@sha256:[a-f0-9]{64}$`)

func (k *KubeRuntime) Configured(context.Context) error {
	c := k.cfg
	if c.Kube == nil || c.Dynamic == nil || c.Envs == nil || !tailnetDomainPattern.MatchString(c.TailnetDomain) || c.RouteNamespace != "hopcharlie-db-network" || c.ArgoNamespace != "argocd" || c.ProxyClass != "hopcharlie-onprem-api-egress-management" || !pinnedImagePattern.MatchString(c.HelloImage) || len(c.Profile) == 0 {
		return ErrUnavailable
	}
	return nil
}
func (k *KubeRuntime) endpoint(id string) string {
	return "https://" + RegistrationHostname(id) + "." + k.cfg.RouteNamespace + ".svc.cluster.local:6443"
}
func (k *KubeRuntime) Route(ctx context.Context, e store.OnpremEnrollment, hostname string) (string, error) {
	if err := k.Configured(ctx); err != nil {
		return "", err
	}
	if !idPattern.MatchString(e.ID) || hostname != RegistrationHostname(e.ID) || e.ClusterUID == "" {
		return "", ErrInvalid
	}
	labels := map[string]string{registrationOwnerLabel: e.ID, registrationUIDLabel: e.ClusterUID}
	annotations := map[string]string{"tailscale.com/tailnet-fqdn": hostname + "." + k.cfg.TailnetDomain, "tailscale.com/proxy-class": k.cfg.ProxyClass, "tailscale.com/tags": "tag:hop-onprem-api-client"}
	services := k.cfg.Kube.CoreV1().Services(k.cfg.RouteNamespace)
	desired := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: hostname, Namespace: k.cfg.RouteNamespace, Labels: labels, Annotations: annotations}, Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeExternalName, ExternalName: hostname + "." + k.cfg.TailnetDomain, Ports: []corev1.ServicePort{{Name: "kubernetes", Port: 6443, Protocol: corev1.ProtocolTCP}}}}
	old, err := services.Get(ctx, hostname, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = services.Create(ctx, desired, metav1.CreateOptions{})
		if err != nil {
			return "", errors.New("onprem API route create failed")
		}
		return k.endpoint(e.ID), nil
	}
	if err != nil {
		return "", errors.New("onprem API route lookup failed")
	}
	if old.Labels[registrationOwnerLabel] != e.ID || old.Labels[registrationUIDLabel] != e.ClusterUID {
		return "", store.ErrConflict
	}
	// Tailscale rewrites ExternalName to its proxy Service. Keep that target,
	// finalizers and other operator-managed state on every credential heartbeat.
	if old.Spec.Type != corev1.ServiceTypeExternalName || len(old.Spec.Ports) != 1 || old.Spec.Ports[0].Port != 6443 {
		return "", store.ErrConflict
	}
	next := old.DeepCopy()
	changed := false
	if next.Annotations == nil {
		next.Annotations = map[string]string{}
	}
	for name, value := range annotations {
		if next.Annotations[name] != value {
			next.Annotations[name] = value
			changed = true
		}
	}
	if changed {
		if _, err = services.Update(ctx, next, metav1.UpdateOptions{}); err != nil {
			return "", errors.New("onprem API route update failed")
		}
	}
	return k.endpoint(e.ID), nil
}

func (k *KubeRuntime) remoteConfig(s RegistrationState, token string) (*rest.Config, error) {
	if !idPattern.MatchString(s.EnrollmentID) || !namePattern.MatchString(s.Environment) || s.Endpoint != k.endpoint(s.EnrollmentID) {
		return nil, ErrInvalid
	}
	ca, err := base64.StdEncoding.DecodeString(s.Connection.CAData)
	if err != nil {
		return nil, ErrInvalid
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, ErrInvalid
	}
	return &rest.Config{Host: s.Endpoint, BearerToken: token, Timeout: 15 * time.Second, TLSClientConfig: rest.TLSClientConfig{CAData: ca, ServerName: "kubernetes.default.svc"}}, nil
}
func (k *KubeRuntime) client(rc *rest.Config) (kubernetes.Interface, error) {
	if k.cfg.NewRemoteClient != nil {
		return k.cfg.NewRemoteClient(rc)
	}
	return kubernetes.NewForConfig(rc)
}

func (k *KubeRuntime) Inspect(ctx context.Context, s RegistrationState) ([]store.OnpremEnrollmentCheck, error) {
	checks := []store.OnpremEnrollmentCheck{}
	add := func(id, status, message string) {
		stamp := time.Now().UTC()
		checks = append(checks, store.OnpremEnrollmentCheck{ID: id, Status: status, Message: message, CheckedAt: &stamp})
	}
	fail := func(id, message string) ([]store.OnpremEnrollmentCheck, error) {
		add(id, "failed", message)
		return checks, errors.New(message)
	}
	cfg, err := k.remoteConfig(s, s.Connection.PlatformToken)
	if err != nil {
		return fail("network", "클러스터 TLS 설정을 확인할 수 없습니다.")
	}
	observer, err := k.client(cfg)
	if err != nil {
		return fail("network", "클러스터 관측 연결을 구성하지 못했습니다.")
	}
	if _, err = observer.Discovery().ServerVersionWithContext(ctx); err != nil {
		return fail("network", "관리 서버에서 클러스터 API에 인증된 TLS로 연결하지 못했습니다.")
	}
	add("network", "passed", "관리 서버의 인증된 TLS API 연결을 확인했습니다.")
	ns, err := observer.CoreV1().Namespaces().Get(ctx, "kube-system", metav1.GetOptions{})
	if err != nil || string(ns.UID) != s.Connection.ClusterUID {
		return fail("k3s", "실제 클러스터 UID가 등록 요청과 일치하지 않습니다.")
	}
	nodes, err := observer.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil || len(nodes.Items) == 0 {
		return fail("k3s", "클러스터 노드를 확인하지 못했습니다.")
	}
	ready := false
	for _, n := range nodes.Items {
		for _, c := range n.Status.Conditions {
			if c.Type == corev1.NodeReady && c.Status == corev1.ConditionTrue {
				ready = true
			}
		}
	}
	if !ready {
		return fail("k3s", "Ready 상태인 노드가 없습니다.")
	}
	add("k3s", "passed", "실제 클러스터 UID와 Ready 노드를 확인했습니다.")
	argoCfg, err := k.remoteConfig(s, s.Connection.ArgoToken)
	if err != nil {
		return fail("permissions", "배포 인증 정보를 확인하지 못했습니다.")
	}
	deployer, err := k.client(argoCfg)
	if err != nil {
		return fail("permissions", "배포 권한 연결을 구성하지 못했습니다.")
	}
	probeNS := "hop-check-" + strings.TrimPrefix(s.EnrollmentID, "enr_")
	for _, r := range []struct {
		a    authv1.ResourceAttributes
		want bool
	}{
		{authv1.ResourceAttributes{Verb: "create", Resource: "namespaces"}, true},
		{authv1.ResourceAttributes{Verb: "create", Group: "apps", Resource: "deployments", Namespace: probeNS}, true},
		{authv1.ResourceAttributes{Verb: "create", Resource: "pods", Namespace: probeNS}, true},
		{authv1.ResourceAttributes{Verb: "get", Resource: "pods", Subresource: "proxy", Namespace: probeNS}, true},
		{authv1.ResourceAttributes{Verb: "get", Resource: "secrets", Namespace: "kube-system"}, false},
		{authv1.ResourceAttributes{Verb: "create", Group: "rbac.authorization.k8s.io", Resource: "clusterrolebindings"}, false},
	} {
		review, err := deployer.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, &authv1.SelfSubjectAccessReview{Spec: authv1.SelfSubjectAccessReviewSpec{ResourceAttributes: &r.a}}, metav1.CreateOptions{})
		if err != nil || review.Status.Allowed != r.want || review.Status.EvaluationError != "" {
			return fail("permissions", "배포 필수 권한 또는 시스템·RBAC 접근 차단 검사를 통과하지 못했습니다.")
		}
	}
	// SSAR evaluates RBAC, not admission. This dry-run must hit this exact
	// enrollment's protected-namespace policy before delegated credentials publish.
	policyName := "hop-" + strings.TrimPrefix(s.EnrollmentID, "enr_") + "-argo"
	_, denied := deployer.CoreV1().ConfigMaps("default").Create(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: probeNS, Namespace: "default"}}, metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}})
	if denied == nil || (!apierrors.IsForbidden(denied) && !apierrors.IsInvalid(denied)) || !strings.Contains(denied.Error(), policyName) {
		return fail("permissions", "보호된 시스템 네임스페이스 변경을 차단하는 실제 admission 정책을 확인하지 못했습니다.")
	}
	add("permissions", "passed", "배포 권한과 시스템 시크릿·RBAC·보호 네임스페이스 변경 거부를 실제 확인했습니다.")
	if err = k.ensureProbe(ctx, deployer, s, probeNS); err != nil {
		return fail("image_pull", "서버 주도 검증 Pod를 준비하지 못했습니다.")
	}
	waitCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	var pod *corev1.Pod
	for {
		pod, err = deployer.CoreV1().Pods(probeNS).Get(waitCtx, "hello", metav1.GetOptions{})
		if err == nil && (pod.Labels[registrationOwnerLabel] != s.EnrollmentID || pod.Labels[registrationUIDLabel] != s.Connection.ClusterUID) {
			return fail("image_pull", "검증 Pod 소유권이 일치하지 않습니다.")
		}
		pulled, healthy := false, false
		if err == nil {
			for _, c := range pod.Status.ContainerStatuses {
				if c.Name == "hello" && c.ImageID != "" {
					pulled = true
					healthy = c.Ready
				}
			}
		}
		if pulled && healthy && pod.UID != "" {
			break
		}
		select {
		case <-waitCtx.Done():
			add("image_pull", "pending", "실제 이미지 다운로드와 Pod 준비를 기다립니다.")
			return checks, errors.New("onprem probe not ready")
		case <-time.After(time.Second):
		}
	}
	add("image_pull", "passed", "고정 digest 이미지의 다운로드와 실제 Ready 컨테이너를 확인했습니다.")
	response, err := deployer.CoreV1().RESTClient().Get().Namespace(probeNS).Resource("pods").Name("hello:8080").SubResource("proxy").Stream(ctx)
	if err != nil {
		return fail("hello_world", "실제 검증 Pod의 HTTP 응답을 읽지 못했습니다.")
	}
	body, err := io.ReadAll(io.LimitReader(response, 513))
	response.Close()
	if err != nil || len(body) > 512 || !bytes.Equal(body, []byte("Hello World! "+s.EnrollmentID+" "+string(pod.UID)+"\n")) {
		return fail("hello_world", "실제 검증 Pod의 HTTP 응답이 등록 ID와 일치하지 않습니다.")
	}
	add("hello_world", "passed", "관리 서버가 해당 등록 장비의 HTTP 응답을 직접 확인했습니다.")
	return checks, nil
}

func (k *KubeRuntime) ensureProbe(ctx context.Context, client kubernetes.Interface, s RegistrationState, namespace string) error {
	labels := map[string]string{registrationOwnerLabel: s.EnrollmentID, registrationUIDLabel: s.Connection.ClusterUID, "pod-security.kubernetes.io/enforce": "restricted", "pod-security.kubernetes.io/audit": "restricted", "pod-security.kubernetes.io/warn": "restricted"}
	ns, err := client.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		ns, err = client.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace, Labels: labels}}, metav1.CreateOptions{})
	}
	if err != nil {
		return errors.New("probe namespace unavailable")
	}
	if ns.Labels[registrationOwnerLabel] != s.EnrollmentID || ns.Labels[registrationUIDLabel] != s.Connection.ClusterUID {
		return store.ErrConflict
	}
	old, err := client.CoreV1().Pods(namespace).Get(ctx, "hello", metav1.GetOptions{})
	if err == nil {
		if old.Labels[registrationOwnerLabel] != s.EnrollmentID || old.Labels[registrationUIDLabel] != s.Connection.ClusterUID || len(old.Spec.Containers) != 1 || old.Spec.Containers[0].Image != k.cfg.HelloImage {
			return store.ErrConflict
		}
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return errors.New("probe pod unavailable")
	}
	no, yes, uid := false, true, int64(10001)
	// This fixed program binds the HTTP proof to one server-generated enrollment.
	program := fmt.Sprintf("import os\nfrom http.server import BaseHTTPRequestHandler,HTTPServer\nclass H(BaseHTTPRequestHandler):\n def do_GET(self):\n  b=(%q+os.environ['POD_UID']+'\\n').encode();self.send_response(200);self.end_headers();self.wfile.write(b)\n def log_message(self,*a): pass\nHTTPServer(('0.0.0.0',8080),H).serve_forever()", "Hello World! "+s.EnrollmentID+" ")
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "hello", Namespace: namespace, Labels: labels}, Spec: corev1.PodSpec{AutomountServiceAccountToken: &no, EnableServiceLinks: &no, RestartPolicy: corev1.RestartPolicyAlways, SecurityContext: &corev1.PodSecurityContext{RunAsNonRoot: &yes, RunAsUser: &uid, SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}}, Containers: []corev1.Container{{Name: "hello", Env: []corev1.EnvVar{{Name: "POD_UID", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.uid"}}}}, Image: k.cfg.HelloImage, ImagePullPolicy: corev1.PullAlways, Command: []string{"python", "-I", "-u", "-c", program}, Ports: []corev1.ContainerPort{{Name: "http", ContainerPort: 8080}}, SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: &no, ReadOnlyRootFilesystem: &yes, Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}}, Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m"), corev1.ResourceMemory: resource.MustParse("16Mi")}, Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("200m"), corev1.ResourceMemory: resource.MustParse("64Mi")}}}}}}
	_, err = client.CoreV1().Pods(namespace).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		return errors.New("probe pod create failed")
	}
	return nil
}

func (k *KubeRuntime) Publish(ctx context.Context, s RegistrationState) error {
	if err := k.Configured(ctx); err != nil {
		return err
	}
	cfg, err := k.remoteConfig(s, s.Connection.PlatformToken)
	if err != nil {
		return err
	}
	if owner, uid, ok := k.cfg.Envs.ConnectionIdentity(s.Environment); ok && (owner != s.EnrollmentID || uid != s.Connection.ClusterUID) {
		return store.ErrConflict
	}
	ca, err := base64.StdEncoding.DecodeString(s.Connection.CAData)
	if err != nil {
		return ErrInvalid
	}
	secretConfig, err := json.Marshal(map[string]any{"bearerToken": s.Connection.ArgoToken, "tlsClientConfig": map[string]any{"insecure": false, "caData": base64.StdEncoding.EncodeToString(ca), "serverName": "kubernetes.default.svc"}})
	if err != nil {
		return ErrInvalid
	}
	name := registrationResourceName(s.EnrollmentID)
	desired := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: k.cfg.ArgoNamespace, Labels: map[string]string{"argocd.argoproj.io/secret-type": "cluster", registrationOwnerLabel: s.EnrollmentID, registrationUIDLabel: s.Connection.ClusterUID}}, Data: map[string][]byte{"name": []byte(name), "server": []byte(s.Endpoint), "config": secretConfig, "clusterResources": []byte("true")}}
	secrets := k.cfg.Kube.CoreV1().Secrets(k.cfg.ArgoNamespace)
	old, err := secrets.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = secrets.Create(ctx, desired, metav1.CreateOptions{})
	} else if err == nil {
		if old.Labels[registrationOwnerLabel] != s.EnrollmentID || old.Labels[registrationUIDLabel] != s.Connection.ClusterUID || string(old.Data["server"]) != s.Endpoint {
			return store.ErrConflict
		}
		desired.ResourceVersion = old.ResourceVersion
		desired.UID = old.UID
		_, err = secrets.Update(ctx, desired, metav1.UpdateOptions{})
	}
	if err != nil {
		return errors.New("Argo cluster credential registration failed")
	}
	if err = EnsureEnvironmentApplications(ctx, k.cfg.Dynamic, k.cfg.ArgoNamespace, s, k.cfg.Profile); err != nil {
		return err
	}
	return k.cfg.Envs.RegisterConnection(s.Environment, s.EnrollmentID, s.Connection.ClusterUID, cfg)
}

func (k *KubeRuntime) Remove(ctx context.Context, id, environment string) error {
	if !idPattern.MatchString(id) {
		return ErrInvalid
	}
	// Drop the platform's active capability first, even when external cleanup
	// cannot finish. Foreign ownership is still rejected before any deletion.
	if err := k.cfg.Envs.RemoveConnection(environment, id); err != nil {
		return err
	}
	secretName := "hop-onprem-" + strings.TrimPrefix(id, "enr_")
	secret, err := k.cfg.Kube.CoreV1().Secrets(k.cfg.ArgoNamespace).Get(ctx, secretName, metav1.GetOptions{})
	if err == nil {
		if secret.Labels[registrationOwnerLabel] != id {
			return store.ErrConflict
		}
		uid, rv := secret.UID, secret.ResourceVersion
		err = k.cfg.Kube.CoreV1().Secrets(k.cfg.ArgoNamespace).Delete(ctx, secretName, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &rv}})
	}
	if err != nil && !apierrors.IsNotFound(err) {
		return errors.New("Argo credential revocation failed")
	}
	routeName := RegistrationHostname(id)
	route, err := k.cfg.Kube.CoreV1().Services(k.cfg.RouteNamespace).Get(ctx, routeName, metav1.GetOptions{})
	if err == nil {
		if route.Labels[registrationOwnerLabel] != id {
			return store.ErrConflict
		}
		uid, rv := route.UID, route.ResourceVersion
		err = k.cfg.Kube.CoreV1().Services(k.cfg.RouteNamespace).Delete(ctx, routeName, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &rv}})
	}
	if err != nil && !apierrors.IsNotFound(err) {
		return errors.New("onprem API route revocation failed")
	}
	return nil
}
