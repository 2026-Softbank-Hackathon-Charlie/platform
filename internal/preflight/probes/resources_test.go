package probes

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/envs"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/preflight/parity"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/metadata"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// Real HTTP clients with production content negotiation; all resources below
// are synthetic fixtures, not evidence from EKS, GKE or an on-prem cluster.
func resourceClients(t *testing.T, handler http.HandlerFunc) (metadata.Interface, dynamic.Interface) {
	t.Helper()
	s := httptest.NewServer(handler)
	t.Cleanup(s.Close)
	path := filepath.Join(t.TempDir(), "kubeconfig")
	cfg := clientcmdapi.Config{CurrentContext: "wrong-default",
		Clusters:  map[string]*clientcmdapi.Cluster{"test": {Server: s.URL}},
		Contexts:  map[string]*clientcmdapi.Context{"bound": {Cluster: "test", AuthInfo: "test"}},
		AuthInfos: map[string]*clientcmdapi.AuthInfo{"test": {}},
	}
	if err := clientcmd.WriteToFile(cfg, path); err != nil {
		t.Fatal(err)
	}
	meta, dyn, err := envs.NewReadClients(envs.EnvConfig{Kubeconfig: path, KubeContext: "bound"}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return meta, dyn
}

func resourceCheck(t *testing.T, r parity.Result, id string) parity.Check {
	t.Helper()
	for _, c := range r.Checks {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("missing %s in %+v", id, r)
	return parity.Check{}
}

func TestSecretsMetadataAndFailures(t *testing.T) {
	for _, tc := range []struct {
		name string
		code int
		kind string
		want parity.Status
	}{
		{"exists", 200, "PartialObjectMetadata", parity.Pass},
		{"missing", 404, "Status", parity.Fail},
		{"forbidden", 403, "Status", parity.Unknown},
		{"unauthorized", 401, "Status", parity.Unknown},
		{"negotiation unsupported", 406, "Status", parity.Unknown},
		{"full object rejected", 200, "Secret", parity.Unknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			meta, _ := resourceClients(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != "GET" || r.URL.Path != "/api/v1/namespaces/shop/secrets/app-runtime" {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				}
				if r.Header.Get("Accept") != "application/json;as=PartialObjectMetadata;g=meta.k8s.io;v=v1" {
					t.Errorf("full object fallback: %q", r.Header.Get("Accept"))
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.code)
				if tc.code == 200 {
					version := "meta.k8s.io/v1"
					if tc.kind == "Secret" {
						version = "v1"
					}
					json.NewEncoder(w).Encode(map[string]any{"apiVersion": version, "kind": tc.kind,
						"metadata": map[string]any{"name": "app-runtime", "namespace": "shop", "annotations": map[string]string{"sensitive": "DO-NOT-REPORT"}}})
				} else {
					json.NewEncoder(w).Encode(map[string]any{"apiVersion": "v1", "kind": "Status", "status": "Failure", "code": tc.code, "message": "DO-NOT-REPORT"})
				}
			})
			p, err := NewSecrets(target(), meta, []SecretRequirement{{ID: "runtime", Name: "app-runtime"}})
			if err != nil {
				t.Fatal(err)
			}
			r, err := p.Check(context.Background(), target())
			if err != nil || resourceCheck(t, r, "runtime").Status != tc.want {
				t.Fatalf("%+v %v", r, err)
			}
			if r.Status == parity.Pass || calls != 1 {
				t.Fatalf("incomplete capability passed or fallback attempted: %+v calls=%d", r, calls)
			}
			b, _ := json.Marshal(r)
			if strings.Contains(string(b), "DO-NOT-REPORT") || strings.Contains(string(b), "app-runtime") {
				t.Fatalf("response leaked: %s", b)
			}
			wrong := target()
			wrong.Environment = "eks"
			if _, err := p.Check(context.Background(), wrong); err != ErrWrongTarget || calls != 1 {
				t.Fatal("wrong environment was queried")
			}
		})
	}
}

func TestAuditCollectorReadinessIsNotDelivery(t *testing.T) {
	for _, tc := range []struct {
		name                                     string
		desired, available, generation, observed int
		want                                     parity.Status
	}{
		{"available", 1, 1, 3, 3, parity.Pass},
		{"down", 1, 0, 3, 3, parity.Fail},
		{"scaled down", 0, 0, 3, 3, parity.Fail},
		{"old status", 1, 1, 3, 2, parity.Unknown},
		{"no generation", 1, 1, 0, 0, parity.Unknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, dyn := resourceClients(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" || r.URL.Path != "/apis/apps/v1/namespaces/logging/deployments/audit-collector" {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				}
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(map[string]any{"apiVersion": "apps/v1", "kind": "Deployment",
					"metadata": map[string]any{"name": "audit-collector", "namespace": "logging", "generation": tc.generation},
					"spec":     map[string]any{"replicas": tc.desired}, "status": map[string]any{"availableReplicas": tc.available, "observedGeneration": tc.observed}})
			})
			p, err := NewAuditCollector(target(), dyn, "logging", "audit-collector")
			if err != nil {
				t.Fatal(err)
			}
			r, err := p.Check(context.Background(), target())
			if err != nil || resourceCheck(t, r, "audit-collector").Status != tc.want || r.Status == parity.Pass || resourceCheck(t, r, "audit-delivery").Status != parity.Unknown {
				t.Fatalf("%+v %v", r, err)
			}
		})
	}
}

func TestBackupChecksDoNotProveRestore(t *testing.T) {
	for _, tc := range []struct {
		name, archiver, timestamp string
		storeStatus               int
		wantWAL, wantBackup       parity.Status
	}{
		{"healthy", "True", time.Now().Add(-time.Hour).UTC().Format(time.RFC3339), 200, parity.Pass, parity.Pass},
		{"stale", "True", time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339), 200, parity.Pass, parity.Fail},
		{"wal failed", "False", time.Now().Add(-time.Hour).UTC().Format(time.RFC3339), 200, parity.Fail, parity.Pass},
		{"no WAL condition", "", "", 200, parity.Unknown, parity.Unknown},
		{"invalid timestamp", "True", "not-a-time", 200, parity.Pass, parity.Unknown},
		{"future timestamp", "True", time.Now().Add(time.Hour).UTC().Format(time.RFC3339), 200, parity.Pass, parity.Unknown},
		{"store forbidden", "True", "", 403, parity.Pass, parity.Unknown},
		{"store missing", "True", "", 404, parity.Pass, parity.Fail},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, dyn := resourceClients(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" {
					t.Errorf("write request: %s", r.Method)
				}
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/apis/postgresql.cnpg.io/v1/namespaces/shop/clusters/db":
					json.NewEncoder(w).Encode(map[string]any{"apiVersion": "postgresql.cnpg.io/v1", "kind": "Cluster", "metadata": map[string]any{"name": "db", "namespace": "shop"},
						"spec":   map[string]any{"plugins": []any{map[string]any{"name": "barman-cloud.cloudnative-pg.io", "isWALArchiver": true, "parameters": map[string]any{"barmanObjectName": "backup", "serverName": "actual-server"}}}},
						"status": map[string]any{"conditions": []any{map[string]any{"type": "ContinuousArchiving", "status": tc.archiver}}}})
				case "/apis/barmancloud.cnpg.io/v1/namespaces/shop/objectstores/backup":
					w.WriteHeader(tc.storeStatus)
					if tc.storeStatus != 200 {
						json.NewEncoder(w).Encode(map[string]any{"apiVersion": "v1", "kind": "Status", "status": "Failure", "code": tc.storeStatus, "message": "DO-NOT-REPORT"})
						return
					}
					json.NewEncoder(w).Encode(map[string]any{"apiVersion": "barmancloud.cnpg.io/v1", "kind": "ObjectStore", "metadata": map[string]any{"name": "backup", "namespace": "shop"},
						"status": map[string]any{"serverRecoveryWindow": map[string]any{
							"actual-server": map[string]any{"lastSuccessfulBackupTime": tc.timestamp},
							"db":            map[string]any{"lastSuccessfulBackupTime": time.Now().UTC().Format(time.RFC3339)},
						}}})
				default:
					t.Errorf("unexpected resource: %s", r.URL.Path)
					w.WriteHeader(500)
				}
			})
			p, err := NewBackup(target(), dyn, "db", 24*time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			r, err := p.Check(context.Background(), target())
			if err != nil || resourceCheck(t, r, "wal-archiving").Status != tc.wantWAL || resourceCheck(t, r, "recent-backup").Status != tc.wantBackup || resourceCheck(t, r, "restore").Status != parity.Unknown || r.Status == parity.Pass {
				t.Fatalf("%+v %v", r, err)
			}
			b, _ := json.Marshal(r)
			if strings.Contains(string(b), "DO-NOT-REPORT") {
				t.Fatalf("provider error leaked: %s", b)
			}
		})
	}
}

func TestResourceProbesUnavailableAndCancelled(t *testing.T) {
	meta, dyn := resourceClients(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(403)
		json.NewEncoder(w).Encode(map[string]any{"apiVersion": "v1", "kind": "Status", "status": "Failure", "code": 403, "message": "DO-NOT-REPORT"})
	})
	s, _ := NewSecrets(target(), meta, []SecretRequirement{{ID: "runtime", Name: "runtime"}})
	a, _ := NewAuditCollector(target(), dyn, "logging", "collector")
	b, _ := NewBackup(target(), dyn, "db", time.Hour)
	for _, p := range []parity.Probe{s, a, b} {
		r, err := p.Check(context.Background(), target())
		if err != nil || r.Status != parity.Unknown {
			t.Fatalf("denied access: %+v %v", r, err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		r, _ = p.Check(ctx, target())
		if r.Status != parity.Unknown {
			t.Fatalf("cancelled: %+v", r)
		}
		wrong := target()
		wrong.Environment = "eks"
		if _, err := p.Check(context.Background(), wrong); err != ErrWrongTarget {
			t.Fatal("wrong target accepted")
		}
	}
}

func TestResourceProbeRequestsRespectDeadline(t *testing.T) {
	meta, dyn := resourceClients(t, func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	s, _ := NewSecrets(target(), meta, []SecretRequirement{{ID: "runtime", Name: "runtime"}})
	a, _ := NewAuditCollector(target(), dyn, "logging", "collector")
	b, _ := NewBackup(target(), dyn, "db", time.Hour)
	for _, p := range []parity.Probe{s, a, b} {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		start := time.Now()
		r, err := p.Check(ctx, target())
		cancel()
		if err != nil || r.Status != parity.Unknown || time.Since(start) > time.Second {
			t.Fatalf("request did not remain bounded/unknown: %+v %v", r, err)
		}
	}
}

func TestBackupMissingOrUnsupportedConfigurationIsNotSuccess(t *testing.T) {
	for name, spec := range map[string]string{
		"no plugin":        `{}`,
		"malformed plugin": `{"plugins":"invalid"}`,
		"disabled plugin":  `{"plugins":[{"name":"barman-cloud.cloudnative-pg.io","enabled":false,"isWALArchiver":true,"parameters":{"barmanObjectName":"backup"}}]}`,
		"not archiver":     `{"plugins":[{"name":"barman-cloud.cloudnative-pg.io","parameters":{"barmanObjectName":"backup"}}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, dyn := resourceClients(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/apis/postgresql.cnpg.io/v1/namespaces/shop/clusters/db" {
					t.Errorf("unexpected secondary read: %s", r.URL.Path)
				}
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(`{"apiVersion":"postgresql.cnpg.io/v1","kind":"Cluster","metadata":{"namespace":"shop","name":"db"},"spec":` + spec + `,"status":{"conditions":[{"type":"ContinuousArchiving","status":"True"}]}}`))
			})
			p, _ := NewBackup(target(), dyn, "db", time.Hour)
			r, err := p.Check(context.Background(), target())
			if err != nil || r.Status != parity.Unknown || resourceCheck(t, r, "backup-config").Status != parity.Unknown {
				t.Fatalf("%+v %v", r, err)
			}
		})
	}
}

func TestResourceProbeValidation(t *testing.T) {
	meta, dyn := resourceClients(t, func(w http.ResponseWriter, r *http.Request) { t.Error("validation contacted API") })
	for _, refs := range [][]SecretRequirement{nil, {{ID: "runtime", Name: "../secret"}}, {{ID: "secret-usability", Name: "runtime"}}, {{ID: "same", Name: "a"}, {ID: "same", Name: "b"}}} {
		if _, err := NewSecrets(target(), meta, refs); err != ErrInvalidConfig {
			t.Fatal("invalid references accepted")
		}
	}
	if _, err := NewAuditCollector(target(), dyn, "../namespace", "collector"); err != ErrInvalidConfig {
		t.Fatal("invalid collector accepted")
	}
	if _, err := NewBackup(target(), dyn, "db", 0); err != ErrInvalidConfig {
		t.Fatal("invalid backup age accepted")
	}
}
