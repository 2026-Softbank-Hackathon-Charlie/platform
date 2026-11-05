package envs

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/cnpg"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

const userAgent = "hopcharlie-platform"

// NewKubeProber builds client-go clients for env's kubeconfig and context.
// Every request is bounded by timeout. It reads only the kubeconfig and never
// contacts the cluster; errors are short and name no file paths.
func NewKubeProber(env EnvConfig, timeout time.Duration) (Prober, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if env.Kubeconfig != "" {
		rules.ExplicitPath = env.Kubeconfig
	}
	raw, err := rules.Load()
	if errors.Is(err, fs.ErrNotExist) {
		return nil, errors.New("kubeconfig file not found")
	}
	if err != nil {
		return nil, errors.New("kubeconfig could not be read")
	}
	// Checked here because client-go reports a missing context only as a
	// plain error string.
	if _, ok := raw.Contexts[env.KubeContext]; !ok {
		return nil, fmt.Errorf("kube context %q not found in kubeconfig", env.KubeContext)
	}
	rc, err := clientcmd.NewNonInteractiveClientConfig(*raw, env.KubeContext, &clientcmd.ConfigOverrides{}, rules).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("kube context %q: invalid kubeconfig", env.KubeContext)
	}
	rc.UserAgent = userAgent
	rc.Timeout = timeout
	// One HTTP client so both clients share a connection pool.
	hc, err := rest.HTTPClientFor(rc)
	if err != nil {
		return nil, fmt.Errorf("kube context %q: invalid client configuration", env.KubeContext)
	}
	kube, err := kubernetes.NewForConfigAndClient(rc, hc)
	if err != nil {
		return nil, fmt.Errorf("kube context %q: invalid client configuration", env.KubeContext)
	}
	dyn, err := dynamic.NewForConfigAndClient(rc, hc)
	if err != nil {
		return nil, fmt.Errorf("kube context %q: invalid client configuration", env.KubeContext)
	}
	return NewClusterProber(kube, dyn), nil
}

// NewClusterProber returns a Prober that reads the server version and nodes
// through kube and the CNPG Clusters of all namespaces through dyn.
func NewClusterProber(kube kubernetes.Interface, dyn dynamic.Interface) Prober {
	return &clusterProber{kube: kube, dyn: dyn}
}

type clusterProber struct {
	kube kubernetes.Interface
	dyn  dynamic.Interface
}

// Probe reports reachable=false when the server version cannot be read. Once
// the server answers, a failed node or database read leaves that part out and
// is named in error.
func (p *clusterProber) Probe(ctx context.Context) apiv1.EnvironmentStatus {
	v, err := p.kube.Discovery().ServerVersionWithContext(ctx)
	if err != nil {
		return failed(describe("server version", err))
	}
	st := apiv1.EnvironmentStatus{Reachable: true}
	if v.GitVersion != "" {
		st.KubernetesVersion = &v.GitVersion
	}
	var errs []string
	if nodes, err := p.nodes(ctx); err != nil {
		errs = append(errs, describe("list nodes", err))
	} else {
		st.Nodes = nodes
	}
	if dbs, err := p.databases(ctx); err != nil {
		errs = append(errs, describe("list CNPG clusters", err))
	} else {
		st.Databases = &dbs
	}
	if len(errs) > 0 {
		msg := strings.Join(errs, "; ")
		st.Error = &msg
	}
	return st
}

func (p *clusterProber) nodes(ctx context.Context) (*apiv1.NodeSummary, error) {
	list, err := p.kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	sum := &apiv1.NodeSummary{Total: len(list.Items), Architectures: []string{}}
	for _, n := range list.Items {
		if nodeReady(&n) {
			sum.Ready++
		}
		if a := n.Status.NodeInfo.Architecture; a != "" && !slices.Contains(sum.Architectures, a) {
			sum.Architectures = append(sum.Architectures, a)
		}
	}
	slices.Sort(sum.Architectures)
	return sum, nil
}

func nodeReady(n *corev1.Node) bool {
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// barmanPlugin is the Barman Cloud plugin's name in a Cluster's
// spec.plugins, and objectStoreGVR the ObjectStores it writes to (ADR 0025).
const barmanPlugin = "barman-cloud.cloudnative-pg.io"

var objectStoreGVR = schema.GroupVersionResource{Group: "barmancloud.cnpg.io", Version: "v1", Resource: "objectstores"}

func (p *clusterProber) databases(ctx context.Context) ([]apiv1.DatabaseCluster, error) {
	list, err := p.dyn.Resource(cnpg.ClusterGVR).List(ctx, metav1.ListOptions{})
	if apierrors.IsNotFound(err) {
		// The CNPG CRD is not installed, so the cluster runs no CNPG databases.
		return []apiv1.DatabaseCluster{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := make([]apiv1.DatabaseCluster, 0, len(list.Items))
	servers := map[string]string{} // namespace/name -> the name its backups go under
	for i := range list.Items {
		d := databaseFrom(&list.Items[i])
		if d.Backup != nil {
			servers[d.Namespace+"/"+d.Name] = serverName(&list.Items[i])
		}
		out = append(out, d)
	}
	if len(servers) > 0 {
		p.backupTimes(ctx, out, servers)
	}
	slices.SortFunc(out, func(a, b apiv1.DatabaseCluster) int {
		return cmp.Or(cmp.Compare(a.Namespace, b.Namespace), cmp.Compare(a.Name, b.Name))
	})
	return out, nil
}

// databaseFrom reads a CNPG Cluster. Replica follows the engine's topology
// rule (cnpg.Status.IsPrimary: spec.replica.primary names another cluster
// than spec.replica.self) and also covers spec.replica.enabled.
func databaseFrom(obj *unstructured.Unstructured) apiv1.DatabaseCluster {
	o := obj.Object
	d := apiv1.DatabaseCluster{Namespace: obj.GetNamespace(), Name: obj.GetName()}
	instances, _, _ := unstructured.NestedInt64(o, "spec", "instances")
	ready, _, _ := unstructured.NestedInt64(o, "status", "readyInstances")
	d.Instances, d.ReadyInstances = int(instances), int(ready)
	if v, _, _ := unstructured.NestedString(o, "status", "phase"); v != "" {
		d.Phase = &v
	}
	if v, _, _ := unstructured.NestedString(o, "status", "currentPrimary"); v != "" {
		d.CurrentPrimary = &v
	}
	topo := cnpg.Status{Self: obj.GetName()}
	if v, _, _ := unstructured.NestedString(o, "spec", "replica", "self"); v != "" {
		topo.Self = v
	}
	topo.ReplicaPrimary, _, _ = unstructured.NestedString(o, "spec", "replica", "primary")
	enabled, _, _ := unstructured.NestedBool(o, "spec", "replica", "enabled")
	d.Replica = enabled || !topo.IsPrimary()
	d.Backup = backupFrom(obj)
	return d
}

// backupFrom reads the backups of a Cluster whose WAL archiver is an enabled
// Barman Cloud plugin, or nil. CNPG reports ContinuousArchiving True on a
// Cluster without any archiver too, so the condition alone says nothing.
// The times come later from the ObjectStore (backupTimes).
func backupFrom(obj *unstructured.Unstructured) *apiv1.DatabaseBackup {
	plugins, _, _ := unstructured.NestedSlice(obj.Object, "spec", "plugins")
	for _, item := range plugins {
		pl, ok := item.(map[string]any)
		if !ok || pl["name"] != barmanPlugin || pl["isWALArchiver"] != true || pl["enabled"] == false {
			continue
		}
		store, _, _ := unstructured.NestedString(pl, "parameters", "barmanObjectName")
		if store == "" {
			continue
		}
		b := &apiv1.DatabaseBackup{ObjectStore: store}
		conditions, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
		for _, c := range conditions {
			if cm, ok := c.(map[string]any); ok && cm["type"] == "ContinuousArchiving" {
				b.WalArchiving = cm["status"] == "True"
			}
		}
		return b
	}
	return nil
}

// serverName is the name a Cluster's backups go under in its ObjectStore:
// the plugin's serverName parameter, or the Cluster's name.
func serverName(obj *unstructured.Unstructured) string {
	plugins, _, _ := unstructured.NestedSlice(obj.Object, "spec", "plugins")
	for _, item := range plugins {
		if pl, ok := item.(map[string]any); ok && pl["name"] == barmanPlugin {
			if v, _, _ := unstructured.NestedString(pl, "parameters", "serverName"); v != "" {
				return v
			}
		}
	}
	return obj.GetName()
}

// backupTimes fills in the backup times of dbs from the ObjectStores'
// status.serverRecoveryWindow, under each Cluster's server name in servers
// (namespace/name). Any failure, such as no permission to list
// ObjectStores, leaves the times out: the databases are still listed.
func (p *clusterProber) backupTimes(ctx context.Context, dbs []apiv1.DatabaseCluster, servers map[string]string) {
	stores, err := p.dyn.Resource(objectStoreGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return
	}
	windows := map[string]map[string]any{} // namespace/store -> serverRecoveryWindow
	for _, s := range stores.Items {
		if w, ok, _ := unstructured.NestedMap(s.Object, "status", "serverRecoveryWindow"); ok {
			windows[s.GetNamespace()+"/"+s.GetName()] = w
		}
	}
	at := func(w map[string]any, key string) *time.Time {
		v, _ := w[key].(string)
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return nil
		}
		return &t
	}
	for i := range dbs {
		b := dbs[i].Backup
		if b == nil {
			continue
		}
		w, _ := windows[dbs[i].Namespace+"/"+b.ObjectStore][servers[dbs[i].Namespace+"/"+dbs[i].Name]].(map[string]any)
		if w == nil {
			continue
		}
		b.LastSuccessfulBackup = at(w, "lastSuccessfulBackupTime")
		b.LastFailedBackup = at(w, "lastFailedBackupTime")
		b.FirstRecoverabilityPoint = at(w, "firstRecoverabilityPoint")
	}
}

// describe turns a client error into a short message without request URLs,
// file paths or credentials.
func describe(op string, err error) string {
	var ue *url.Error
	isURL := errors.As(err, &ue)
	var msg string
	switch {
	case errors.Is(err, context.DeadlineExceeded) || isURL && ue.Timeout():
		msg = "timed out"
	case errors.Is(err, context.Canceled):
		msg = "cancelled"
	case apierrors.IsUnauthorized(err):
		msg = "unauthorized: credentials rejected or expired"
	case apierrors.IsForbidden(err):
		msg = "forbidden by RBAC"
	case isURL:
		msg = shorten(ue.Err.Error()) // drop the method and URL
	default:
		msg = shorten(err.Error())
	}
	return op + ": " + msg
}

// shorten keeps the first line (credential plugins append install hints) and
// caps its length.
func shorten(s string) string {
	s, _, _ = strings.Cut(s, "\n")
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > 160 {
		s = string(r[:160]) + "..."
	}
	return s
}
