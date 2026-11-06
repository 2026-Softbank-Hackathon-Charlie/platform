package probes

import (
	"context"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/preflight/parity"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/metadata"
)

var (
	secretGVR      = schema.GroupVersionResource{Version: "v1", Resource: "secrets"}
	deploymentGVR  = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
	clusterGVR     = schema.GroupVersionResource{Group: "postgresql.cnpg.io", Version: "v1", Resource: "clusters"}
	objectStoreGVR = schema.GroupVersionResource{Group: "barmancloud.cnpg.io", Version: "v1", Resource: "objectstores"}
)

func validResourceName(name string) bool {
	return name != "" && len(validation.IsDNS1123Subdomain(name)) == 0
}

func resourceResult(capability parity.Capability, target parity.Target) parity.Result {
	return parity.Result{Capability: capability, Target: target, Status: parity.Unknown,
		Code: "resource_checks_incomplete", ObservedAt: time.Now().UTC(), Checks: []parity.Check{}}
}

// appendCheck preserves verified partial evidence, but no incomplete capability
// can pass. Provider errors, resource bodies, names and annotations are omitted.
func appendCheck(r *parity.Result, check parity.Check) {
	r.Checks = append(r.Checks, check)
	if check.Status == parity.Fail {
		r.Status, r.Code = parity.Fail, "resource_requirement_failed"
	}
}

func readCheck(id string, err error) parity.Check {
	c := parity.Check{ID: id, Status: parity.Unknown, Code: "resource_unavailable"}
	if apierrors.IsNotFound(err) {
		c.Status, c.Code = parity.Fail, "resource_missing"
	}
	return c
}

// SecretRequirement is a server-owned reference, never a secret value.
// ID is the non-sensitive label retained in the audit report.
type SecretRequirement struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type Secrets struct {
	target   parity.Target
	client   metadata.Interface
	required []SecretRequirement
}

// NewSecrets requires a metadata-only client (envs.NewReadClients removes
// client-go's full-object fallback). RBAC still requires get on secrets.
// Metadata may contain sensitive annotations; none are included in the result.
func NewSecrets(target parity.Target, client metadata.Interface, required []SecretRequirement) (*Secrets, error) {
	if !parity.ValidTarget(target) || client == nil || len(required) == 0 {
		return nil, ErrInvalidConfig
	}
	seen := map[string]bool{}
	for _, s := range required {
		if !simpleID.MatchString(s.ID) || s.ID == "secret-usability" || seen[s.ID] || !validResourceName(s.Name) {
			return nil, ErrInvalidConfig
		}
		seen[s.ID] = true
	}
	return &Secrets{target: target, client: client, required: append([]SecretRequirement(nil), required...)}, nil
}

func (p *Secrets) Check(ctx context.Context, target parity.Target) (parity.Result, error) {
	r := resourceResult(parity.Secret, target)
	if p == nil || p.client == nil {
		return r, ErrInvalidConfig
	}
	if target != p.target {
		return r, ErrWrongTarget
	}
	for _, s := range p.required {
		obj, err := p.client.Resource(secretGVR).Namespace(target.Namespace).Get(ctx, s.Name, metav1.GetOptions{})
		c := readCheck(s.ID, err)
		if err == nil && obj != nil && obj.GetName() == s.Name && obj.GetNamespace() == target.Namespace {
			c.Status, c.Code = parity.Pass, "secret_metadata_observed"
		}
		appendCheck(&r, c)
		if ctx.Err() != nil {
			break
		}
	}
	appendCheck(&r, parity.Check{ID: "secret-usability", Status: parity.Unknown, Code: "secret_usability_not_verified"})
	return r, nil
}

// AuditCollector verifies only the readiness of an explicitly selected audit
// collector Deployment. It never reads logs, policy contents or audit events.
type AuditCollector struct {
	target     parity.Target
	client     dynamic.Interface
	namespace  string
	deployment string
}

func NewAuditCollector(target parity.Target, client dynamic.Interface, namespace, deployment string) (*AuditCollector, error) {
	if !parity.ValidTarget(target) || client == nil || !simpleID.MatchString(namespace) || !validResourceName(deployment) {
		return nil, ErrInvalidConfig
	}
	return &AuditCollector{target: target, client: client, namespace: namespace, deployment: deployment}, nil
}

func (p *AuditCollector) Check(ctx context.Context, target parity.Target) (parity.Result, error) {
	r := resourceResult(parity.Audit, target)
	if p == nil || p.client == nil {
		return r, ErrInvalidConfig
	}
	if target != p.target {
		return r, ErrWrongTarget
	}
	obj, err := p.client.Resource(deploymentGVR).Namespace(p.namespace).Get(ctx, p.deployment, metav1.GetOptions{})
	c := readCheck("audit-collector", err)
	if err == nil && obj != nil && obj.GetName() == p.deployment && obj.GetNamespace() == p.namespace {
		desired, hasDesired, e1 := unstructured.NestedInt64(obj.Object, "spec", "replicas")
		observed, hasObserved, e2 := unstructured.NestedInt64(obj.Object, "status", "observedGeneration")
		ready, hasReady, e3 := unstructured.NestedInt64(obj.Object, "status", "availableReplicas")
		if !hasDesired && e1 == nil {
			desired = 1
		}
		c.Code = "collector_status_incomplete"
		switch {
		case e1 != nil || e2 != nil || e3 != nil || !hasObserved || obj.GetGeneration() <= 0 || observed < obj.GetGeneration():
		case desired == 0:
			c.Status, c.Code = parity.Fail, "collector_scaled_to_zero"
		case desired > 0 && hasReady && ready >= desired:
			c.Status, c.Code = parity.Pass, "collector_available"
		case desired > 0:
			c.Status, c.Code = parity.Fail, "collector_unavailable"
		}
	}
	appendCheck(&r, c)
	appendCheck(&r, parity.Check{ID: "audit-delivery", Status: parity.Unknown, Code: "audit_delivery_not_verified"})
	return r, nil
}

type Backup struct {
	target  parity.Target
	client  dynamic.Interface
	cluster string
	maxAge  time.Duration
}

// NewBackup reads the same Barman Cloud plugin and serverRecoveryWindow fields
// used by the environment backup view (ADR 0025). No restore is performed.
func NewBackup(target parity.Target, client dynamic.Interface, cluster string, maxAge time.Duration) (*Backup, error) {
	if !parity.ValidTarget(target) || client == nil || !validResourceName(cluster) || maxAge < time.Minute || maxAge > 30*24*time.Hour {
		return nil, ErrInvalidConfig
	}
	return &Backup{target: target, client: client, cluster: cluster, maxAge: maxAge}, nil
}

func (p *Backup) Check(ctx context.Context, target parity.Target) (parity.Result, error) {
	r := resourceResult(parity.BackupRestore, target)
	if p == nil || p.client == nil {
		return r, ErrInvalidConfig
	}
	if target != p.target {
		return r, ErrWrongTarget
	}
	obj, err := p.client.Resource(clusterGVR).Namespace(target.Namespace).Get(ctx, p.cluster, metav1.GetOptions{})
	if err != nil || obj == nil {
		appendCheck(&r, readCheck("backup-config", err))
	} else if obj.GetName() != p.cluster || obj.GetNamespace() != target.Namespace {
		appendCheck(&r, parity.Check{ID: "backup-config", Status: parity.Unknown, Code: "resource_identity_mismatch"})
	} else {
		p.checkCluster(ctx, &r, obj)
	}
	appendCheck(&r, parity.Check{ID: "restore", Status: parity.Unknown, Code: "restore_not_verified"})
	return r, nil
}

func (p *Backup) checkCluster(ctx context.Context, r *parity.Result, obj *unstructured.Unstructured) {
	plugins, _, err := unstructured.NestedSlice(obj.Object, "spec", "plugins")
	if err != nil {
		appendCheck(r, parity.Check{ID: "backup-config", Status: parity.Unknown, Code: "backup_config_invalid"})
		return
	}
	var store, server string
	for _, item := range plugins {
		pl, ok := item.(map[string]any)
		if !ok || pl["name"] != "barman-cloud.cloudnative-pg.io" || pl["isWALArchiver"] != true || pl["enabled"] == false {
			continue
		}
		store, _, _ = unstructured.NestedString(pl, "parameters", "barmanObjectName")
		server, _, _ = unstructured.NestedString(pl, "parameters", "serverName")
		break
	}
	if !validResourceName(store) {
		// Other backup providers may exist; unsupported must not mean absent.
		appendCheck(r, parity.Check{ID: "backup-config", Status: parity.Unknown, Code: "barman_archiver_not_configured"})
		return
	}
	if server == "" {
		server = obj.GetName()
	}
	appendCheck(r, parity.Check{ID: "backup-config", Status: parity.Pass, Code: "barman_archiver_configured"})
	wal := parity.Check{ID: "wal-archiving", Status: parity.Unknown, Code: "archiving_status_unknown"}
	conditions, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	for _, item := range conditions {
		c, ok := item.(map[string]any)
		if !ok || c["type"] != "ContinuousArchiving" {
			continue
		}
		if c["status"] == "True" {
			wal.Status, wal.Code = parity.Pass, "archiving_reported_healthy"
		}
		if c["status"] == "False" {
			wal.Status, wal.Code = parity.Fail, "archiving_reported_failed"
		}
		break
	}
	appendCheck(r, wal)
	storeObj, err := p.client.Resource(objectStoreGVR).Namespace(p.target.Namespace).Get(ctx, store, metav1.GetOptions{})
	backup := readCheck("recent-backup", err)
	if err == nil && storeObj != nil && storeObj.GetName() == store && storeObj.GetNamespace() == p.target.Namespace {
		value, _, _ := unstructured.NestedString(storeObj.Object, "status", "serverRecoveryWindow", server, "lastSuccessfulBackupTime")
		at, parseErr := time.Parse(time.RFC3339, value)
		backup.Code = "backup_time_unknown"
		if parseErr == nil && !at.After(r.ObservedAt) {
			backup.Status, backup.Code = parity.Pass, "recent_backup_reported"
			if r.ObservedAt.Sub(at) > p.maxAge {
				backup.Status, backup.Code = parity.Fail, "backup_stale"
			}
		}
	}
	appendCheck(r, backup)
}
