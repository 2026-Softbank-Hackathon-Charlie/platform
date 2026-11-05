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
	if dbs, servers, err := p.databases(ctx); err != nil {
		errs = append(errs, describe("list CNPG clusters", err))
	} else {
		st.Databases = &dbs
		// The databases stand without their backup times; a failure here
		// only says why the times are missing.
		if err := p.backupTimes(ctx, dbs, servers); err != nil {
			errs = append(errs, describe("list Barman ObjectStores", err))
		}
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

// databases lists the CNPG Clusters, and for those with backups the name
// their backups go under in the ObjectStore (namespace/name -> server name).
func (p *clusterProber) databases(ctx context.Context) ([]apiv1.DatabaseCluster, map[string]string, error) {
	list, err := p.dyn.Resource(cnpg.ClusterGVR).List(ctx, metav1.ListOptions{})
	if apierrors.IsNotFound(err) {
		// The CNPG CRD is not installed, so the cluster runs no CNPG databases.
		return []apiv1.DatabaseCluster{}, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	out := make([]apiv1.DatabaseCluster, 0, len(list.Items))
	servers := map[string]string{}
	for i := range list.Items {
		d, server := databaseFrom(&list.Items[i])
		if d.Backup != nil {
			servers[d.Namespace+"/"+d.Name] = server
		}
		out = append(out, d)
	}
	slices.SortFunc(out, func(a, b apiv1.DatabaseCluster) int {
		return cmp.Or(cmp.Compare(a.Namespace, b.Namespace), cmp.Compare(a.Name, b.Name))
	})
	return out, servers, nil
}

// databaseFrom reads a CNPG Cluster. Replica follows the engine's topology
// rule (cnpg.Status.IsPrimary: spec.replica.primary names another cluster
// than spec.replica.self) and also covers spec.replica.enabled.
func databaseFrom(obj *unstructured.Unstructured) (apiv1.DatabaseCluster, string) {
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
	var server string
	d.Backup, server = backupFrom(obj)
	return d, server
}

// backupFrom reads the backups of a Cluster whose WAL archiver is an enabled
// Barman Cloud plugin, and the name they go under in its ObjectStore (the
// same entry's serverName, or the Cluster's name); nil without one. CNPG
// reports ContinuousArchiving True on a Cluster without any archiver too, so
// the condition alone says nothing; and a Cluster that has not reported the
// condition yet is not called failing. The times come from the ObjectStore
// (backupTimes).
func backupFrom(obj *unstructured.Unstructured) (*apiv1.DatabaseBackup, string) {
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
		server, _, _ := unstructured.NestedString(pl, "parameters", "serverName")
		if server == "" {
			server = obj.GetName()
		}
		b := &apiv1.DatabaseBackup{ObjectStore: store, WalArchiving: true}
		conditions, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
		for _, c := range conditions {
			if cm, ok := c.(map[string]any); ok && cm["type"] == "ContinuousArchiving" && cm["status"] == "False" {
				b.WalArchiving = false
			}
		}
		return b, server
	}
	return nil, ""
}

// backupTimes fills in the backup times of dbs from the ObjectStores'
// status.serverRecoveryWindow, under each Cluster's server name in servers
// (namespace/name). Without a Cluster with backups it reads nothing; without
// the ObjectStore CRD there are no times to read. Any other failure, such as
// no permission to list ObjectStores, is returned: the times are unknown,
// not absent.
func (p *clusterProber) backupTimes(ctx context.Context, dbs []apiv1.DatabaseCluster, servers map[string]string) error {
	if len(servers) == 0 {
		return nil
	}
	stores, err := p.dyn.Resource(objectStoreGVR).List(ctx, metav1.ListOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
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
	return nil
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
