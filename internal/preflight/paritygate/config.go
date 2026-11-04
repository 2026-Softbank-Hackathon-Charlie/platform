// Package paritygate runs the parity probes (identity via Kubernetes RBAC,
// metrics via Prometheus) against a migration's target before it starts and
// returns the report for the audit log. It only records: nothing it finds
// stops a run. The configuration is server-owned (PLATFORM_PARITY_FILE),
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

	"sigs.k8s.io/yaml"
)

var ErrInvalidConfig = errors.New("invalid parity configuration")

// ModeAudit records the report and never blocks. It is the only mode: the
// secret, audit and backup-restore probes do not exist yet, so a blocking
// mode would deny every run.
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
	Prometheus *Prometheus `json:"prometheus,omitempty"`
}

// Prometheus is a Prometheus HTTP API the platform can reach without
// credentials (for example the in-cluster service of kube-prometheus-stack).
type Prometheus struct {
	URL string `json:"url"`
	// MaxAge is the oldest acceptable last scrape, e.g. "2m" (1s to 10m).
	MaxAge string `json:"maxAge,omitempty"`
}

// App is what parity expects of one app in every environment.
type App struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	// ServiceAccount is the account the app's pods run as; the RBAC rules
	// are evaluated for it with SubjectAccessReview.
	ServiceAccount string `json:"serviceAccount,omitempty"`
	// RBAC needs at least one allowed and one denied rule.
	RBAC []Rule `json:"rbac,omitempty"`
	// PrometheusJob is the scrape job of the app's targets.
	PrometheusJob string `json:"prometheusJob,omitempty"`
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
		return bad("mode must be %q (parity only records; it never blocks a run)", ModeAudit)
	}
	for name, e := range c.Environments {
		if !slices.Contains(envs, name) {
			return bad("environments.%s is not a registered environment", name)
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
		if !id.MatchString(a.Name) || !id.MatchString(a.Namespace) || names[a.Name] || namespaces[a.Namespace] {
			return bad("apps[%d]: invalid or duplicate name or namespace", i)
		}
		names[a.Name], namespaces[a.Namespace] = true, true
		if len(a.RBAC) == 0 && a.PrometheusJob == "" {
			return bad("apps[%d]: needs rbac or prometheusJob", i)
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
