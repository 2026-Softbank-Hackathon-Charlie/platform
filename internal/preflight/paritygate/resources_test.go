package paritygate

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/preflight/parity"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/metadata"
	"k8s.io/client-go/rest"
)

const resourceConfig = `version: 1
mode: audit
environments:
  eks: {}
  gke: {}
  onprem:
    auditCollector: {namespace: logging, deployment: audit-collector}
apps:
  - name: shop
    namespace: shop
    environments:
      onprem:
        secrets: [{id: runtime, name: onprem-runtime}]
        backup: {cluster: onprem-db, maxAge: 26h}
`

func TestResourceFactoryBoundToEnvironmentAndMissingContext(t *testing.T) {
	cfg, err := ParseConfig([]byte(resourceConfig), []string{"eks", "gke", "onprem"})
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	g := NewWithResources(cfg, nil, nil, func(env string) (metadata.Interface, dynamic.Interface, error) {
		calls++
		if env != "onprem" {
			t.Errorf("queried wrong environment %q", env)
		}
		return nil, nil, errors.New("sensitive kubeconfig detail")
	})
	p := shopPlan()
	p.Target.Env = "onprem"
	p.Target.Cluster = "onprem-db"
	// Untrusted endpoint fields must not select the client's kubeconfig.
	p.Target.Kubeconfig, p.Target.Context = "untrusted", "untrusted"
	r, err := g.Check(context.Background(), p)
	if err != nil || calls != 1 || r.Passed {
		t.Fatalf("%+v %v calls=%d", r, err, calls)
	}
	for _, cap := range []parity.Capability{parity.Secret, parity.Audit, parity.BackupRestore} {
		if c := result(r, cap); c.Status != parity.Unknown || c.Code != "probe_failed" {
			t.Fatalf("%s %+v", cap, c)
		}
	}
	if strings.Contains(strings.Join(Lines(r), "\n"), "sensitive") {
		t.Fatal("factory error leaked")
	}
	p.Target.Cluster = "different-db"
	r, err = g.Check(context.Background(), p)
	if err != nil || result(r, parity.BackupRestore).Code != "backup_target_mismatch" {
		t.Fatalf("unrelated DB was checked: %+v %v", r, err)
	}
	// EKS has no resource requirements: do not inherit on-prem names or clients.
	before := calls
	p.Target.Env = "eks"
	r, err = g.Check(context.Background(), p)
	if err != nil || calls != before {
		t.Fatalf("unexpected client use: %v calls=%d", err, calls)
	}
	for _, cap := range []parity.Capability{parity.Secret, parity.Audit, parity.BackupRestore} {
		if c := result(r, cap); c.Code != "probe_not_configured" {
			t.Fatalf("%s %+v", cap, c)
		}
	}
}

func TestResourceGatePreservesPartialEvidence(t *testing.T) {
	cfg, err := ParseConfig([]byte(resourceConfig), []string{"eks", "gke", "onprem"})
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != "GET" {
			t.Errorf("write request: %s", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		var body string
		switch r.URL.Path {
		case "/api/v1/namespaces/shop/secrets/onprem-runtime":
			body = `{"apiVersion":"meta.k8s.io/v1","kind":"PartialObjectMetadata","metadata":{"namespace":"shop","name":"onprem-runtime"}}`
		case "/apis/apps/v1/namespaces/logging/deployments/audit-collector":
			body = `{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"namespace":"logging","name":"audit-collector","generation":1},"spec":{"replicas":1},"status":{"observedGeneration":1,"availableReplicas":1}}`
		case "/apis/postgresql.cnpg.io/v1/namespaces/shop/clusters/onprem-db":
			body = `{"apiVersion":"postgresql.cnpg.io/v1","kind":"Cluster","metadata":{"namespace":"shop","name":"onprem-db"},"spec":{"plugins":[{"name":"barman-cloud.cloudnative-pg.io","isWALArchiver":true,"parameters":{"barmanObjectName":"backup"}}]},"status":{"conditions":[{"type":"ContinuousArchiving","status":"True"}]}}`
		case "/apis/barmancloud.cnpg.io/v1/namespaces/shop/objectstores/backup":
			json.NewEncoder(w).Encode(map[string]any{"apiVersion": "barmancloud.cnpg.io/v1", "kind": "ObjectStore", "metadata": map[string]any{"namespace": "shop", "name": "backup"}, "status": map[string]any{"serverRecoveryWindow": map[string]any{"onprem-db": map[string]any{"lastSuccessfulBackupTime": time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)}}}})
			return
		default:
			t.Errorf("wrong reference: %s", r.URL.Path)
			w.WriteHeader(500)
			return
		}
		w.Write([]byte(body))
	}))
	defer s.Close()
	meta, err := metadata.NewForConfig(&rest.Config{Host: s.URL})
	if err != nil {
		t.Fatal(err)
	}
	dyn, err := dynamic.NewForConfig(&rest.Config{Host: s.URL})
	if err != nil {
		t.Fatal(err)
	}
	g := NewWithResources(cfg, nil, nil, func(env string) (metadata.Interface, dynamic.Interface, error) {
		if env != "onprem" {
			t.Errorf("wrong environment: %s", env)
		}
		return meta, dyn, nil
	})
	p := shopPlan()
	p.Target.Env, p.Target.Cluster = "onprem", "onprem-db"
	r, err := g.Check(context.Background(), p)
	if err != nil || r.Passed || calls != 4 {
		t.Fatalf("%+v %v calls=%d", r, err, calls)
	}
	for _, cap := range []parity.Capability{parity.Secret, parity.Audit, parity.BackupRestore} {
		x := result(r, cap)
		if x.Status != parity.Unknown || len(x.Checks) < 2 || x.Checks[0].Status != parity.Pass {
			t.Fatalf("lost partial evidence: %+v", x)
		}
	}
	lines := strings.Join(Lines(r), "\n")
	for _, want := range []string{"runtime=pass(secret_metadata_observed)", "audit-delivery=unknown(audit_delivery_not_verified)", "recent-backup=pass(recent_backup_reported)", "restore=unknown(restore_not_verified)"} {
		if !strings.Contains(lines, want) {
			t.Fatalf("missing %q in %s", want, lines)
		}
	}
}

func TestResourceConfigValidation(t *testing.T) {
	for name, replacement := range map[string][2]string{
		"unknown app environment":     {"      onprem:", "      mars:"},
		"secret traversal":            {"name: onprem-runtime", "name: ../runtime"},
		"reserved check id":           {"id: runtime", "id: secret-usability"},
		"duplicate secret id":         {"secrets: [{id: runtime, name: onprem-runtime}]", "secrets: [{id: runtime, name: a}, {id: runtime, name: b}]"},
		"missing backup age":          {", maxAge: 26h", ""},
		"too long backup age":         {"maxAge: 26h", "maxAge: 721h"},
		"too short backup age":        {"maxAge: 26h", "maxAge: 59s"},
		"invalid cluster":             {"cluster: onprem-db", "cluster: ../db"},
		"invalid collector namespace": {"namespace: logging", "namespace: Logging"},
		"invalid deployment":          {"deployment: audit-collector", "deployment: ../collector"},
		"unknown field":               {"maxAge: 26h", "maxAge: 26h, verifiedRestore: true"},
	} {
		t.Run(name, func(t *testing.T) {
			raw := strings.Replace(resourceConfig, replacement[0], replacement[1], 1)
			if _, err := ParseConfig([]byte(raw), []string{"eks", "gke", "onprem"}); !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("accepted invalid config: %v", err)
			}
		})
	}
}

func TestThreeEnvironmentTemplateLoads(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "examples", "parity-three-environments.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	c, err := ParseConfig(raw, []string{"eks", "gke", "onprem"})
	if err != nil || len(c.Apps) != 1 || len(c.Apps[0].Environments) != 3 {
		t.Fatalf("%+v %v", c, err)
	}
	if _, err := ParseConfig(raw, []string{"eks", "gke"}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatal("unregistered onprem accepted")
	}
}
