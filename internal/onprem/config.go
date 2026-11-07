package onprem

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/envs"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/secrets"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// FromEnv is opt-in because registration creates scoped credentials, a tailnet
// device and Argo resources. Missing setup never falls back to bootstrap-only.
func FromEnv(ctx context.Context, getenv func(string) string, st store.EnrollmentStore, cipher *secrets.Cipher, registry *envs.Service) (*Connector, error) {
	if value := getenv("PLATFORM_ONPREM_REGISTRATION"); value != "1" {
		if value != "" && value != "0" {
			return nil, errors.New("PLATFORM_ONPREM_REGISTRATION must be 0 or 1")
		}
		return nil, nil
	}
	if cipher == nil {
		return nil, errors.New("onprem registration requires the configured secrets encryption key")
	}
	tailnet, err := TailnetFromEnv(ctx, getenv)
	if err != nil {
		return nil, err
	}
	if tailnet == nil {
		return nil, errors.New("dedicated onprem tailnet OAuth client is not configured")
	}
	rc, err := rest.InClusterConfig()
	if err != nil {
		return nil, errors.New("onprem registration requires its management service account")
	}
	rc.Timeout = 15 * time.Second
	k, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return nil, errors.New("invalid registration management client")
	}
	dyn, err := dynamic.NewForConfig(rc)
	if err != nil {
		return nil, errors.New("invalid registration management client")
	}
	profile := map[string]any{}
	data, err := os.ReadFile(getenv("PLATFORM_ONPREM_PROFILE_FILE"))
	if err != nil || len(data) > 1<<20 || json.Unmarshal(data, &profile) != nil || len(profile) == 0 {
		return nil, errors.New("onprem app profile file is missing or invalid")
	}
	base := getenv("PLATFORM_ONPREM_PUBLIC_URL")
	if base == "" {
		base = "https://api.hopcharlie.dev"
	}
	connector := &Connector{Store: st, Cipher: cipher, Tailnet: tailnet, Origin: base, AgentDirectory: getenv("PLATFORM_ONPREM_AGENT_DIRECTORY"), AgentImage: getenv("PLATFORM_ONPREM_AGENT_IMAGE"), TailscaleImage: getenv("PLATFORM_ONPREM_TAILSCALE_IMAGE")}
	connector.Runtime = NewKubeRuntime(KubeRuntimeConfig{Kube: k, Dynamic: dyn, Envs: registry, TailnetDomain: tailnet.Domain, RouteNamespace: "hopcharlie-db-network", ArgoNamespace: "argocd", ProxyClass: "hopcharlie-onprem-api-egress-management", HelloImage: getenv("PLATFORM_ONPREM_HELLO_IMAGE"), Profile: profile})
	// Live RBAC/admission may still be propagating during a rollout. Each
	// issuance and registration checks it; do not take the whole API down.
	return connector, nil
}
