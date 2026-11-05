package envs

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/cnpg"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/version"
	fakediscovery "k8s.io/client-go/discovery/fake"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

func node(name, arch string, ready corev1.ConditionStatus) *corev1.Node {
	n := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status:     corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{Architecture: arch}},
	}
	if ready != "" {
		n.Status.Conditions = []corev1.NodeCondition{
			{Type: corev1.NodeMemoryPressure, Status: corev1.ConditionFalse},
			{Type: corev1.NodeReady, Status: ready},
		}
	}
	return n
}

func cnpgCluster(ns, name string, spec, status map[string]any) *unstructured.Unstructured {
	obj := map[string]any{
		"apiVersion": "postgresql.cnpg.io/v1",
		"kind":       "Cluster",
		"metadata":   map[string]any{"namespace": ns, "name": name},
		"spec":       spec,
	}
	if status != nil {
		obj["status"] = status
	}
	return &unstructured.Unstructured{Object: obj}
}

func newKube(gitVersion string, nodes ...runtime.Object) *fake.Clientset {
	cs := fake.NewClientset(nodes...)
	cs.Discovery().(*fakediscovery.FakeDiscovery).FakedServerVersion = &version.Info{GitVersion: gitVersion}
	return cs
}

func newDynamic(objs ...runtime.Object) *dynamicfake.FakeDynamicClient {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{cnpg.ClusterGVR: "ClusterList", objectStoreGVR: "ObjectStoreList"}, objs...)
}

func objectStore(ns, name string, windows map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "barmancloud.cnpg.io/v1",
		"kind":       "ObjectStore",
		"metadata":   map[string]any{"namespace": ns, "name": name},
		"spec":       map[string]any{},
		"status":     map[string]any{"serverRecoveryWindow": windows},
	}}
}

func barman(params map[string]any, extra map[string]any) []any {
	pl := map[string]any{"name": "barman-cloud.cloudnative-pg.io", "isWALArchiver": true, "parameters": params}
	for k, v := range extra {
		pl[k] = v
	}
	return []any{pl}
}

func archiving(status string) map[string]any {
	return map[string]any{"conditions": []any{
		map[string]any{"type": "Ready", "status": "True"},
		map[string]any{"type": "ContinuousArchiving", "status": status},
	}}
}

func TestProbeReadsNodesAndDatabases(t *testing.T) {
	kube := newKube("v1.31.4-gke.1086000",
		node("pool-a-1", "amd64", corev1.ConditionTrue),
		node("pool-a-2", "amd64", corev1.ConditionTrue),
		node("pool-arm-1", "arm64", corev1.ConditionFalse),
		node("pool-arm-2", "arm64", ""), // no Ready condition yet
		node("pool-a-3", "amd64", corev1.ConditionUnknown),
	)
	dyn := newDynamic(
		// Primary of the distributed topology.
		cnpgCluster("shop", "shop-db-gke", map[string]any{
			"instances": int64(3),
			"replica":   map[string]any{"self": "shop-db-gke", "primary": "shop-db-gke", "source": "shop-db-eks"},
		}, map[string]any{"phase": cnpg.HealthyPhase, "readyInstances": int64(3), "currentPrimary": "shop-db-gke-1"}),
		// Topology without self: self defaults to the resource name, so this is the primary.
		cnpgCluster("shop", "shop-db", map[string]any{
			"instances": int64(1),
			"replica":   map[string]any{"primary": "shop-db", "source": "shop-db-eks"},
		}, map[string]any{"phase": cnpg.HealthyPhase, "readyInstances": int64(1), "currentPrimary": "shop-db-1"}),
		// Replica: the topology names another cluster as primary.
		cnpgCluster("staging", "shop-db", map[string]any{
			"instances": int64(2),
			"replica":   map[string]any{"self": "shop-db-onprem", "primary": "shop-db-gke", "source": "shop-db-gke"},
		}, map[string]any{"phase": "Setting up primary", "readyInstances": int64(1), "currentPrimary": "shop-db-1"}),
		// Legacy replica cluster.
		cnpgCluster("legacy", "pg", map[string]any{
			"instances": int64(1),
			"replica":   map[string]any{"enabled": true, "source": "pg-origin"},
		}, map[string]any{"phase": cnpg.HealthyPhase, "readyInstances": int64(1), "currentPrimary": "pg-1"}),
		// Just created: no status yet.
		cnpgCluster("apps", "new-db", map[string]any{"instances": int64(1)}, nil),
	)

	st := NewClusterProber(kube, dyn).Probe(context.Background())

	if !st.Reachable || st.Error != nil {
		t.Fatalf("reachable=%v error=%v, want reachable without error", st.Reachable, deref(st.Error))
	}
	if deref(st.KubernetesVersion) != "v1.31.4-gke.1086000" {
		t.Errorf("kubernetesVersion = %q", deref(st.KubernetesVersion))
	}
	if st.Nodes == nil || st.Nodes.Total != 5 || st.Nodes.Ready != 2 || strings.Join(st.Nodes.Architectures, ",") != "amd64,arm64" {
		t.Errorf("nodes = %+v, want total 5, ready 2, architectures amd64,arm64", st.Nodes)
	}

	type db struct {
		ns, name, phase, primary string
		instances, ready         int
		replica                  bool
	}
	want := []db{
		{"apps", "new-db", "", "", 1, 0, false},
		{"legacy", "pg", cnpg.HealthyPhase, "pg-1", 1, 1, true},
		{"shop", "shop-db", cnpg.HealthyPhase, "shop-db-1", 1, 1, false},
		{"shop", "shop-db-gke", cnpg.HealthyPhase, "shop-db-gke-1", 3, 3, false},
		{"staging", "shop-db", "Setting up primary", "shop-db-1", 2, 1, true},
	}
	if st.Databases == nil || len(*st.Databases) != len(want) {
		t.Fatalf("databases = %+v, want %d sorted by namespace/name", st.Databases, len(want))
	}
	for i, d := range *st.Databases {
		got := db{d.Namespace, d.Name, deref(d.Phase), deref(d.CurrentPrimary), d.Instances, d.ReadyInstances, d.Replica}
		if got != want[i] {
			t.Errorf("databases[%d] = %+v, want %+v", i, got, want[i])
		}
	}
	if d := (*st.Databases)[0]; d.Phase != nil || d.CurrentPrimary != nil {
		t.Errorf("cluster without status reports phase=%v currentPrimary=%v, want both omitted", d.Phase, d.CurrentPrimary)
	}
}

// TestProbeReadsBackups reads the backups of Clusters whose WAL archiver is
// the Barman Cloud plugin (ADR 0025): the archiving condition from the
// Cluster, the times from the ObjectStore under the Cluster's server name.
func TestProbeReadsBackups(t *testing.T) {
	dyn := newDynamic(
		cnpgCluster("moimroom", "moimroom-db", map[string]any{"instances": int64(1),
			"plugins": barman(map[string]any{"barmanObjectName": "moimroom-db", "serverName": "moimroom-db"}, map[string]any{"enabled": true})},
			archiving("True")),
		// No serverName: the Cluster's name. Archiving is failing. The serverName
		// of another plugin entry is not this one's.
		cnpgCluster("charlie-apps", "moimroom-db-gke", map[string]any{"instances": int64(1),
			"plugins": append([]any{map[string]any{"name": "barman-cloud.cloudnative-pg.io", "isWALArchiver": false, "parameters": map[string]any{"barmanObjectName": "x", "serverName": "other"}}},
				barman(map[string]any{"barmanObjectName": "moimroom-db-gke-backup"}, nil)...)},
			archiving("False")),
		// Just turned on: no ContinuousArchiving condition yet, so not failing.
		cnpgCluster("fresh", "db", map[string]any{"instances": int64(1),
			"plugins": barman(map[string]any{"barmanObjectName": "db"}, nil)}, nil),
		// CNPG says ContinuousArchiving True without any archiver: no backups.
		cnpgCluster("plain", "db", map[string]any{"instances": int64(1)}, archiving("True")),
		// A plugin that is turned off, or not the WAL archiver: no backups.
		cnpgCluster("off", "db", map[string]any{"instances": int64(1),
			"plugins": barman(map[string]any{"barmanObjectName": "db"}, map[string]any{"enabled": false})}, archiving("True")),
		cnpgCluster("other", "db", map[string]any{"instances": int64(1),
			"plugins": barman(map[string]any{"barmanObjectName": "db"}, map[string]any{"isWALArchiver": false})}, archiving("True")),
		objectStore("moimroom", "moimroom-db", map[string]any{
			"moimroom-db":    map[string]any{"firstRecoverabilityPoint": "2026-11-05T18:00:10Z", "lastSuccessfulBackupTime": "2026-11-06T18:00:08Z", "lastFailedBackupTime": "2026-11-05T15:00:00Z"},
			"another-server": map[string]any{"lastSuccessfulBackupTime": "2020-01-01T00:00:00Z"},
		}),
		objectStore("charlie-apps", "moimroom-db-gke-backup", map[string]any{
			"moimroom-db-gke": map[string]any{"lastSuccessfulBackupTime": "2026-11-06T18:00:30Z"},
		}),
	)
	st := NewClusterProber(newKube("v1.35.0"), dyn).Probe(context.Background())
	if st.Error != nil || st.Databases == nil {
		t.Fatalf("status %+v error %q", st, deref(st.Error))
	}
	got := map[string]string{}
	for _, d := range *st.Databases {
		raw, _ := json.Marshal(d.Backup)
		got[d.Namespace+"/"+d.Name] = string(raw)
	}
	want := map[string]string{
		"charlie-apps/moimroom-db-gke": `{"lastSuccessfulBackup":"2026-11-06T18:00:30Z","objectStore":"moimroom-db-gke-backup","walArchiving":false}`,
		"moimroom/moimroom-db":         `{"firstRecoverabilityPoint":"2026-11-05T18:00:10Z","lastFailedBackup":"2026-11-05T15:00:00Z","lastSuccessfulBackup":"2026-11-06T18:00:08Z","objectStore":"moimroom-db","walArchiving":true}`,
		"fresh/db":                     `{"objectStore":"db","walArchiving":true}`,
		"off/db":                       "null",
		"other/db":                     "null",
		"plain/db":                     "null",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s backup = %s, want %s", k, got[k], v)
		}
	}
}

// TestProbeBackupsWithoutObjectStores lists the databases without backup
// times when the ObjectStores cannot be read. Without the CRD there are no
// times; any other failure is named in error, so the times are unknown
// rather than absent.
func TestProbeBackupsWithoutObjectStores(t *testing.T) {
	for _, c := range []struct {
		name, wantErr string
		fail          error
	}{
		{"forbidden", "list Barman ObjectStores: forbidden by RBAC", apierrors.NewForbidden(objectStoreGVR.GroupResource(), "", errors.New("cannot list objectstores"))},
		{"no CRD", "", apierrors.NewNotFound(objectStoreGVR.GroupResource(), "")},
	} {
		t.Run(c.name, func(t *testing.T) {
			dyn := newDynamic(cnpgCluster("moimroom", "moimroom-db", map[string]any{"instances": int64(1),
				"plugins": barman(map[string]any{"barmanObjectName": "moimroom-db"}, nil)}, archiving("True")))
			dyn.PrependReactor("list", "objectstores", func(clienttesting.Action) (bool, runtime.Object, error) {
				return true, nil, c.fail
			})
			st := NewClusterProber(newKube("v1.35.0"), dyn).Probe(context.Background())
			if deref(st.Error) != c.wantErr || st.Databases == nil || len(*st.Databases) != 1 {
				t.Fatalf("status %+v error %q, want error %q", st, deref(st.Error), c.wantErr)
			}
			b := (*st.Databases)[0].Backup
			if b == nil || !b.WalArchiving || b.ObjectStore != "moimroom-db" || b.LastSuccessfulBackup != nil || b.FirstRecoverabilityPoint != nil {
				t.Fatalf("backup %+v", b)
			}
		})
	}
}

// TestProbeSkipsObjectStoresWithoutBackups does not read ObjectStores when no
// Cluster has backups, so a cluster without the permission reports no error.
func TestProbeSkipsObjectStoresWithoutBackups(t *testing.T) {
	dyn := newDynamic(cnpgCluster("plain", "db", map[string]any{"instances": int64(1)}, archiving("True")))
	dyn.PrependReactor("list", "objectstores", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(objectStoreGVR.GroupResource(), "", errors.New("cannot list objectstores"))
	})
	if st := NewClusterProber(newKube("v1.35.0"), dyn).Probe(context.Background()); st.Error != nil {
		t.Fatalf("error %q without any backups", deref(st.Error))
	}
}

func TestProbeWithoutCNPG(t *testing.T) {
	dyn := newDynamic()
	// What an API server without the CNPG CRD answers for an unknown resource.
	dyn.PrependReactor("list", "clusters", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewNotFound(cnpg.ClusterGVR.GroupResource(), "")
	})
	st := NewClusterProber(newKube("v1.32.1+k3s1", node("k3s-1", "arm64", corev1.ConditionTrue)), dyn).Probe(context.Background())
	if !st.Reachable || st.Error != nil {
		t.Fatalf("reachable=%v error=%v, want a missing CRD to be no error", st.Reachable, deref(st.Error))
	}
	if st.Databases == nil || len(*st.Databases) != 0 {
		t.Fatalf("databases = %v, want an empty list", st.Databases)
	}
	raw, _ := json.Marshal(st)
	if !strings.Contains(string(raw), `"databases":[]`) {
		t.Errorf("JSON %s, want databases as []", raw)
	}
}

func TestProbeEmptyCluster(t *testing.T) {
	st := NewClusterProber(newKube("v1.31.0"), newDynamic()).Probe(context.Background())
	if !st.Reachable || st.Nodes == nil || st.Nodes.Total != 0 || st.Nodes.Architectures == nil || st.Databases == nil {
		t.Fatalf("status = %+v, want reachable with zero nodes, [] architectures and [] databases", st)
	}
}

func TestProbeUnreachable(t *testing.T) {
	kube := newKube("v1.31.0", node("n1", "amd64", corev1.ConditionTrue))
	kube.PrependReactor("get", "version", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, &url.Error{Op: "Get", URL: "https://34.64.10.20/version?timeout=3s",
			Err: errors.New("dial tcp 34.64.10.20:443: connect: connection refused")}
	})
	st := NewClusterProber(kube, newDynamic()).Probe(context.Background())
	if st.Reachable || st.Nodes != nil || st.Databases != nil || st.KubernetesVersion != nil {
		t.Fatalf("status = %+v, want unreachable with nothing else", st)
	}
	if got := deref(st.Error); got != "server version: dial tcp 34.64.10.20:443: connect: connection refused" {
		t.Errorf("error = %q", got)
	}
}

func TestProbePartialFailure(t *testing.T) {
	kube := newKube("v1.30.6-eks-7f9249a", node("n1", "amd64", corev1.ConditionTrue))
	kube.PrependReactor("list", "nodes", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "nodes"}, "",
			errors.New(`User "arn:aws:iam::123456789012:user/ci" cannot list resource "nodes"`))
	})
	dyn := newDynamic(cnpgCluster("shop", "shop-db-eks", map[string]any{"instances": int64(1)}, nil))
	st := NewClusterProber(kube, dyn).Probe(context.Background())
	if !st.Reachable || st.Nodes != nil || st.Databases == nil || len(*st.Databases) != 1 {
		t.Fatalf("status = %+v, want reachable, nodes omitted, one database", st)
	}
	if got := deref(st.Error); got != "list nodes: forbidden by RBAC" {
		t.Errorf("error = %q", got)
	}
}

type timeoutError struct{}

func (timeoutError) Error() string   { return "i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

func TestDescribe(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{fmt.Errorf("get: %w", context.DeadlineExceeded), "op: timed out"},
		{&url.Error{Op: "Get", URL: "https://10.0.0.1/version", Err: timeoutError{}}, "op: timed out"},
		{context.Canceled, "op: cancelled"},
		{apierrors.NewUnauthorized("Unauthorized"), "op: unauthorized: credentials rejected or expired"},
		{&url.Error{Op: "Get", URL: "https://10.0.0.1/version", Err: errors.New("getting credentials: exec: executable gke-gcloud-auth-plugin not found\n\nIt looks like you are trying to use a client-go credential plugin that is not installed.")},
			"op: getting credentials: exec: executable gke-gcloud-auth-plugin not found"},
		{errors.New(strings.Repeat("x", 300)), "op: " + strings.Repeat("x", 160) + "..."},
	}
	for _, tc := range cases {
		if got := describe("op", tc.err); got != tc.want {
			t.Errorf("describe(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
}

// fakeAPIServer is the part of a Kubernetes API server the prober reads. It
// has no CNPG CRD, so the clusters resource answers 404. It serves TLS because
// client-go sends kubeconfig credentials only over TLS.
func fakeAPIServer(t *testing.T, token string) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		var body any
		switch r.URL.Path {
		case "/version":
			body = version.Info{GitVersion: "v1.32.1+k3s1", Platform: "linux/arm64"}
		case "/api/v1/nodes":
			body = corev1.NodeList{
				TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "NodeList"},
				Items:    []corev1.Node{*node("k3s-1", "arm64", corev1.ConditionTrue), *node("k3s-2", "amd64", corev1.ConditionFalse)},
			}
		default:
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// writeKubeconfig writes a kubeconfig with one context per token, all pointing
// at srv. The current context is deliberately one the tests do not ask for.
func writeKubeconfig(t *testing.T, srv *httptest.Server, tokens map[string]string) string {
	t.Helper()
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	var b strings.Builder
	fmt.Fprintf(&b, "apiVersion: v1\nkind: Config\nclusters:\n- name: c\n  cluster:\n    server: %s\n    certificate-authority-data: %s\nusers:\n",
		srv.URL, base64.StdEncoding.EncodeToString(ca))
	for ctx, tok := range tokens {
		fmt.Fprintf(&b, "- name: %s\n  user:\n    token: %s\n", ctx, tok)
	}
	b.WriteString("contexts:\n")
	for ctx := range tokens {
		fmt.Fprintf(&b, "- name: %s\n  context:\n    cluster: c\n    user: %s\n", ctx, ctx)
	}
	b.WriteString("- name: unrelated\n  context:\n    cluster: c\n    user: nobody\ncurrent-context: unrelated\n")
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestKubeProberAgainstAPIServer(t *testing.T) {
	srv := fakeAPIServer(t, "good-token")
	kubeconfig := writeKubeconfig(t, srv, map[string]string{"onprem-hopcharlie": "good-token", "stale": "expired-token"})
	ctx := context.Background()

	p, err := NewKubeProber(EnvConfig{Name: "onprem", KubeContext: "onprem-hopcharlie", Kubeconfig: kubeconfig}, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	st := p.Probe(ctx)
	if !st.Reachable || st.Error != nil {
		t.Fatalf("reachable=%v error=%q", st.Reachable, deref(st.Error))
	}
	if deref(st.KubernetesVersion) != "v1.32.1+k3s1" || st.Nodes.Total != 2 || st.Nodes.Ready != 1 ||
		strings.Join(st.Nodes.Architectures, ",") != "amd64,arm64" || st.Databases == nil || len(*st.Databases) != 0 {
		t.Fatalf("status = %+v nodes=%+v databases=%v", st, st.Nodes, st.Databases)
	}

	p, err = NewKubeProber(EnvConfig{Name: "onprem", KubeContext: "stale", Kubeconfig: kubeconfig}, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	st = p.Probe(ctx)
	if st.Reachable || deref(st.Error) != "server version: unauthorized: credentials rejected or expired" {
		t.Fatalf("reachable=%v error=%q, want unauthorized", st.Reachable, deref(st.Error))
	}
	assertNoSecrets(t, deref(st.Error), "expired-token", srv.URL, kubeconfig)
}

func TestKubeProberConfigErrors(t *testing.T) {
	kubeconfig := writeKubeconfig(t, fakeAPIServer(t, "t0ken"), map[string]string{"gke-hopcharlie": "t0ken"})
	missing := filepath.Join(t.TempDir(), "absent", "kubeconfig")
	cases := []struct {
		env  EnvConfig
		want string
	}{
		{EnvConfig{KubeContext: "eks-hopcharlie", Kubeconfig: kubeconfig}, `kube context "eks-hopcharlie" not found in kubeconfig`},
		{EnvConfig{KubeContext: "gke-hopcharlie", Kubeconfig: missing}, "kubeconfig file not found"},
	}
	for _, tc := range cases {
		_, err := NewKubeProber(tc.env, time.Second)
		if err == nil || err.Error() != tc.want {
			t.Errorf("NewKubeProber(%s) err = %v, want %q", tc.env.KubeContext, err, tc.want)
			continue
		}
		assertNoSecrets(t, err.Error(), "t0ken", kubeconfig, missing, filepath.Dir(kubeconfig))
	}
}

func assertNoSecrets(t *testing.T, msg string, secrets ...string) {
	t.Helper()
	for _, s := range secrets {
		if strings.Contains(msg, s) {
			t.Errorf("message %q leaks %q", msg, s)
		}
	}
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
