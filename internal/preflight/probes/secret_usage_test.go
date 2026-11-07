package probes

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/preflight/parity"
)

func TestSecretUsageFreshnessCoverageAndRedaction(t *testing.T) {
	target := parity.Target{Environment: "eks", Namespace: "shop", Application: "shop"}
	required := []SecretRequirement{{ID: "db", Name: "db-credentials"}}
	cfg := SecretUsage{Service: "shop", Port: 8080, Checks: []SecretUse{{ID: "db", SecretID: "db", Operation: "database-query"}}}
	for _, tc := range []struct {
		name  string
		reply func(string) ([]byte, error)
		want  parity.Status
	}{
		{"fresh", func(n string) ([]byte, error) {
			return json.Marshal(map[string]any{"version": 1, "challenge": n, "checks": map[string]string{"database-query": "pass"}})
		}, parity.Pass},
		{"operation failed", func(n string) ([]byte, error) {
			return json.Marshal(map[string]any{"version": 1, "challenge": n, "checks": map[string]string{"database-query": "fail"}})
		}, parity.Fail},
		{"old response", func(n string) ([]byte, error) {
			return json.Marshal(map[string]any{"version": 1, "challenge": "old", "checks": map[string]string{"database-query": "pass"}})
		}, parity.Unknown},
		{"missing operation", func(n string) ([]byte, error) {
			return json.Marshal(map[string]any{"version": 1, "challenge": n, "checks": map[string]string{}})
		}, parity.Unknown},
		{"ordinary readiness", func(string) ([]byte, error) { return []byte(`{"status":"ready"}`), nil }, parity.Unknown},
		{"too large", func(string) ([]byte, error) { return []byte(strings.Repeat("x", 8193)), nil }, parity.Unknown},
		{"provider error", func(string) ([]byte, error) { return nil, errors.New("password=do-not-print") }, parity.Unknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checks := checkSecretUsage(context.Background(), target, cfg, func(_ context.Context, got parity.Target, _ SecretUsage, n string) ([]byte, error) {
				if got != target || len(n) != 64 {
					t.Fatal("unbound request")
				}
				return tc.reply(n)
			}, required)
			last := checks[len(checks)-1]
			if last.Status != tc.want {
				t.Fatalf("%+v", checks)
			}
			raw, _ := json.Marshal(checks)
			if strings.Contains(string(raw), "do-not-print") {
				t.Fatal("provider error leaked")
			}
		})
	}
	checks := checkSecretUsage(context.Background(), target, cfg, func(_ context.Context, _ parity.Target, _ SecretUsage, n string) ([]byte, error) {
		return json.Marshal(map[string]any{"version": 1, "challenge": n, "checks": map[string]string{"database-query": "pass"}})
	}, append(required, SecretRequirement{ID: "uncovered", Name: "other"}))
	if checks[len(checks)-1].Status != parity.Unknown {
		t.Fatal("uncovered Secret passed")
	}
}

func TestRuntimeEvidenceCannotOverrideMissingSecret(t *testing.T) {
	target := parity.Target{Environment: "eks", Namespace: "shop", Application: "shop"}
	meta, _ := resourceClients(t, func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	p, err := NewSecrets(target, meta, []SecretRequirement{{ID: "db", Name: "db-credentials"}})
	if err != nil {
		t.Fatal(err)
	}
	err = p.WithUsage(SecretUsage{Service: "shop", Port: 8080, Checks: []SecretUse{{ID: "db", SecretID: "db", Operation: "database-query"}}}, func(_ context.Context, _ parity.Target, _ SecretUsage, n string) ([]byte, error) {
		return json.Marshal(map[string]any{"version": 1, "challenge": n, "checks": map[string]string{"database-query": "pass"}})
	})
	if err != nil {
		t.Fatal(err)
	}
	r, err := p.Check(context.Background(), target)
	if err != nil || r.Status != parity.Fail {
		t.Fatalf("%+v %v", r, err)
	}
	if _, err := p.Check(context.Background(), parity.Target{Environment: "gke", Namespace: "shop", Application: "shop"}); err != ErrWrongTarget {
		t.Fatal("wrong target accepted")
	}
}

func TestSecretUsageConfigRejectsUnknownBindings(t *testing.T) {
	c := SecretUsage{Service: "shop", Port: 8080, Checks: []SecretUse{{ID: "db", SecretID: "missing", Operation: "database-query"}}}
	if c.Validate([]SecretRequirement{{ID: "db", Name: "db"}}) == nil {
		t.Fatal("unregistered secret ID")
	}
	c.Checks[0].SecretID = "db"
	c.Checks[0].Operation = "write-arbitrary-data"
	if c.Validate([]SecretRequirement{{ID: "db", Name: "db"}}) == nil {
		t.Fatal("unsafe operation")
	}
	c.Service = "http://169.254.169.254"
	c.Checks[0].Operation = "database-query"
	if c.Validate([]SecretRequirement{{ID: "db", Name: "db"}}) == nil {
		t.Fatal("arbitrary endpoint")
	}
}
