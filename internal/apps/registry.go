package apps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"slices"
	"strings"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/plans"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/secrets"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/appspec"
)

// Registry adds the apps registered from GitHub with a database pair (ADR
// 0030) to a Catalog, read from the store on every use, so a new app's
// plans can be drafted and pass the policy without a restart. An app of
// the apps file with the same name wins, and so does a password variable
// of the file.
type Registry struct {
	Store RegistryStore
	// Secrets decrypts the pairs' passwords.
	Secrets *secrets.Cipher
	// HostTemplates maps an environment to the host of an instance's
	// database there as the API reaches it, with {instance} for the
	// instance (PLATFORM_APP_DB_HOST_TEMPLATES, ParseHostTemplates).
	HostTemplates map[string]string
}

// RegistryStore is the part of the store a Registry reads.
type RegistryStore interface {
	GetApp(ctx context.Context, name string) (store.App, error)
	GetAppDatabase(ctx context.Context, app string) (store.AppDatabase, error)
	ListAppDatabases(ctx context.Context) ([]store.AppDatabase, error)
}

// ParseHostTemplates reads PLATFORM_APP_DB_HOST_TEMPLATES: comma-separated
// <env>=<host template> entries, one per environment of environments, whose
// only placeholder is {instance}, e.g.
// eks=db-eks-{instance}.hopcharlie-db-network.svc.cluster.local.
func ParseHostTemplates(v string, environments []string) (map[string]string, error) {
	out := map[string]string{}
	for _, e := range strings.Split(v, ",") {
		name, tmpl, ok := strings.Cut(strings.TrimSpace(e), "=")
		switch {
		case !ok:
			return nil, fmt.Errorf("%q is not <env>=<host template>", e)
		case !slices.Contains(environments, name):
			return nil, fmt.Errorf("environment %q is not in the environments file", name)
		case out[name] != "":
			return nil, fmt.Errorf("environment %s has two templates", name)
		}
		filled := strings.ReplaceAll(tmpl, "{instance}", "app--branch")
		if !strings.Contains(tmpl, "{instance}") || !hostName.MatchString(filled) {
			return nil, fmt.Errorf("%s: %q must be a host name with {instance} and no other placeholder", name, tmpl)
		}
		out[name] = tmpl
	}
	return out, nil
}

// RegistryPasswordEnv is the variable a registered app's database password
// is read from in env: <ENV>_<APP>_PGPASSWORD, upper case, "-" as "_".
func RegistryPasswordEnv(env, app string) string {
	return strings.ToUpper(strings.ReplaceAll(env+"_"+app, "-", "_")) + "_PGPASSWORD"
}

// endpoint is a database of a pair in one environment; host has no port
// (5432).
type endpoint struct {
	env, host, passwordEnv string
}

func (e endpoint) hostPort() string { return net.JoinHostPort(e.host, "5432") }

// endpoints are the pair's databases in the environments that have a host
// template; an environment whose host or variable would not be allowed is
// left out.
func (r *Registry) endpoints(d store.AppDatabase) []endpoint {
	var out []endpoint
	for _, env := range []string{d.PrimaryEnv, d.PeerEnv} {
		tmpl, ok := r.HostTemplates[env]
		if !ok {
			continue
		}
		host := strings.ReplaceAll(tmpl, "{instance}", d.App) // the default instance
		pw := RegistryPasswordEnv(env, d.App)
		if !hostName.MatchString(host) || plans.CheckPasswordEnv(pw) != nil {
			continue
		}
		out = append(out, endpoint{env: env, host: host, passwordEnv: pw})
	}
	return out
}

// app returns the registered app name as a catalog entry with its pair, or
// false when it has no pair or no postgres resource.
func (r *Registry) app(ctx context.Context, name string) (App, store.AppDatabase, bool, error) {
	d, err := r.Store.GetAppDatabase(ctx, name)
	if errors.Is(err, store.ErrNotFound) {
		return App{}, store.AppDatabase{}, false, nil
	}
	if err != nil {
		return App{}, store.AppDatabase{}, false, err
	}
	a, err := r.Store.GetApp(ctx, name)
	if errors.Is(err, store.ErrNotFound) {
		return App{}, store.AppDatabase{}, false, nil
	}
	if err != nil {
		return App{}, store.AppDatabase{}, false, err
	}
	var spec appspec.Spec
	if err := json.Unmarshal(a.Spec, &spec); err != nil {
		return App{}, store.AppDatabase{}, false, fmt.Errorf("the stored spec of %s: %w", name, err)
	}
	i := slices.IndexFunc(spec.Resources, func(r appspec.Resource) bool { return r.Type == appspec.TypePostgres })
	if i < 0 {
		return App{}, store.AppDatabase{}, false, nil
	}
	// The chart's names (ADR 0030): namespace the instance, Cluster
	// <app>-<resource>, database app owned by app.
	out := App{Name: name, Namespace: name, Database: Database{Name: "app", User: "app"}, Environments: map[string]Environment{}}
	for _, e := range r.endpoints(d) {
		out.Environments[e.env] = Environment{CnpgCluster: name + "-" + spec.Resources[i].Name, DBHost: e.host, DBPort: 5432, PasswordEnv: e.passwordEnv}
	}
	return out, d, true, nil
}

// pairs lists the pairs the catalog takes from the registry: not of an app
// of the file.
func (c *Catalog) pairs(ctx context.Context) ([]store.AppDatabase, error) {
	all, err := c.registry.Store.ListAppDatabases(ctx)
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(all, func(d store.AppDatabase) bool { return c.Has(d.App) }), nil
}

// RegistryHosts returns, read now, the password variables of the
// registered apps' databases with the one host each may be sent to, and
// each such app's hosts, for the plan policy (plans.AppDatabases). A
// variable or app of the apps file is left out: the file's entries win.
// Empty without a registry.
func (c *Catalog) RegistryHosts(ctx context.Context) (passwordHosts, appHosts map[string][]string, err error) {
	passwordHosts, appHosts = map[string][]string{}, map[string][]string{}
	if c.registry == nil {
		return passwordHosts, appHosts, nil
	}
	pairs, err := c.pairs(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("read the database pairs: %w", err)
	}
	static := c.PasswordHosts()
	for _, d := range pairs {
		var hosts []string
		for _, e := range c.registry.endpoints(d) {
			hosts = append(hosts, e.hostPort())
			if _, taken := static[e.passwordEnv]; !taken {
				passwordHosts[e.passwordEnv] = []string{e.hostPort()}
			}
		}
		if len(hosts) > 0 {
			slices.Sort(hosts)
			appHosts[d.App] = hosts
		}
	}
	return passwordHosts, appHosts, nil
}

// SetRegistryPasswords puts the password of every registered app's
// database that refs names into the server's environment, decrypted from
// its pair now (ADR 0030): the engine expands a plan's ${VAR} only from
// there. Other names are left alone, as are the apps file's variables, which
// the server's environment already holds. Without a registry it does
// nothing.
func (c *Catalog) SetRegistryPasswords(ctx context.Context, refs []string) error {
	if c.registry == nil || len(refs) == 0 {
		return nil
	}
	pairs, err := c.pairs(ctx)
	if err != nil {
		return fmt.Errorf("read the database pairs: %w", err)
	}
	static := c.PasswordHosts()
	for _, d := range pairs {
		for _, e := range c.registry.endpoints(d) {
			if _, taken := static[e.passwordEnv]; taken || !slices.Contains(refs, e.passwordEnv) {
				continue
			}
			if err := c.setPassword(d, e.passwordEnv); err != nil {
				return err
			}
		}
	}
	return nil
}

// setPassword sets name to the password of pair d.
func (c *Catalog) setPassword(d store.AppDatabase, name string) error {
	if c.registry.Secrets == nil {
		return fmt.Errorf("the database password of %s is stored encrypted, but this server has no PLATFORM_SECRETS_KEY", d.App)
	}
	pw, _, err := c.registry.Secrets.OpenDatabase(d)
	if err != nil {
		return err
	}
	return os.Setenv(name, pw)
}
