package retention

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/kube"
	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/pg"
	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/envs"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
)

var clusters = schema.GroupVersionResource{Group: "postgresql.cnpg.io", Version: "v1", Resource: "clusters"}
var backups = schema.GroupVersionResource{Group: "postgresql.cnpg.io", Version: "v1", Resource: "backups"}
var pods = schema.GroupVersionResource{Version: "v1", Resource: "pods"}
var configmaps = schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}

type Traffic interface {
	State(context.Context, time.Duration) (apiv1.TrafficState, error)
}
type Database interface {
	InRecovery(context.Context) (bool, error)
	ClientSessions(context.Context) (int, error)
	Close()
}

// Kube only obtains clients from registered server environments, not plan paths.
// RecoveryProof is an operator-controlled ConfigMap in the target DB namespace.
type Kube struct {
	Clients map[string]dynamic.Interface
	Traffic func(string) Traffic
	Connect func(context.Context, string) (Database, error)
	Now     func() time.Time
}

func NewKube(environments []envs.EnvConfig, traffic func(string) Traffic) (*Kube, error) {
	k := &Kube{Clients: map[string]dynamic.Interface{}, Traffic: traffic, Now: time.Now}
	k.Connect = func(ctx context.Context, dsn string) (Database, error) { return pg.Connect(ctx, dsn) }
	for _, e := range environments {
		client, err := kube.Dynamic(e.Kubeconfig, e.KubeContext)
		if err != nil {
			return nil, fmt.Errorf("retention environment %s: %w", e.Name, err)
		}
		k.Clients[e.Name] = client
	}
	return k, nil
}

func (k *Kube) client(e plan.Endpoint) (dynamic.Interface, error) {
	c := k.Clients[e.Env]
	if c == nil || e.Namespace == "" || e.Cluster == "" {
		return nil, errors.New("retention endpoint not registered")
	}
	return c, nil
}

func (k *Kube) Capture(ctx context.Context, p *plan.Plan) (Job, error) {
	if p.Gateway.Mode != "http" || p.Gateway.SourceBackend == p.Gateway.TargetBackend {
		return Job{}, errors.New("source retention requires distinct HTTP gateway backends")
	}
	src, err := k.client(p.Source)
	if err != nil {
		return Job{}, err
	}
	dst, err := k.client(p.Target)
	if err != nil {
		return Job{}, err
	}
	s, err := src.Resource(clusters).Namespace(p.Source.Namespace).Get(ctx, p.Source.Cluster, metav1.GetOptions{})
	if err != nil {
		return Job{}, err
	}
	if s.GetUID() == "" || s.GetDeletionTimestamp() != nil {
		return Job{}, errors.New("invalid source identity")
	}
	j := Job{SourceUID: string(s.GetUID()), Pods: []Pod{}}
	list, err := src.Resource(pods).Namespace(p.Source.Namespace).List(ctx, metav1.ListOptions{LabelSelector: "cnpg.io/cluster=" + p.Source.Cluster})
	if err != nil {
		return Job{}, err
	}
	for _, pod := range list.Items {
		if owned(&pod, s.GetUID()) {
			j.Pods = append(j.Pods, Pod{Name: pod.GetName(), UID: string(pod.GetUID())})
		}
	}
	if len(j.Pods) == 0 {
		return Job{}, errors.New("no source database pods")
	}
	t, err := dst.Resource(clusters).Namespace(p.Target.Namespace).Get(ctx, p.Target.Cluster, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return Job{}, err
	}
	if err == nil {
		j.TargetUID = string(t.GetUID())
	}
	return j, nil
}

func owned(obj *unstructured.Unstructured, uid types.UID) bool {
	for _, ref := range obj.GetOwnerReferences() {
		if ref.UID == uid && ref.Kind == "Cluster" && ref.APIVersion == "postgresql.cnpg.io/v1" {
			return true
		}
	}
	return false
}

func self(obj *unstructured.Unstructured) string {
	s, _, _ := unstructured.NestedString(obj.Object, "spec", "replica", "self")
	if s == "" {
		s = obj.GetName()
	}
	return s
}
func primary(obj *unstructured.Unstructured) string {
	s, _, _ := unstructured.NestedString(obj.Object, "spec", "replica", "primary")
	return s
}

// Check reads live topology, gateway, SQL and backup/recovery evidence. Missing
// or stale evidence is a block, never inferred from a succeeded run alone.
func (k *Kube) Check(ctx context.Context, p *plan.Plan, j Job, ended time.Time) error {
	if p == nil || allowSourceRewire(p) {
		return errors.New("source retention requires database group reconciliation")
	}
	src, err := k.client(p.Source)
	if err != nil {
		return err
	}
	dst, err := k.client(p.Target)
	if err != nil {
		return err
	}
	t, err := dst.Resource(clusters).Namespace(p.Target.Namespace).Get(ctx, p.Target.Cluster, metav1.GetOptions{})
	if err != nil {
		return err
	}
	ready, _, _ := unstructured.NestedInt64(t.Object, "status", "readyInstances")
	instances, _, _ := unstructured.NestedInt64(t.Object, "spec", "instances")
	phase, _, _ := unstructured.NestedString(t.Object, "status", "phase")
	if j.TargetUID == "" || string(t.GetUID()) != j.TargetUID || t.GetDeletionTimestamp() != nil || primary(t) != self(t) || ready < 1 || ready != instances || phase != "Cluster in healthy state" {
		return errors.New("target identity or health changed")
	}
	now := k.Now()
	s, err := src.Resource(clusters).Namespace(p.Source.Namespace).Get(ctx, p.Source.Cluster, metav1.GetOptions{})
	if apierrors.IsNotFound(err) && j.Status == "deleting" {
		s = nil
	} else if err != nil {
		return err
	}
	if s != nil {
		if string(s.GetUID()) != j.SourceUID || primary(s) != self(t) || self(s) == self(t) {
			return errors.New("source identity or replica role changed")
		}
		a := s.GetAnnotations()
		if a["hopcharlie.io/retention-allowed"] != "true" {
			return errors.New("source database is not opted in for retirement")
		}
		if a["hopcharlie.io/retention-hold"] != "" {
			return errors.New("operator retention hold")
		}
		if until := a["hopcharlie.io/retain-until"]; until != "" {
			at, err := time.Parse(time.RFC3339, until)
			if err != nil || now.Before(at) {
				return errors.New("retention extended or invalid")
			}
		}
	}
	if k.Traffic == nil || k.Connect == nil {
		return errors.New("live checks unavailable")
	}
	tc := k.Traffic(p.Gateway.URL)
	if tc == nil {
		return errors.New("gateway unavailable")
	}
	traffic, err := tc.State(ctx, 30*time.Second)
	if err != nil {
		return err
	}
	if traffic.UpdatedAt.IsZero() || now.Sub(traffic.UpdatedAt) > time.Minute || traffic.UpdatedAt.After(now.Add(5*time.Second)) || traffic.WritesHeld || traffic.WriteBackend != p.Gateway.TargetBackend || traffic.ReadWeights[p.Gateway.TargetBackend] != 100 {
		return errors.New("gateway is not fully on target")
	}
	for name, weight := range traffic.ReadWeights {
		if name != p.Gateway.TargetBackend && weight != 0 {
			return errors.New("other backend still receives reads")
		}
	}
	// Re-measure using the platform's default gate; an old success is not enough.
	stats, measured := traffic.Stats[p.Gateway.TargetBackend]
	if !measured || stats.Requests < 20 || stats.ErrorRate > 0.01 || stats.P95Millis > 800 || stats.ErrorRate < 0 || stats.P95Millis < 0 {
		return errors.New("target SLO is unmeasured or outside the default gate")
	}
	if traffic.InFlight.Reads != 0 || traffic.InFlight.Writes != 0 {
		return errors.New("requests still in flight")
	}
	if s != nil {
		db, err := k.Connect(ctx, p.Source.DSN)
		if err != nil {
			return errors.New("source SQL check unavailable")
		}
		defer db.Close()
		recovery, err := db.InRecovery(ctx)
		if err != nil || !recovery {
			return errors.New("source is not a standby")
		}
		sessions, err := db.ClientSessions(ctx)
		if err != nil || sessions != 0 {
			return errors.New("source still has client sessions")
		}
	}
	db, err := k.Connect(ctx, p.Target.DSN)
	if err != nil {
		return errors.New("target SQL check unavailable")
	}
	defer db.Close()
	recovery, err := db.InRecovery(ctx)
	if err != nil || recovery {
		return errors.New("target is not writable primary")
	}
	return k.recoveryProof(ctx, dst, p, j, ended, now)
}

// The proof is an explicit trusted operator/restore-verifier record, not a
// claim that a configured backup or this worker has performed a restore.
func (k *Kube) recoveryProof(ctx context.Context, dst dynamic.Interface, p *plan.Plan, j Job, ended, now time.Time) error {
	proof, err := dst.Resource(configmaps).Namespace(p.Target.Namespace).Get(ctx, "hopcharlie-retention-recovery", metav1.GetOptions{})
	if err != nil {
		return errors.New("restore proof unavailable")
	}
	d, _, err := unstructured.NestedStringMap(proof.Object, "data")
	if err != nil {
		return errors.New("invalid restore proof")
	}
	at, err := time.Parse(time.RFC3339, d["verifiedAt"])
	if err != nil || at.Before(ended) || at.After(now) || now.Sub(at) > 24*time.Hour || d["runId"] != j.RunID || d["targetUID"] != j.TargetUID || d["result"] != "passed" || d["restoreRunId"] == "" {
		return errors.New("restore proof stale or out of scope")
	}
	b, err := dst.Resource(backups).Namespace(p.Target.Namespace).Get(ctx, d["backupName"], metav1.GetOptions{})
	if err != nil {
		return errors.New("backup unavailable")
	}
	cluster, _, _ := unstructured.NestedString(b.Object, "spec", "cluster", "name")
	phase, _, _ := unstructured.NestedString(b.Object, "status", "phase")
	stopped, _, _ := unstructured.NestedString(b.Object, "status", "stoppedAt")
	completed, err := time.Parse(time.RFC3339, stopped)
	if err != nil || completed.Before(ended) || completed.After(at) || now.Sub(completed) > 24*time.Hour || d["backupUID"] == "" || string(b.GetUID()) != d["backupUID"] || cluster != p.Target.Cluster || phase != "completed" || b.GetDeletionTimestamp() != nil {
		return errors.New("completed backup does not match restore proof")
	}
	return nil
}

func (k *Kube) Remove(ctx context.Context, p *plan.Plan, j Job) (bool, error) {
	if p == nil || allowSourceRewire(p) {
		return false, errors.New("source retention requires database group reconciliation")
	}
	if j.Status != "deleting" {
		return false, errors.New("deletion intent not persisted")
	}
	src, err := k.client(p.Source)
	if err != nil {
		return false, err
	}
	resource := src.Resource(clusters).Namespace(p.Source.Namespace)
	s, err := resource.Get(ctx, p.Source.Cluster, metav1.GetOptions{})
	if err == nil {
		if string(s.GetUID()) != j.SourceUID {
			return false, errors.New("source replaced")
		}
		// Never allow a role/hold change between Check and the DELETE read.
		dst, err := k.client(p.Target)
		if err != nil {
			return false, err
		}
		t, err := dst.Resource(clusters).Namespace(p.Target.Namespace).Get(ctx, p.Target.Cluster, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		if string(t.GetUID()) != j.TargetUID || primary(t) != self(t) || primary(s) != self(t) || self(s) == self(t) {
			return false, errors.New("topology changed before deletion")
		}
		if a := s.GetAnnotations(); a["hopcharlie.io/retention-hold"] != "" {
			return false, errors.New("retention held")
		}
		if s.GetAnnotations()["hopcharlie.io/retention-allowed"] != "true" {
			return false, errors.New("source retirement permission revoked")
		}
		if until := s.GetAnnotations()["hopcharlie.io/retain-until"]; until != "" {
			at, err := time.Parse(time.RFC3339, until)
			if err != nil || k.Now().Before(at) {
				return false, errors.New("retention extended")
			}
		}
		uid, rv := s.GetUID(), s.GetResourceVersion()
		if uid == "" || rv == "" {
			return false, errors.New("delete preconditions unavailable")
		}
		// Orphan propagation preserves PVCs, secrets, backups and services.
		// Only the captured DB pods are explicitly deleted after CR disappearance.
		policy := metav1.DeletePropagationOrphan
		if err := resource.Delete(ctx, p.Source.Cluster, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &rv}, PropagationPolicy: &policy}); err != nil {
			return false, err
		}
		return false, nil
	}
	if !apierrors.IsNotFound(err) {
		return false, err
	}
	done := true
	for _, captured := range j.Pods {
		pr := src.Resource(pods).Namespace(p.Source.Namespace)
		pod, err := pr.Get(ctx, captured.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return false, err
		}
		if string(pod.GetUID()) != captured.UID {
			return false, errors.New("database pod replaced; operator reconciliation required")
		}
		for _, owner := range pod.GetOwnerReferences() {
			if owner.Kind == "Cluster" && string(owner.UID) != j.SourceUID {
				return false, errors.New("database pod adopted by another cluster")
			}
		}
		uid, rv := pod.GetUID(), pod.GetResourceVersion()
		if uid == "" || rv == "" {
			return false, errors.New("pod preconditions unavailable")
		}
		if err := pr.Delete(ctx, captured.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &rv}}); err != nil && !apierrors.IsNotFound(err) {
			return false, err
		}
		done = false
	}
	remaining, err := src.Resource(pods).Namespace(p.Source.Namespace).List(ctx, metav1.ListOptions{LabelSelector: "cnpg.io/cluster=" + p.Source.Cluster})
	if err != nil {
		return false, err
	}
	if len(remaining.Items) > 0 {
		return false, errors.New("source database pods remain; operator reconciliation required")
	}
	return done, nil
}
