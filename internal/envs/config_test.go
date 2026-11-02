package envs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadExampleConfig(t *testing.T) {
	cfg, err := LoadConfig(filepath.Join("..", "..", "examples", "envs.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range cfg.Environments {
		names = append(names, e.Name)
		if e.GatewayBackend != e.Name {
			t.Errorf("%s: gatewayBackend = %q, want the name", e.Name, e.GatewayBackend)
		}
	}
	if got := strings.Join(names, ","); got != "gke,eks,onprem" {
		t.Fatalf("names = %s, want config order gke,eks,onprem", got)
	}
	eks := cfg.Environments[1]
	if eks.Provider != "aws" || eks.DisplayName != "AWS EKS" || eks.Region != "ap-northeast-2" || eks.KubeContext != "eks-hopcharlie" {
		t.Fatalf("eks = %+v", eks)
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	path := writeFile(t, "envs.yaml", `
environments:
  - name: onprem
    provider: onprem
    kubeContext: k3s
    kubeconfig: /etc/hopcharlie/k3s.yaml
  - name: eks-2
    provider: aws
    kubeContext: eks
    displayName: Second EKS
    gatewayBackend: eks-b
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []EnvConfig{
		{Name: "onprem", Provider: "onprem", DisplayName: "onprem", KubeContext: "k3s", Kubeconfig: "/etc/hopcharlie/k3s.yaml", GatewayBackend: "onprem"},
		{Name: "eks-2", Provider: "aws", DisplayName: "Second EKS", KubeContext: "eks", GatewayBackend: "eks-b"},
	}
	for i, w := range want {
		if cfg.Environments[i] != w {
			t.Errorf("env %d = %+v, want %+v", i, cfg.Environments[i], w)
		}
	}
}

func TestLoadConfigInvalid(t *testing.T) {
	cases := map[string]struct{ yaml, want string }{
		"empty":           {`environments: []`, "no environments"},
		"missing list":    {`{}`, "no environments"},
		"uppercase name":  {"environments:\n  - {name: GKE, provider: gcp, kubeContext: g}", "must match"},
		"underscore name": {"environments:\n  - {name: on_prem, provider: onprem, kubeContext: k}", "must match"},
		"empty name":      {"environments:\n  - {provider: gcp, kubeContext: g}", "must match"},
		"duplicate name": {"environments:\n  - {name: gke, provider: gcp, kubeContext: a}\n  - {name: gke, provider: gcp, kubeContext: b}",
			`duplicate name "gke"`},
		"bad provider":     {"environments:\n  - {name: aks, provider: azure, kubeContext: a}", `provider "azure"`},
		"missing provider": {"environments:\n  - {name: gke, kubeContext: g}", `provider ""`},
		"missing context":  {"environments:\n  - {name: gke, provider: gcp}", "kubeContext is required"},
		"unknown field":    {"environments:\n  - {name: gke, provider: gcp, kubeContext: g, context: g}", "unknown field"},
		"not yaml":         {"environments: [", "parse environments file"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := LoadConfig(writeFile(t, "envs.yaml", tc.yaml))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestLoadConfigMissingFile(t *testing.T) {
	if _, err := LoadConfig(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
		t.Fatal("want an error for a missing file")
	}
}

func TestNewValidatesConfig(t *testing.T) {
	if _, err := New(Config{}, Options{}); err == nil {
		t.Fatal("New accepted an empty config")
	}
	s, err := New(Config{Environments: []EnvConfig{{Name: "gke", Provider: "gcp", KubeContext: "g"}}}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	c, ok := s.Config("gke")
	if !ok || c.DisplayName != "gke" || c.GatewayBackend != "gke" {
		t.Fatalf("Config(gke) = %+v, %v; want defaults applied", c, ok)
	}
	if _, ok := s.Config("eks"); ok {
		t.Fatal("Config(eks) found an unregistered environment")
	}
}
