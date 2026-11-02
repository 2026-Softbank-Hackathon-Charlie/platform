// Package apps is the registry of the applications HopCharlie migrates
// (PLATFORM_APPS_FILE): for each app, its namespace and database, and per
// environment its CNPG cluster, database host, password variable and file
// bucket. From it the platform drafts engine plans, so a route between two
// environments never needs hand-written plan JSON.
package apps

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"

	"sigs.k8s.io/yaml"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/envs"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

// Config is the apps file.
type Config struct {
	Apps []App `json:"apps"`
}

// App is one application.
type App struct {
	// Name becomes part of plan ids, e.g. "moimroom".
	Name string `json:"name"`
	// Namespace holds the app's CNPG Cluster in every environment.
	Namespace string   `json:"namespace"`
	Database  Database `json:"database"`
	// Environments is keyed by environment name from PLATFORM_ENVS_FILE.
	Environments map[string]Environment `json:"environments"`
}

// Database is what the DSNs connect to and what verification compares.
type Database struct {
	Name string `json:"name"`
	User string `json:"user"`
	// Schemas are compared by verification; empty uses the engine default (public).
	Schemas []string `json:"schemas,omitempty"`
}

// Environment is the app in one environment.
type Environment struct {
	CnpgCluster string `json:"cnpgCluster"`
	// DBHost is the host of the cluster's -rw service (over Tailscale).
	DBHost string `json:"dbHost"`
	// DBPort defaults to 5432.
	DBPort int `json:"dbPort,omitempty"`
	// PasswordEnv names the server environment variable holding the database
	// password. Plans refer to it as ${VAR}; it is added to the plan policy's
	// allowlist.
	PasswordEnv string `json:"passwordEnv"`
	// Bucket is the rclone remote and bucket of the app's files, e.g. "gcs:moimroom-photos-gke".
	Bucket string `json:"bucket,omitempty"`
}

var (
	appName  = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`)
	dnsLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)
	dbName   = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,62}$`)
	hostName = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]{0,251}[A-Za-z0-9])?$`)
	envVar   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// LoadConfig reads an apps file (YAML) strictly.
func LoadConfig(path string) (Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read apps file: %w", err)
	}
	var cfg Config
	if err := yaml.UnmarshalStrict(raw, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse apps file %s: %w", path, err)
	}
	return cfg, nil
}

// Catalog is the validated apps file together with the environments it
// refers to. It is read-only and safe for concurrent use.
type Catalog struct {
	apps       []App
	envs       []envs.EnvConfig // PLATFORM_ENVS_FILE order
	gatewayURL string
}

// New validates cfg against the registered environments (normalized, as
// envs.LoadConfig returns them). gatewayURL is the platform gateway's admin
// URL, or "" when the server has none.
func New(cfg Config, environments []envs.EnvConfig, gatewayURL string) (*Catalog, error) {
	if len(cfg.Apps) == 0 {
		return nil, errors.New("no apps configured")
	}
	known := map[string]bool{}
	for _, e := range environments {
		known[e.Name] = true
	}
	seen := map[string]bool{}
	out := make([]App, len(cfg.Apps))
	for i, a := range cfg.Apps {
		switch {
		case !appName.MatchString(a.Name):
			return nil, fmt.Errorf("apps[%d]: name %q must match %s", i, a.Name, appName)
		case seen[a.Name]:
			return nil, fmt.Errorf("apps[%d]: duplicate name %q", i, a.Name)
		case !dnsLabel.MatchString(a.Namespace):
			return nil, fmt.Errorf("app %q: namespace %q is not a Kubernetes namespace name", a.Name, a.Namespace)
		case !dbName.MatchString(a.Database.Name) || !dbName.MatchString(a.Database.User):
			return nil, fmt.Errorf("app %q: database name and user must match %s", a.Name, dbName)
		case len(a.Environments) == 0:
			return nil, fmt.Errorf("app %q: no environments", a.Name)
		}
		seen[a.Name] = true
		for _, s := range a.Database.Schemas {
			if !dbName.MatchString(s) {
				return nil, fmt.Errorf("app %q: schema %q must match %s", a.Name, s, dbName)
			}
		}
		envCopy := map[string]Environment{}
		for name, e := range a.Environments {
			switch {
			case !known[name]:
				return nil, fmt.Errorf("app %q: environment %q is not in the environments file", a.Name, name)
			case !dnsLabel.MatchString(e.CnpgCluster):
				return nil, fmt.Errorf("app %q in %s: cnpgCluster %q is not a Kubernetes resource name", a.Name, name, e.CnpgCluster)
			case !hostName.MatchString(e.DBHost):
				return nil, fmt.Errorf("app %q in %s: dbHost %q is not a host name", a.Name, name, e.DBHost)
			case e.DBPort < 0 || e.DBPort > 65535:
				return nil, fmt.Errorf("app %q in %s: dbPort %d is out of range", a.Name, name, e.DBPort)
			case !envVar.MatchString(e.PasswordEnv):
				return nil, fmt.Errorf("app %q in %s: passwordEnv %q is not an environment variable name", a.Name, name, e.PasswordEnv)
			}
			if e.DBPort == 0 {
				e.DBPort = 5432
			}
			envCopy[name] = e
		}
		a.Database.Schemas = slices.Clone(a.Database.Schemas)
		a.Environments = envCopy
		out[i] = a
	}
	return &Catalog{apps: out, envs: slices.Clone(environments), gatewayURL: gatewayURL}, nil
}

// PasswordEnvs lists every passwordEnv once, for the plan policy's allowlist.
func (c *Catalog) PasswordEnvs() []string {
	var out []string
	for _, a := range c.apps {
		for _, e := range a.Environments {
			if !slices.Contains(out, e.PasswordEnv) {
				out = append(out, e.PasswordEnv)
			}
		}
	}
	slices.Sort(out)
	return out
}

// List returns the apps in file order, each with its environments in
// environments-file order. Passwords and their variable names are not included.
func (c *Catalog) List() []apiv1.App {
	out := make([]apiv1.App, 0, len(c.apps))
	for _, a := range c.apps {
		item := apiv1.App{
			Name:         a.Name,
			Namespace:    a.Namespace,
			Database:     apiv1.AppDatabase{Name: a.Database.Name, User: a.Database.User, Schemas: slices.Clone(a.Database.Schemas)},
			Environments: []apiv1.AppEnvironment{},
		}
		if item.Database.Schemas == nil {
			item.Database.Schemas = []string{}
		}
		for _, env := range c.envs {
			e, ok := a.Environments[env.Name]
			if !ok {
				continue
			}
			ae := apiv1.AppEnvironment{Env: env.Name, CnpgCluster: e.CnpgCluster, DbHost: hostPort(e)}
			if e.Bucket != "" {
				b := e.Bucket
				ae.Bucket = &b
			}
			item.Environments = append(item.Environments, ae)
		}
		out = append(out, item)
	}
	return out
}

func (c *Catalog) app(name string) (App, bool) {
	for _, a := range c.apps {
		if a.Name == name {
			return a, true
		}
	}
	return App{}, false
}

func (c *Catalog) env(name string) (envs.EnvConfig, bool) {
	for _, e := range c.envs {
		if e.Name == name {
			return e, true
		}
	}
	return envs.EnvConfig{}, false
}
