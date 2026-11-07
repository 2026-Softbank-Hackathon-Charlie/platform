// Package paritygate runs RBAC, Prometheus and optional resource-evidence
// probes against a migration's target before it starts and
// returns the report for the audit log. A separate explicit per-app
// observability opt-in can block unverified signal delivery. Other capabilities
// retain audit semantics. Configuration is server-owned (PLATFORM_PARITY_FILE),
// never taken from a plan or an agent.
package paritygate

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/yaml"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/preflight/probes"
)

var ErrInvalidConfig = errors.New("invalid parity configuration")

// ModeAudit records the overall report without blanket enforcement. End-to-end secret usability,
// audit delivery, restore and alert verification remain incomplete.
const ModeAudit = "audit"

// Config is the parity file.
type Config struct {
	Version int    `json:"version"`
	Mode    string `json:"mode"`
	// Environments says how to reach each environment's monitoring. An
	// environment without an entry has no metrics probe.
	Environments map[string]EnvProbes `json:"environments,omitempty"`
	// Apps are matched to a plan by its target namespace.
	Apps []App `json:"apps"`
}

// EnvProbes are an environment's probe endpoints.
type EnvProbes struct {
	Prometheus     *Prometheus     `json:"prometheus,omitempty"`
	AuditCollector *AuditCollector `json:"auditCollector,omitempty"`
	Telemetry      *Telemetry      `json:"telemetry,omitempty"`
}

type Telemetry struct {
	ClusterName           string `json:"clusterName"`
	DeploymentEnvironment string `json:"deploymentEnvironment"`
	MaxAge                string `json:"maxAge,omitempty"`
}

func (t Telemetry) maxAge() (time.Duration, error) {
	if t.MaxAge == "" {
		return 2 * time.Minute, nil
	}
	d, err := time.ParseDuration(t.MaxAge)
	if err != nil || d < time.Second || d > 10*time.Minute {
		return 0, ErrInvalidConfig
	}
	return d, nil
}

// Required signals are an explicit per-app opt-in. Existing global audit mode
// remains unchanged and never becomes a blanket enforce switch.
type ObservationRequirement struct {
	RequiredSignals []string `json:"requiredSignals"`
	ServiceNames    []string `json:"serviceNames,omitempty"`
	Enforce         bool     `json:"enforce,omitempty"`
}

// AuditCollector names an existing collector, not a claim of audit coverage.
// Managed audit services without a collector Deployment remain unconfigured.
type AuditCollector struct {
	Namespace  string `json:"namespace"`
	Deployment string `json:"deployment"`
}

// AppResources must be reviewed separately per environment. Names are not
// inferred from a Plan, copied from another environment, or submitted by AI.
type AppResources struct {
	CloudIAM    *probes.CloudIAM           `json:"cloudIAM,omitempty"`
	Secrets     []probes.SecretRequirement `json:"secrets,omitempty"`
	SecretUsage *probes.SecretUsage        `json:"secretUsage,omitempty"`
	Backup      *Backup                    `json:"backup,omitempty"`
}

type Backup struct {
	Cluster string `json:"cluster"`
	MaxAge  string `json:"maxAge"`
}

func (b Backup) maxAge() (time.Duration, error) {
	d, err := time.ParseDuration(b.MaxAge)
	if err != nil || d < time.Minute || d > 30*24*time.Hour {
		return 0, ErrInvalidConfig
	}
	return d, nil
}

// Prometheus is a Prometheus HTTP API the platform can reach without
// credentials (for example the in-cluster service of kube-prometheus-stack).
type Prometheus struct {
	URL string `json:"url"`
	// MaxAge is the oldest acceptable last scrape, e.g. "2m" (1s to 10m).
	MaxAge string            `json:"maxAge,omitempty"`
	Labels map[string]string `json:"labels,omitempty"`
}

// App is what parity expects of one app in every environment.
type App struct {
	// Baselines are explicit operator-reviewed source requirements. Missing or
	// expired baselines never imply an empty set of source dependencies.
	Baselines map[string]Baseline `json:"baselines,omitempty"`
	Name      string              `json:"name"`
	Namespace string              `json:"namespace"`
	// ServiceAccount is the account the app's pods run as; the RBAC rules
	// are evaluated for it with SubjectAccessReview.
	ServiceAccount string `json:"serviceAccount,omitempty"`
	// RBAC needs at least one allowed and one denied rule.
	RBAC []Rule `json:"rbac,omitempty"`
	// PrometheusJob is the scrape job of the app's targets.
	PrometheusJob string                  `json:"prometheusJob,omitempty"`
	Environments  map[string]AppResources `json:"environments,omitempty"`
	Observability *ObservationRequirement `json:"observability,omitempty"`
}

type Baseline struct {
	Revision   string    `json:"revision"`
	ReviewedAt time.Time `json:"reviewedAt"`
	ExpiresAt  time.Time `json:"expiresAt"`
}

// Rule is one Kubernetes operation the app must (allowed) or must not be
// able to do. Namespace defaults to the app's namespace in the target.
type Rule struct {
	ID          string `json:"id"`
	Verb        string `json:"verb"`
	Group       string `json:"group,omitempty"`
	Resource    string `json:"resource"`
	Subresource string `json:"subresource,omitempty"`
	Name        string `json:"name,omitempty"`
	Namespace   string `json:"namespace,omitempty"`
	// Allowed is required: a forgotten field must not silently mean "denied".
	Allowed *bool `json:"allowed"`
}

var id = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// ParseConfig parses and validates a parity file. envs are the registered
// environment names; an entry for any other environment is an error.
func ParseConfig(raw []byte, envs []string) (Config, error) {
	var c Config
	if err := yaml.UnmarshalStrict(raw, &c); err != nil {
		return Config{}, fmt.Errorf("%w: malformed document or unknown fields", ErrInvalidConfig)
	}
	return c, c.validate(envs)
}

func (c Config) validate(envs []string) error {
	bad := func(format string, a ...any) error {
		return fmt.Errorf("%w: "+format, append([]any{ErrInvalidConfig}, a...)...)
	}
	if c.Version != 1 {
		return bad("version must be 1")
	}
	if c.Mode != ModeAudit {
		return bad("mode must be %q (only explicit per-app observability may enforce)", ModeAudit)
	}
	clusters := map[string]bool{}
	for name, e := range c.Environments {
		if !slices.Contains(envs, name) {
			return bad("environments.%s is not a registered environment", name)
		}
		if e.Telemetry != nil {
			if !id.MatchString(e.Telemetry.ClusterName) || !id.MatchString(e.Telemetry.DeploymentEnvironment) || clusters[e.Telemetry.ClusterName] {
				return bad("environments.%s.telemetry: invalid clusterName", name)
			}
			clusters[e.Telemetry.ClusterName] = true
			if _, err := e.Telemetry.maxAge(); err != nil {
				return bad("environments.%s.telemetry: invalid maxAge", name)
			}
		}
		if e.Prometheus != nil {
			for key, value := range e.Prometheus.Labels {
				if (key != "environment" && key != "cluster") || len(value) == 0 || len(value) > 200 || strings.ContainsAny(value, "\r\n\x00") {
					return bad("environments.%s.prometheus: invalid binding labels", name)
				}
			}
		}
		if a := e.AuditCollector; a != nil {
			if !id.MatchString(a.Namespace) || a.Deployment == "" || len(validation.IsDNS1123Subdomain(a.Deployment)) != 0 {
				return bad("environments.%s.auditCollector: invalid namespace or deployment", name)
			}
		}
		if p := e.Prometheus; p != nil {
			u, err := url.Parse(p.URL)
			if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "http" && u.Scheme != "https") {
				return bad("environments.%s.prometheus.url must be an http(s) URL without credentials or query", name)
			}
			if _, err := p.maxAge(); err != nil {
				return bad("environments.%s.prometheus.maxAge must be a duration from 1s to 10m", name)
			}
		}
	}
	if len(c.Apps) == 0 {
		return bad("apps must not be empty")
	}
	names, namespaces := map[string]bool{}, map[string]bool{}
	for i, a := range c.Apps {
		if o := a.Observability; o != nil {
			if len(o.RequiredSignals) == 0 || len(o.RequiredSignals) > 3 {
				return bad("apps[%d].observability: one to three required signals expected", i)
			}
			seen := map[string]bool{}
			for _, signal := range o.RequiredSignals {
				if (signal != "logs" && signal != "traces" && signal != "metrics") || seen[signal] {
					return bad("apps[%d].observability: invalid or duplicate signal", i)
				}
				seen[signal] = true
			}
			if len(o.ServiceNames) > 16 || (seen["logs"] || seen["traces"]) && len(o.ServiceNames) == 0 {
				return bad("apps[%d].observability: exact service names required", i)
			}
			for _, service := range o.ServiceNames {
				if !id.MatchString(service) {
					return bad("apps[%d].observability: invalid service name", i)
				}
			}
		}
		for env, baseline := range a.Baselines {
			if !slices.Contains(envs, env) || !id.MatchString(baseline.Revision) || baseline.ReviewedAt.IsZero() || !baseline.ExpiresAt.After(baseline.ReviewedAt) || baseline.ExpiresAt.Sub(baseline.ReviewedAt) > 30*24*time.Hour {
				return bad("apps[%d].baselines: registered environment, revision and review interval up to 30 days required", i)
			}
		}
		if !id.MatchString(a.Name) || !id.MatchString(a.Namespace) || names[a.Name] || namespaces[a.Namespace] {
			return bad("apps[%d]: invalid or duplicate name or namespace", i)
		}
		names[a.Name], namespaces[a.Namespace] = true, true
		configured := len(a.RBAC) > 0 || a.PrometheusJob != "" || a.Observability != nil
		for env, resources := range a.Environments {
			if !slices.Contains(envs, env) {
				return bad("apps[%d].environments: unregistered environment", i)
			}
			seen := map[string]bool{}
			for _, s := range resources.Secrets {
				if !id.MatchString(s.ID) || s.ID == "secret-usability" || seen[s.ID] || s.Name == "" || len(validation.IsDNS1123Subdomain(s.Name)) != 0 {
					return bad("apps[%d].environments: invalid or duplicate secret reference", i)
				}
				seen[s.ID] = true
				configured = true
			}
			if b := resources.Backup; b != nil {
				if _, err := b.maxAge(); err != nil || b.Cluster == "" || len(validation.IsDNS1123Subdomain(b.Cluster)) != 0 {
					return bad("apps[%d].environments: backup needs cluster and maxAge from 1m to 720h", i)
				}
				configured = true
			}
			if u := resources.SecretUsage; u != nil {
				if err := u.Validate(resources.Secrets); err != nil {
					return bad("apps[%d].environments: invalid secretUsage binding", i)
				}
			}
			if cloud := resources.CloudIAM; cloud != nil {
				if err := cloud.Validate(); err != nil {
					return bad("apps[%d].environments: invalid cloudIAM policy", i)
				}
				configured = true
			}
			if e := c.Environments[env]; e.AuditCollector != nil {
				configured = true
			}
		}
		if !configured {
			return bad("apps[%d]: needs rbac, prometheusJob or environment resource checks", i)
		}
		if len(a.RBAC) > 0 {
			if !id.MatchString(a.ServiceAccount) {
				return bad("apps[%d].serviceAccount is required with rbac", i)
			}
			seen, allowed, denied := map[string]bool{}, false, false
			for j, r := range a.RBAC {
				if r.Allowed == nil || !id.MatchString(r.ID) || seen[r.ID] || r.Verb == "" || r.Verb == "*" || r.Resource == "" || r.Resource == "*" ||
					(r.Namespace != "" && !id.MatchString(r.Namespace)) {
					return bad("apps[%d].rbac[%d]: missing allowed, invalid or duplicate id, or a wildcard verb or resource", i, j)
				}
				seen[r.ID] = true
				allowed, denied = allowed || *r.Allowed, denied || !*r.Allowed
			}
			if !allowed || !denied {
				return bad("apps[%d].rbac needs at least one allowed and one denied rule", i)
			}
		}
		if j := a.PrometheusJob; j != "" && (len(j) > 200 || strings.ContainsAny(j, "\r\n\x00")) {
			return bad("apps[%d].prometheusJob is invalid", i)
		}
	}
	return nil
}

func (p Prometheus) maxAge() (time.Duration, error) {
	if p.MaxAge == "" {
		return 2 * time.Minute, nil
	}
	d, err := time.ParseDuration(p.MaxAge)
	if err != nil || d < time.Second || d > 10*time.Minute {
		return 0, ErrInvalidConfig
	}
	return d, nil
}

func (c Config) appFor(namespace string) (App, bool) {
	for _, a := range c.Apps {
		if a.Namespace == namespace {
			return a, true
		}
	}
	return App{}, false
}
