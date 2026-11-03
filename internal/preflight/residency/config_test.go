package residency

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/preflight"
)

func fixtureConfig() Config {
	return Config{
		Version:       1,
		Environments:  []Environment{{"aws", "KR"}, {"onprem", "KR"}, {"tokyo", "JP"}, {"us", "US"}},
		APACCountries: []string{"KR", "JP"},
		Catalog: []ClusterCatalog{{
			Scope: preflight.ClusterScope{Environment: "aws", Namespace: "shop", Cluster: "db"},
			Datasets: []ClassifiedDataset{
				{preflight.Dataset{Database: "app", Schema: "public", Table: "users"}, "kr-only"},
				{preflight.Dataset{Database: "another_db", Schema: "sales", Table: "products"}, "global"},
			},
		}},
	}
}

func TestConfigStrictParsing(t *testing.T) {
	good, err := json.Marshal(fixtureConfig())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseConfig(good); err != nil {
		t.Fatal(err)
	}
	for name, raw := range map[string]string{
		"empty": "", "malformed": "catalog: [TOPSECRET", "unknown": `{"version":1,"TOPSECRET":"value"}`,
		"duplicate yaml": "version: 1\nversion: 2", "duplicate json": `{"version":1,"version":2}`,
		"type": `{"version":"TOPSECRET"}`, "null": "null",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParseConfig([]byte(raw))
			if !errors.Is(err, ErrInvalidConfig) || strings.Contains(err.Error(), "TOPSECRET") {
				t.Fatalf("unsafe or missing error: %v", err)
			}
		})
	}
}

func TestConfigRejectsInvalidEntries(t *testing.T) {
	cases := map[string]func(*Config){
		"version":                   func(c *Config) { c.Version = 2 },
		"empty environments":        func(c *Config) { c.Environments = nil },
		"empty catalog":             func(c *Config) { c.Catalog = nil },
		"empty apac":                func(c *Config) { c.APACCountries = nil },
		"duplicate environment":     func(c *Config) { c.Environments = append(c.Environments, c.Environments[0]) },
		"invalid environment":       func(c *Config) { c.Environments[0].ID = "postgres://TOPSECRET" },
		"country unknown":           func(c *Config) { c.Environments[0].Country = "ZZ" },
		"country lowercase":         func(c *Config) { c.Environments[0].Country = "kr" },
		"macroregion":               func(c *Config) { c.Environments[0].Country = "EU" },
		"country alias":             func(c *Config) { c.Environments[0].Country = "UK" },
		"country numeric":           func(c *Config) { c.Environments[0].Country = "410" },
		"apac unknown":              func(c *Config) { c.APACCountries = []string{"XX"} },
		"apac duplicate":            func(c *Config) { c.APACCountries = []string{"KR", "KR"} },
		"duplicate cluster":         func(c *Config) { c.Catalog = append(c.Catalog, c.Catalog[0]) },
		"missing scope environment": func(c *Config) { c.Catalog[0].Scope.Environment = "missing" },
		"bad cluster":               func(c *Config) { c.Catalog[0].Scope.Cluster = "TOPSECRET\n" },
		"empty datasets":            func(c *Config) { c.Catalog[0].Datasets = nil },
		"unknown label":             func(c *Config) { c.Catalog[0].Datasets[0].Label = "TOPSECRET" },
		"duplicate dataset":         func(c *Config) { c.Catalog[0].Datasets = append(c.Catalog[0].Datasets, c.Catalog[0].Datasets[0]) },
		"bad dataset":               func(c *Config) { c.Catalog[0].Datasets[0].Dataset.Table = "TOPSECRET\n" },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := fixtureConfig()
			change(&cfg)
			_, err := cfg.normalized()
			if !errors.Is(err, ErrInvalidConfig) || strings.Contains(err.Error(), "TOPSECRET") {
				t.Fatalf("unsafe or missing error: %v", err)
			}
		})
	}
}

func TestCountryCodes(t *testing.T) {
	codes := strings.Fields(countries)
	seen := map[string]bool{}
	if len(codes) != 249 {
		t.Fatalf("country table has %d codes", len(codes))
	}
	for _, code := range codes {
		if seen[code] || !validCountry(code) {
			t.Fatalf("invalid/duplicate ISO code %s", code)
		}
		seen[code] = true
	}
}
