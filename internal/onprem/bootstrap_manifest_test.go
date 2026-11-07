package onprem

import (
	"encoding/json"
	"strings"
	"testing"
)

func manifestConfig() BootstrapConfig {
	return BootstrapConfig{EnrollmentID: "enr_0123456789abcdef01234567", Origin: "https://api.example.test", ConnectionToken: strings.Repeat("a", 48), Hostname: "hop-k3s-0123456789abcdef01234567", TailnetAuthKey: "tskey-auth-test-key", AgentImage: "example.test/agent@sha256:" + strings.Repeat("1", 64), TailscaleImage: "tailscale/tailscale@sha256:" + strings.Repeat("2", 64), AgentArch: "amd64", AgentURL: "https://api.example.test/v1/onprem/agent.bin?arch=amd64", AgentSHA256: strings.Repeat("3", 64)}
}

func TestBootstrapManifestScopesCredentialsAndPreservesKubernetesTLS(t *testing.T) {
	c := manifestConfig()
	data, err := BootstrapManifest(c)
	if err != nil {
		t.Fatal(err)
	}
	var list struct {
		Items []map[string]any `json:"items"`
	}
	if err = json.Unmarshal(data, &list); err != nil {
		t.Fatal(err)
	}
	var deployment map[string]any
	var tokenRule, admission bool
	for _, o := range list.Items {
		if o["kind"] == "Deployment" {
			deployment = o
		}
		if o["kind"] == "ValidatingAdmissionPolicy" {
			admission = true
		}
		if o["kind"] == "Role" && strings.Contains(string(mustJSON(o)), "serviceaccounts/token") {
			tokenRule = true
			if !strings.Contains(string(mustJSON(o)), "resourceNames") {
				t.Fatal("token minting is not resource scoped")
			}
		}
	}
	if deployment == nil || !tokenRule || !admission {
		t.Fatal("missing connector, scoped renewal, or protected-namespace admission")
	}
	encoded := string(mustJSON(deployment))
	if strings.Contains(encoded, c.ConnectionToken) || strings.Contains(encoded, c.TailnetAuthKey) {
		t.Fatal("credentials embedded in pod template instead of Secret references")
	}
	if !strings.Contains(string(data), `kubernetes.default.svc:443`) || strings.Contains(string(data), `insecure-skip-tls-verify`) {
		t.Fatal("Kubernetes TLS forwarding was changed")
	}
}

func TestBootstrapManifestRejectsUntrustedConfiguration(t *testing.T) {
	tests := []func(*BootstrapConfig){func(c *BootstrapConfig) { c.Origin = "http://api.test" }, func(c *BootstrapConfig) { c.Hostname = "attacker" }, func(c *BootstrapConfig) { c.AgentImage = "example.test/agent:latest" }, func(c *BootstrapConfig) { c.ConnectionToken = "x\nAuthorization: x" }, func(c *BootstrapConfig) { c.EnrollmentID = "../../etc/passwd" }}
	for i, change := range tests {
		c := manifestConfig()
		change(&c)
		if _, err := BootstrapManifest(c); err == nil {
			t.Fatalf("invalid case %d accepted", i)
		}
	}
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }
