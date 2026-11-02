// Package envs is the registry of the Kubernetes environments HopCharlie
// migrates between (GKE, EKS, on-prem k3s). It reports each environment's live
// cluster status: API server reachability and version, node readiness and CPU
// architectures, and the CloudNativePG clusters it runs.
package envs

import (
	"errors"
	"fmt"
	"os"
	"regexp"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
	"sigs.k8s.io/yaml"
)

// EnvConfig is one registered environment.
type EnvConfig struct {
	// Name is the environment id used in URLs and plans, e.g. "gke".
	Name string `json:"name"`
	// Provider is aws, gcp or onprem.
	Provider string `json:"provider"`
	// DisplayName defaults to Name.
	DisplayName string `json:"displayName,omitempty"`
	Region      string `json:"region,omitempty"`
	// KubeContext is the kubeconfig context of the environment's cluster.
	KubeContext string `json:"kubeContext"`
	// Kubeconfig is an optional kubeconfig path. Empty uses the default
	// loading rules ($KUBECONFIG, then ~/.kube/config).
	Kubeconfig string `json:"kubeconfig,omitempty"`
	// GatewayBackend is the traffic gateway backend serving this environment. Defaults to Name.
	GatewayBackend string `json:"gatewayBackend,omitempty"`
}

// Config is the environments file.
type Config struct {
	Environments []EnvConfig `json:"environments"`
}

var namePattern = regexp.MustCompile(`^[a-z0-9-]+$`)

// LoadConfig reads an environments file (YAML), validates it and applies defaults.
func LoadConfig(path string) (Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read environments file: %w", err)
	}
	var cfg Config
	if err := yaml.UnmarshalStrict(raw, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse environments file %s: %w", path, err)
	}
	return cfg.normalize()
}

// normalize validates c and returns a copy with defaults applied.
func (c Config) normalize() (Config, error) {
	if len(c.Environments) == 0 {
		return Config{}, errors.New("no environments configured")
	}
	out := Config{Environments: make([]EnvConfig, len(c.Environments))}
	seen := map[string]bool{}
	for i, e := range c.Environments {
		switch {
		case !namePattern.MatchString(e.Name):
			return Config{}, fmt.Errorf("environments[%d]: name %q must match %s", i, e.Name, namePattern)
		case seen[e.Name]:
			return Config{}, fmt.Errorf("environments[%d]: duplicate name %q", i, e.Name)
		case !apiv1.Provider(e.Provider).Valid():
			return Config{}, fmt.Errorf("environment %q: provider %q must be one of aws, gcp, onprem", e.Name, e.Provider)
		case e.KubeContext == "":
			// Without an explicit context the probe would report whatever
			// cluster the kubeconfig's current context points at.
			return Config{}, fmt.Errorf("environment %q: kubeContext is required", e.Name)
		}
		seen[e.Name] = true
		if e.DisplayName == "" {
			e.DisplayName = e.Name
		}
		if e.GatewayBackend == "" {
			e.GatewayBackend = e.Name
		}
		out.Environments[i] = e
	}
	return out, nil
}
