package security

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/envs"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

// KubeRequestTimeout bounds one Kubernetes API request.
const KubeRequestTimeout = 5 * time.Second

// systemNamespaces are run by Kubernetes or the cloud provider, not by the
// team; their workloads are skipped. systemPrefixes covers the managed
// add-on namespaces of GKE (gke-*, gmp-*) and EKS (amazon-*, eks-*).
var (
	systemNamespaces = []string{"kube-system", "kube-public", "kube-node-lease"}
	systemPrefixes   = []string{"kube-", "gke-", "gmp-", "amazon-", "eks-"}
)

func systemNamespace(ns string) bool {
	if slices.Contains(systemNamespaces, ns) {
		return true
	}
	for _, p := range systemPrefixes {
		if strings.HasPrefix(ns, p) {
			return true
		}
	}
	return false
}

// KubeCollector checks one registered cluster: privileged and host-level
// pods, containers that may run as root or have no limits, namespaces without
// a NetworkPolicy, cluster-admin bindings and Services exposed on nodes or
// load balancers. It only lists; it never changes the cluster.
type KubeCollector struct {
	env   envs.EnvConfig
	build func() (kubernetes.Interface, error)

	mu   sync.Mutex
	kube kubernetes.Interface // built on first use; a failed build is retried
}

// NewKubeCollector reads env's cluster with the kubeconfig rules of the
// environment registry.
func NewKubeCollector(env envs.EnvConfig) *KubeCollector {
	return newKubeCollector(env, func() (kubernetes.Interface, error) { return envs.NewKubeClient(env, KubeRequestTimeout) })
}

func newKubeCollector(env envs.EnvConfig, build func() (kubernetes.Interface, error)) *KubeCollector {
	return &KubeCollector{env: env, build: build}
}

func (c *KubeCollector) source() Source {
	return Source{Name: "kubernetes", Provider: apiv1.SecurityProvider(c.env.Provider), Environment: c.env.Name}
}

// Sources is the cluster's one source.
func (c *KubeCollector) Sources() []Source { return []Source{c.source()} }

func (c *KubeCollector) client() (kubernetes.Interface, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.kube == nil {
		k, err := c.build()
		if err != nil {
			return nil, err
		}
		c.kube = k
	}
	return c.kube, nil
}

// Collect reports not_configured when no client can be built from the
// kubeconfig, unavailable when the API server does not answer, and error
// (with the findings of the checks that worked) when some lists fail.
func (c *KubeCollector) Collect(ctx context.Context) []Report {
	rep := Report{Source: c.source()}
	kube, err := c.client()
	if err != nil {
		rep.Status, rep.Message = apiv1.SecuritySourceStatusNotConfigured, err.Error()
		return []Report{rep}
	}
	v, err := kube.Discovery().ServerVersionWithContext(ctx)
	if err != nil {
		rep.Status, rep.Message = apiv1.SecuritySourceStatusUnavailable, envs.DescribeError("server version", err)
		return []Report{rep}
	}

	k := kubeChecks{findings: map[string]apiv1.SecurityFinding{}}
	var errs []string
	pods, err := listPods(ctx, kube)
	if err != nil {
		errs = append(errs, envs.DescribeError("list pods", err))
	} else {
		k.pods(pods)
		if pols, err := kube.NetworkingV1().NetworkPolicies("").List(ctx, metav1.ListOptions{}); err != nil {
			errs = append(errs, envs.DescribeError("list network policies", err))
		} else {
			k.networkPolicies(pods, pols.Items)
		}
	}
	if crbs, err := kube.RbacV1().ClusterRoleBindings().List(ctx, metav1.ListOptions{}); err != nil {
		errs = append(errs, envs.DescribeError("list cluster role bindings", err))
	} else {
		k.clusterAdmins(crbs.Items)
	}
	if svcs, err := kube.CoreV1().Services("").List(ctx, metav1.ListOptions{}); err != nil {
		errs = append(errs, envs.DescribeError("list services", err))
	} else {
		k.services(svcs.Items)
	}

	for _, f := range k.findings {
		rep.Findings = append(rep.Findings, f)
	}
	rep.Status = apiv1.SecuritySourceStatusOk
	rep.Message = fmt.Sprintf("Kubernetes %s, %s checked outside system namespaces", v.GitVersion, many(k.checked, "running pod", "running pods"))
	if len(errs) > 0 {
		rep.Status, rep.Message = apiv1.SecuritySourceStatusError, strings.Join(errs, "; ")
	}
	return []Report{rep}
}

// listPods lists every pod, a page at a time.
func listPods(ctx context.Context, kube kubernetes.Interface) ([]corev1.Pod, error) {
	var out []corev1.Pod
	opts := metav1.ListOptions{Limit: 500}
	for {
		list, err := kube.CoreV1().Pods("").List(ctx, opts)
		if err != nil {
			return nil, err
		}
		out = append(out, list.Items...)
		if list.Continue == "" {
			return out, nil
		}
		opts.Continue = list.Continue
	}
}

// kubeChecks collects findings keyed by check and resource, so the replicas
// of one workload give one finding.
type kubeChecks struct {
	findings map[string]apiv1.SecurityFinding
	checked  int
}

func (k *kubeChecks) add(f apiv1.SecurityFinding) {
	key := f.Check + "\x00" + f.Resource
	if _, ok := k.findings[key]; !ok {
		k.findings[key] = f
	}
}

// active reports pods that run or will run; finished Job pods are history.
func active(p *corev1.Pod) bool {
	return p.Status.Phase != corev1.PodSucceeded && p.Status.Phase != corev1.PodFailed
}

// workload names the controller a pod belongs to, e.g. "Deployment/web",
// "StatefulSet/db" or "Cluster/pg" (CloudNativePG); a bare pod is "Pod/name".
func workload(p *corev1.Pod) string {
	ref := metav1.GetControllerOf(p)
	if ref == nil {
		return "Pod/" + p.Name
	}
	kind, name := ref.Kind, ref.Name
	if h := p.Labels["pod-template-hash"]; kind == "ReplicaSet" && h != "" && strings.HasSuffix(name, "-"+h) {
		kind, name = "Deployment", strings.TrimSuffix(name, "-"+h)
	}
	return kind + "/" + name
}

func (k *kubeChecks) pods(pods []corev1.Pod) {
	for i := range pods {
		p := &pods[i]
		if systemNamespace(p.Namespace) || !active(p) {
			continue
		}
		k.checked++
		w := p.Namespace + "/" + workload(p)
		spec := &p.Spec
		if spec.HostNetwork {
			k.add(apiv1.SecurityFinding{Check: "k8s.host-network", Category: apiv1.SecurityCategoryNetwork, Severity: apiv1.SecuritySeverityMedium,
				Title: "Pod uses the host network", Resource: w,
				Detail:         fmt.Sprintf("%s (pod %s) shares the node's network namespace and can bind any node port.", w, p.Name),
				Recommendation: "Use the pod network and a Service unless the pod must bind node ports."})
		}
		if spec.HostPID {
			k.add(apiv1.SecurityFinding{Check: "k8s.host-pid", Category: apiv1.SecurityCategoryWorkload, Severity: apiv1.SecuritySeverityHigh,
				Title: "Pod shares the host PID namespace", Resource: w,
				Detail:         fmt.Sprintf("%s (pod %s) can see and signal every process on the node.", w, p.Name),
				Recommendation: "Remove hostPID."})
		}
		for _, v := range spec.Volumes {
			if v.HostPath == nil {
				continue
			}
			k.add(apiv1.SecurityFinding{Check: "k8s.host-path", Category: apiv1.SecurityCategoryWorkload, Severity: apiv1.SecuritySeverityMedium,
				Title: "Pod mounts a host path", Resource: w + "/" + v.Name,
				Detail:         fmt.Sprintf("%s mounts the node path %s as volume %q.", w, v.HostPath.Path, v.Name),
				Recommendation: "Use a PersistentVolume, a ConfigMap or an emptyDir instead of a node path."})
		}
		podNonRoot := spec.SecurityContext != nil && isTrue(spec.SecurityContext.RunAsNonRoot)
		for _, ctr := range slices.Concat(spec.InitContainers, spec.Containers) {
			r := w + "/" + ctr.Name
			sc := ctr.SecurityContext
			if sc != nil && isTrue(sc.Privileged) {
				k.add(apiv1.SecurityFinding{Check: "k8s.privileged", Category: apiv1.SecurityCategoryWorkload, Severity: apiv1.SecuritySeverityHigh,
					Title: "Privileged container", Resource: r,
					Detail:         fmt.Sprintf("Container %q of %s runs privileged: it has every capability and the node's devices.", ctr.Name, w),
					Recommendation: "Drop privileged and add only the capabilities the container needs."})
			}
			if !podNonRoot && (sc == nil || !isTrue(sc.RunAsNonRoot)) {
				k.add(apiv1.SecurityFinding{Check: "k8s.run-as-non-root", Category: apiv1.SecurityCategoryWorkload, Severity: apiv1.SecuritySeverityLow,
					Title: "Container does not set runAsNonRoot", Resource: r,
					Detail:         fmt.Sprintf("Container %q of %s may run as root: neither the pod nor the container sets runAsNonRoot.", ctr.Name, w),
					Recommendation: "Set securityContext.runAsNonRoot: true and a non-zero runAsUser."})
			}
		}
		for _, ctr := range spec.Containers { // init containers end before the app starts
			if len(ctr.Resources.Limits) == 0 {
				k.add(apiv1.SecurityFinding{Check: "k8s.no-limits", Category: apiv1.SecurityCategoryWorkload, Severity: apiv1.SecuritySeverityLow,
					Title: "Container has no resource limits", Resource: w + "/" + ctr.Name,
					Detail:         fmt.Sprintf("Container %q of %s sets no cpu or memory limit, so it can starve the node.", ctr.Name, w),
					Recommendation: "Set resources.limits for memory (and cpu where it matters)."})
			}
		}
	}
}

// networkPolicies reports namespaces that run pods but have no NetworkPolicy:
// every pod there accepts traffic from anywhere in the cluster.
func (k *kubeChecks) networkPolicies(pods []corev1.Pod, pols []networkingv1.NetworkPolicy) {
	covered := map[string]bool{}
	for _, p := range pols {
		covered[p.Namespace] = true
	}
	running := map[string]int{}
	for i := range pods {
		if p := &pods[i]; !systemNamespace(p.Namespace) && active(p) {
			running[p.Namespace]++
		}
	}
	for ns, n := range running {
		if covered[ns] {
			continue
		}
		k.add(apiv1.SecurityFinding{Check: "k8s.no-network-policy", Category: apiv1.SecurityCategoryNetwork, Severity: apiv1.SecuritySeverityMedium,
			Title: "Namespace has no NetworkPolicy", Resource: ns,
			Detail:         fmt.Sprintf("Namespace %s runs %d pods and has no NetworkPolicy, so any pod in the cluster can reach them.", ns, n),
			Recommendation: "Add a default-deny NetworkPolicy and allow only the traffic the namespace needs."})
	}
}

// anyone are the groups and users that mean every caller.
var anyone = []string{"system:authenticated", "system:unauthenticated", "system:anonymous"}

// systemSubject reports subjects Kubernetes or the provider manages:
// system: users and groups (except the ones meaning anyone), EKS's eks:
// identities and service accounts of system namespaces.
func systemSubject(s rbacv1.Subject) bool {
	if s.Kind == rbacv1.ServiceAccountKind {
		return systemNamespace(s.Namespace)
	}
	if slices.Contains(anyone, s.Name) {
		return false
	}
	return strings.HasPrefix(s.Name, "system:") || strings.HasPrefix(s.Name, "eks:")
}

func (k *kubeChecks) clusterAdmins(crbs []rbacv1.ClusterRoleBinding) {
	for _, b := range crbs {
		if b.RoleRef.Kind != "ClusterRole" || b.RoleRef.Name != "cluster-admin" {
			continue
		}
		for _, s := range b.Subjects {
			if systemSubject(s) {
				continue
			}
			who := s.Kind + " " + s.Name
			if s.Namespace != "" {
				who = s.Kind + " " + s.Namespace + "/" + s.Name
			}
			sev := apiv1.SecuritySeverityHigh
			if slices.Contains(anyone, s.Name) {
				sev = apiv1.SecuritySeverityCritical
			}
			k.add(apiv1.SecurityFinding{Check: "k8s.cluster-admin", Category: apiv1.SecurityCategoryIdentity, Severity: sev,
				Title: "cluster-admin bound to " + who, Resource: "ClusterRoleBinding/" + b.Name + "/" + who,
				Detail:         fmt.Sprintf("ClusterRoleBinding %s gives %s full control of the cluster.", b.Name, who),
				Recommendation: "Bind a narrower ClusterRole, or a Role in the namespaces it needs."})
		}
	}
}

func (k *kubeChecks) services(svcs []corev1.Service) {
	for _, s := range svcs {
		if systemNamespace(s.Namespace) {
			continue
		}
		r := s.Namespace + "/Service/" + s.Name
		switch s.Spec.Type {
		case corev1.ServiceTypeLoadBalancer:
			detail := fmt.Sprintf("Service %s/%s has a cloud load balancer", s.Namespace, s.Name)
			if len(s.Spec.LoadBalancerSourceRanges) > 0 {
				detail += " restricted to " + strings.Join(s.Spec.LoadBalancerSourceRanges, ", ")
			}
			k.add(apiv1.SecurityFinding{Check: "k8s.load-balancer", Category: apiv1.SecurityCategoryNetwork, Severity: apiv1.SecuritySeverityMedium,
				Title: "Service exposed through a load balancer", Resource: r, Detail: detail + ports(s.Spec.Ports) + ".",
				Recommendation: "Route users through the gateway, or restrict loadBalancerSourceRanges."})
		case corev1.ServiceTypeNodePort:
			k.add(apiv1.SecurityFinding{Check: "k8s.node-port", Category: apiv1.SecurityCategoryNetwork, Severity: apiv1.SecuritySeverityLow,
				Title: "Service exposed on node ports", Resource: r,
				Detail:         fmt.Sprintf("Service %s/%s opens a port on every node%s.", s.Namespace, s.Name, ports(s.Spec.Ports)),
				Recommendation: "Use a ClusterIP Service behind the gateway or an Ingress."})
		}
	}
}

func ports(ps []corev1.ServicePort) string {
	var out []string
	for _, p := range ps {
		if p.NodePort != 0 {
			out = append(out, fmt.Sprintf("%d->%d/%s", p.NodePort, p.Port, p.Protocol))
		} else {
			out = append(out, fmt.Sprintf("%d/%s", p.Port, p.Protocol))
		}
	}
	if len(out) == 0 {
		return ""
	}
	return " (" + strings.Join(out, ", ") + ")"
}

func isTrue(b *bool) bool { return b != nil && *b }
