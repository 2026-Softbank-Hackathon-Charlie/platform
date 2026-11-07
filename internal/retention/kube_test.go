package retention

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/fake"
	ktesting "k8s.io/client-go/testing"
)

type dbFake struct {
	recovery bool
	sessions int
	fail     bool
}

func (d *dbFake) InRecovery(context.Context) (bool, error) {
	if d.fail {
		return false, errors.New("unavailable")
	}
	return d.recovery, nil
}
func (d *dbFake) ClientSessions(context.Context) (int, error) { return d.sessions, nil }
func (d *dbFake) Close()                                      {}

type trafficFake struct{ state apiv1.TrafficState }

func (t *trafficFake) State(context.Context, time.Duration) (apiv1.TrafficState, error) {
	return t.state, nil
}

func obj(kind, name, uid string, fields map[string]any) *unstructured.Unstructured {
	version := "v1"
	if kind == "Cluster" || kind == "Backup" {
		version = "postgresql.cnpg.io/v1"
	}
	o := &unstructured.Unstructured{Object: map[string]any{"apiVersion": version, "kind": kind, "metadata": map[string]any{"name": name, "namespace": "app", "uid": uid, "resourceVersion": "7"}}}
	for k, v := range fields {
		o.Object[k] = v
	}
	return o
}
func cluster(name, uid, primaryName string) *unstructured.Unstructured {
	return obj("Cluster", name, uid, map[string]any{"spec": map[string]any{"instances": int64(1), "replica": map[string]any{"self": name, "primary": primaryName}}, "status": map[string]any{"readyInstances": int64(1), "phase": "Cluster in healthy state"}})
}

type world struct {
	k                  *Kube
	p                  *plan.Plan
	src, dst           *fake.FakeDynamicClient
	traffic            *trafficFake
	sourceDB, targetDB *dbFake
	now, ended         time.Time
	job                Job
}

func newWorld(t *testing.T) *world {
	t.Helper()
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	ended := now.Add(-2 * time.Hour)
	source := cluster("old", "source-uid", "new")
	source.SetAnnotations(map[string]string{"hopcharlie.io/retention-allowed": "true"})
	target := cluster("new", "target-uid", "new")
	pod := obj("Pod", "old-1", "pod-uid", nil)
	pod.SetLabels(map[string]string{"cnpg.io/cluster": "old"})
	pod.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "postgresql.cnpg.io/v1", Kind: "Cluster", Name: "old", UID: types.UID("source-uid")}})
	pvc := obj("PersistentVolumeClaim", "old-1", "pvc-uid", nil)
	backup := obj("Backup", "backup", "backup-uid", map[string]any{"spec": map[string]any{"cluster": map[string]any{"name": "new"}}, "status": map[string]any{"phase": "completed", "stoppedAt": now.Add(-time.Hour).Format(time.RFC3339)}})
	proof := obj("ConfigMap", "hopcharlie-retention-recovery", "proof-uid", map[string]any{"data": map[string]any{"runId": "run-1", "targetUID": "target-uid", "backupName": "backup", "backupUID": "backup-uid", "restoreRunId": "restore-1", "verifiedAt": now.Add(-30 * time.Minute).Format(time.RFC3339), "result": "passed"}})
	lists := map[schema.GroupVersionResource]string{pods: "PodList", clusters: "ClusterList", backups: "BackupList", configmaps: "ConfigMapList"}
	src := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), lists, source, pod, pvc)
	dst := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), lists, target, backup, proof)
	traffic := &trafficFake{state: apiv1.TrafficState{WriteBackend: "new", ReadWeights: map[string]int{"old": 0, "new": 100}, UpdatedAt: now, Stats: map[string]apiv1.BackendStats{"new": {Requests: 30, ErrorRate: 0, P95Millis: 50}}}}
	sdb, tdb := &dbFake{recovery: true}, &dbFake{}
	k := &Kube{Clients: map[string]dynamic.Interface{"a": src, "b": dst}, Now: func() time.Time { return now }, Traffic: func(string) Traffic { return traffic }, Connect: func(_ context.Context, dsn string) (Database, error) {
		if dsn == "source" {
			return sdb, nil
		}
		return tdb, nil
	}}
	p := &plan.Plan{Source: plan.Endpoint{Env: "a", Namespace: "app", Cluster: "old", DSN: "source"}, Target: plan.Endpoint{Env: "b", Namespace: "app", Cluster: "new", DSN: "target"}}
	p.Gateway.Mode = "http"
	p.Gateway.SourceBackend = "old"
	p.Gateway.TargetBackend = "new"
	j, err := k.Capture(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	j.RunID = "run-1"
	j.Status = "scheduled"
	return &world{k: k, p: p, src: src, dst: dst, traffic: traffic, sourceDB: sdb, targetDB: tdb, now: now, ended: ended, job: j}
}
func edit(t *testing.T, c dynamic.Interface, g schema.GroupVersionResource, name string, change func(*unstructured.Unstructured)) {
	t.Helper()
	r := c.Resource(g).Namespace("app")
	o, err := r.Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	change(o)
	if _, err = r.Update(context.Background(), o, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
}
func TestChecksFailClosed(t *testing.T) {
	cases := map[string]func(*testing.T, *world){
		"valid": func(*testing.T, *world) {},
		"source replaced": func(t *testing.T, w *world) {
			edit(t, w.src, clusters, "old", func(o *unstructured.Unstructured) { o.SetUID("replacement") })
		},
		"source primary": func(t *testing.T, w *world) {
			edit(t, w.src, clusters, "old", func(o *unstructured.Unstructured) {
				_ = unstructured.SetNestedField(o.Object, "old", "spec", "replica", "primary")
			})
		},
		"target replaced": func(t *testing.T, w *world) {
			edit(t, w.dst, clusters, "new", func(o *unstructured.Unstructured) { o.SetUID("replacement") })
		},
		"target unhealthy": func(t *testing.T, w *world) {
			edit(t, w.dst, clusters, "new", func(o *unstructured.Unstructured) {
				_ = unstructured.SetNestedField(o.Object, int64(0), "status", "readyInstances")
			})
		},
		"held": func(t *testing.T, w *world) {
			edit(t, w.src, clusters, "old", func(o *unstructured.Unstructured) {
				o.SetAnnotations(map[string]string{"hopcharlie.io/retention-allowed": "true", "hopcharlie.io/retention-hold": "incident"})
			})
		},
		"extended": func(t *testing.T, w *world) {
			edit(t, w.src, clusters, "old", func(o *unstructured.Unstructured) {
				o.SetAnnotations(map[string]string{"hopcharlie.io/retention-allowed": "true", "hopcharlie.io/retain-until": w.now.Add(time.Hour).Format(time.RFC3339)})
			})
		},
		"traffic source": func(_ *testing.T, w *world) { w.traffic.state.ReadWeights["old"] = 1 },
		"gateway stale":  func(_ *testing.T, w *world) { w.traffic.state.UpdatedAt = w.now.Add(-time.Hour) },
		"inflight":       func(_ *testing.T, w *world) { w.traffic.state.InFlight.Writes = 1 },
		"SLO unmeasured": func(_ *testing.T, w *world) { w.traffic.state.Stats = nil },
		"SLO failed": func(_ *testing.T, w *world) {
			w.traffic.state.Stats["new"] = apiv1.BackendStats{Requests: 30, ErrorRate: 0.5}
		},
		"db sessions":         func(_ *testing.T, w *world) { w.sourceDB.sessions = 1 },
		"sql source writable": func(_ *testing.T, w *world) { w.sourceDB.recovery = false },
		"sql unavailable":     func(_ *testing.T, w *world) { w.sourceDB.fail = true },
		"backup missing": func(_ *testing.T, w *world) {
			_ = w.dst.Resource(backups).Namespace("app").Delete(context.Background(), "backup", metav1.DeleteOptions{})
		},
		"restore absent": func(_ *testing.T, w *world) {
			_ = w.dst.Resource(configmaps).Namespace("app").Delete(context.Background(), "hopcharlie-retention-recovery", metav1.DeleteOptions{})
		},
		"restore wrong run": func(t *testing.T, w *world) {
			edit(t, w.dst, configmaps, "hopcharlie-retention-recovery", func(o *unstructured.Unstructured) {
				_ = unstructured.SetNestedField(o.Object, "other", "data", "runId")
			})
		},
		"backup predates migration": func(t *testing.T, w *world) {
			edit(t, w.dst, backups, "backup", func(o *unstructured.Unstructured) {
				_ = unstructured.SetNestedField(o.Object, w.ended.Add(-time.Minute).Format(time.RFC3339), "status", "stoppedAt")
			})
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			change(t, w)
			err := w.k.Check(context.Background(), w.p, w.job, w.ended)
			if (err == nil) != (name == "valid") {
				t.Fatalf("check=%v", err)
			}
			for _, a := range w.src.Actions() {
				if a.GetVerb() == "delete" {
					t.Fatal("check deleted a resource")
				}
			}
		})
	}
}
func TestRemovalPreservesPVCAndBindsUIDAndVersion(t *testing.T) {
	w := newWorld(t)
	w.job.Status = "deleting"
	seen := false
	w.src.PrependReactor("delete", "clusters", func(action ktesting.Action) (bool, runtime.Object, error) {
		o := action.(ktesting.DeleteAction).GetDeleteOptions()
		if o.Preconditions == nil || string(*o.Preconditions.UID) != "source-uid" || *o.Preconditions.ResourceVersion != "7" || o.PropagationPolicy == nil || *o.PropagationPolicy != metav1.DeletePropagationOrphan {
			t.Fatalf("unsafe delete: %+v", o)
		}
		seen = true
		return false, nil, nil
	})
	for i := 0; i < 3; i++ {
		done, err := w.k.Remove(context.Background(), w.p, w.job)
		if err != nil {
			t.Fatal(err)
		}
		if done != (i == 2) {
			t.Fatalf("attempt %d done=%v", i, done)
		}
	}
	if !seen {
		t.Fatal("no conditional cluster deletion")
	}
	if _, err := w.src.Resource(schema.GroupVersionResource{Version: "v1", Resource: "persistentvolumeclaims"}).Namespace("app").Get(context.Background(), "old-1", metav1.GetOptions{}); err != nil {
		t.Fatal("PVC was removed")
	}
}
func TestPromotionOrReplacementBeforeDeleteIsNotDeleted(t *testing.T) {
	for _, replace := range []bool{false, true} {
		t.Run(fmtBool(replace), func(t *testing.T) {
			w := newWorld(t)
			w.job.Status = "deleting"
			edit(t, w.src, clusters, "old", func(o *unstructured.Unstructured) {
				if replace {
					o.SetUID("new-uid")
				} else {
					_ = unstructured.SetNestedField(o.Object, "old", "spec", "replica", "primary")
				}
			})
			if _, err := w.k.Remove(context.Background(), w.p, w.job); err == nil {
				t.Fatal("unsafe removal accepted")
			}
			for _, a := range w.src.Actions() {
				if a.GetVerb() == "delete" {
					t.Fatal("unsafe delete issued")
				}
			}
		})
	}
}
func fmtBool(b bool) string {
	if b {
		return "replacement"
	}
	return "promotion"
}
