package paritygate

import (
	"strings"
	"testing"
)

func TestSecretUsageConfigBinding(t *testing.T) {
	raw := `version: 1
mode: audit
apps:
  - name: shop
    namespace: shop
    environments:
      eks:
        secrets: [{id: db, name: shop-db-app}]
        secretUsage:
          service: shop-web
          port: 8080
          checks: [{id: db, secretId: db, operation: database-query}]
`
	c, err := ParseConfig([]byte(raw), []string{"eks", "gke"})
	if err != nil {
		t.Fatal(err)
	}
	if c.Apps[0].Environments["eks"].SecretUsage.Service != "shop-web" || c.Apps[0].Environments["gke"].SecretUsage != nil {
		t.Fatal("environment binding lost")
	}
	for _, bad := range []string{strings.Replace(raw, "secretId: db", "secretId: missing", 1), strings.Replace(raw, "port: 8080", "port: 0", 1), strings.Replace(raw, "service: shop-web", "service: https://example.com", 1)} {
		if _, err := ParseConfig([]byte(bad), []string{"eks", "gke"}); err == nil {
			t.Fatal("unsafe binding accepted")
		}
	}
}
