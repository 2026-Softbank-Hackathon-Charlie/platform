package apps

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"slices"
	"strings"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/plans"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/secrets"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
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
	// BucketPrefixes maps an environment to its storage driver's bucket
	// prefix when the registered apps' files are moved there (Files,
	// PLATFORM_APP_FILES, ADR 0032). nil: their files are not moved.
	BucketPrefixes map[string]string
}

// RegistryStore is the part of the store a Registry reads.
type RegistryStore interface {
	GetApp(ctx context.Context, name string) (store.App, error)
	ListApps(ctx context.Context) ([]store.App, error)
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

// endpoints are the pair's databases in the environments of appEnvs, the
// app's, that have a host template: a peer the app was never added to, or
// was taken out of again (ADR 0043), has no database. An environment whose
// host or variable would not be allowed is left out.
func (r *Registry) endpoints(d store.AppDatabase, appEnvs []string) []endpoint {
	var out []endpoint
	for _, env := range appEnvs {
		tmpl, ok := r.HostTemplates[env]
		if !ok || !slices.Contains(appEnvs, env) {
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

// app returns the registered app name as a catalog entry: with its pair
// when it has a postgres resource, or without a pair (nil) when it has
// none and is registered in two or more environments (ADR 0049). false for
// an app with a postgres resource and no pair, or without one in fewer
// environments.
func (r *Registry) app(ctx context.Context, name string) (App, *store.AppDatabase, bool, error) {
	a, err := r.Store.GetApp(ctx, name)
	if errors.Is(err, store.ErrNotFound) {
		return App{}, nil, false, nil
	}
	if err != nil {
		return App{}, nil, false, err
	}
	res, spec, err := specResources(a)
	if err != nil {
		// Unknown resources: neither a database app nor one without (the
		// API shows it as registered only and logs it).
		return App{}, nil, false, nil
	}
	d, err := r.Store.GetAppDatabase(ctx, name)
	paired := err == nil
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return App{}, nil, false, err
	}
	if !res.Postgres {
		// A pair without a postgres resource is a spec that changed after
		// registration: neither path fits, so drafts and the policy
		// (statelessApps) both leave it out.
		if paired {
			return App{}, nil, false, nil
		}
		out, ok := r.statelessApp(a, res, spec)
		return out, nil, ok, nil
	}
	if !paired {
		return App{}, nil, false, nil
	}
	i := slices.IndexFunc(spec.Resources, func(r appspec.Resource) bool { return r.Type == appspec.TypePostgres })
	// The chart's names (ADR 0030): namespace the instance, Cluster
	// <app>-<resource>, database app owned by app.
	// An s3 resource is the instance's bucket, <bucketPrefix><instance>
	// (ADR 0016), named through the environment's remote (ADR 0032).
	out := App{Name: name, Namespace: name, Database: Database{Name: cmp.Or(d.Database, "app"), User: cmp.Or(d.Owner, "app")}, Environments: map[string]Environment{}, resources: &res}
	out.filesRequired = res.S3 && len(r.BucketPrefixes) > 0
	for _, e := range r.endpoints(d, a.Environments) {
		env := Environment{CnpgCluster: name + "-" + spec.Resources[i].Name, DBHost: e.host, DBPort: 5432, PasswordEnv: e.passwordEnv}
		if res.S3 {
			env.Bucket = r.fileBucket(e.env, name)
		}
		out.Environments[e.env] = env
	}
	return out, &d, true, nil
}

// statelessApp is a registered app without a postgres resource as a
// catalog entry (ADR 0049): its environments are the app's own, without a
// database, each with the instance's bucket when it has an s3 resource and
// the registry moves files there. false in fewer than two environments,
// where there is nothing to move it to, and without a public service,
// which is the only thing of the app the gateway routes.
func (r *Registry) statelessApp(a store.App, res apiv1.AppResources, spec appspec.Spec) (App, bool) {
	if len(a.Environments) < 2 || !slices.ContainsFunc(spec.Services, func(s appspec.Service) bool { return s.Public }) {
		return App{}, false
	}
	out := App{Name: a.Name, Namespace: a.Name, Environments: map[string]Environment{}, resources: &res}
	for _, env := range a.Environments {
		var e Environment
		if res.S3 {
			e.Bucket = r.fileBucket(env, a.Name)
		}
		out.Environments[env] = e
	}
	return out, true
}

// specResources reads which resources the stored spec of a's default
// instance declares.
func specResources(a store.App) (apiv1.AppResources, appspec.Spec, error) {
	var spec appspec.Spec
	if err := json.Unmarshal(a.Spec, &spec); err != nil {
		return apiv1.AppResources{}, appspec.Spec{}, fmt.Errorf("the stored spec of %s: %w", a.Name, err)
	}
	var res apiv1.AppResources
	for _, x := range spec.Resources {
		res.Postgres = res.Postgres || x.Type == appspec.TypePostgres
		res.S3 = res.S3 || x.Type == appspec.TypeS3
	}
	return res, spec, nil
}

// SpecResources is what the stored spec of a registered app's default
// instance declares, for an app the catalog has no entry for.
func SpecResources(a store.App) (apiv1.AppResources, error) {
	res, _, err := specResources(a)
	return res, err
}

// pairedApp is a pair the catalog takes from the registry with its app's
// environments.
type pairedApp struct {
	store.AppDatabase
	envs []string
}

// endpoints are the pair's databases (Registry.endpoints).
func (p pairedApp) endpoints(r *Registry) []endpoint { return r.endpoints(p.AppDatabase, p.envs) }

// SourceRewireAllowed binds the opt-in to a registered three-member group,
// including the environment's actual host, context, namespace and Cluster.
// Static apps and plans that merely reuse an app's host cannot enable it.
func (c *Catalog) SourceRewireAllowed(ctx context.Context, p *plan.Plan) (bool, error) {
	if c.registry == nil || p == nil {
		return false, nil
	}
	pairs, err := c.pairs(ctx)
	if err != nil {
		return false, err
	}
	for _, pair := range pairs {
		if len(pair.envs) != 3 {
			continue
		}
		a, _, found, err := c.registry.app(ctx, pair.App)
		if err != nil {
			return false, err
		}
		if !found || p.Source.Env == p.Target.Env {
			continue
		}
		matches := func(ep plan.Endpoint) bool {
			e, ok := a.Environments[ep.Env]
			env, configured := c.env(ep.Env)
			if !ok || !configured || ep.Context != env.KubeContext || ep.Namespace != a.Namespace || ep.Cluster != e.CnpgCluster {
				return false
			}
			cfg, err := pgconn.ParseConfig(ep.DSN)
			if err != nil || cfg.Host != e.DBHost || cfg.Port != uint16(5432) || cfg.Database != a.Database.Name || cfg.User != a.Database.User {
				return false
			}
			return len(cfg.Fallbacks) == 0
		}
		if matches(p.Source) && matches(p.Target) {
			return true, nil
		}
	}
	return false, nil
}

// pairs lists the pairs the catalog takes from the registry: not of an app
// of the file, and of an app that is registered. Pairs are listed before
// apps, so an app registered in between is left out until the next read.
func (c *Catalog) pairs(ctx context.Context) ([]pairedApp, error) {
	all, err := c.registry.Store.ListAppDatabases(ctx)
	if err != nil {
		return nil, err
	}
	apps, err := c.registry.Store.ListApps(ctx)
	if err != nil {
		return nil, err
	}
	envs := map[string][]string{}
	for _, a := range apps {
		envs[a.Name] = a.Environments
	}
	var out []pairedApp
	for _, d := range all {
		if e, ok := envs[d.App]; ok && !c.Has(d.App) {
			out = append(out, pairedApp{AppDatabase: d, envs: e})
		}
	}
	return out, nil
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
		for _, e := range d.endpoints(c.registry) {
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

// RegistryBuckets returns, read now, the buckets files of the registered
// apps' plans may move between (ADR 0032): <env>:<bucketPrefix><app> in
// each environment of every pair that the registry moves files in, for
// the plan policy (plans.AppDatabases). The bucket of an app without an s3
// resource does not exist, so it is not told apart: rclone fails on it.
// Empty without a registry or with PLATFORM_APP_FILES off.
func (c *Catalog) RegistryBuckets(ctx context.Context) ([]string, error) {
	out := []string{}
	if c.registry == nil || len(c.registry.BucketPrefixes) == 0 {
		return out, nil
	}
	pairs, err := c.pairs(ctx)
	if err != nil {
		return nil, fmt.Errorf("read the database pairs: %w", err)
	}
	for _, d := range pairs {
		for _, e := range d.endpoints(c.registry) {
			if b := c.registry.fileBucket(e.env, d.App); b != "" {
				out = append(out, b)
			}
		}
	}
	stateless, err := c.statelessApps(ctx)
	if err != nil {
		return nil, err
	}
	for _, a := range stateless {
		for _, e := range a.Environments {
			if e.Bucket != "" {
				out = append(out, e.Bucket)
			}
		}
	}
	return out, nil
}

// statelessApps lists, read now, the registered apps without a postgres
// resource that plans can be drafted for (Registry.statelessApp, ADR 0049),
// not of the apps file. An app whose stored spec does not decode is left
// out: its plans are refused rather than every plan.
func (c *Catalog) statelessApps(ctx context.Context) ([]App, error) {
	pairs, err := c.registry.Store.ListAppDatabases(ctx)
	if err != nil {
		return nil, fmt.Errorf("read the database pairs: %w", err)
	}
	all, err := c.registry.Store.ListApps(ctx)
	if err != nil {
		return nil, fmt.Errorf("read the registered apps: %w", err)
	}
	var out []App
	for _, a := range all {
		if c.Has(a.Name) || slices.ContainsFunc(pairs, func(d store.AppDatabase) bool { return d.App == a.Name }) {
			continue
		}
		res, spec, err := specResources(a)
		if err != nil || res.Postgres {
			continue
		}
		if s, ok := c.registry.statelessApp(a, res, spec); ok {
			out = append(out, s)
		}
	}
	return out, nil
}

// RegistryStateless returns, read now, every registered app without a
// database that plans can be drafted for, with the gateway backend and
// bucket of each environment it is registered in, for the plan policy
// (plans.AppDatabases): a plan without a database must use the gateway
// scope of such an app, between two of its environments (ADR 0049). Empty without a registry.
func (c *Catalog) RegistryStateless(ctx context.Context) (map[string]map[string]plans.StatelessEnv, error) {
	out := map[string]map[string]plans.StatelessEnv{}
	if c.registry == nil {
		return out, nil
	}
	apps, err := c.statelessApps(ctx)
	if err != nil {
		return nil, err
	}
	for _, a := range apps {
		envs := map[string]plans.StatelessEnv{}
		for name, e := range a.Environments {
			if env, ok := c.env(name); ok { // an environment no longer in the file is left out
				envs[name] = plans.StatelessEnv{Backend: env.GatewayBackend, Bucket: e.Bucket}
			}
		}
		out[a.Name] = envs
	}
	return out, nil
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
		for _, e := range d.endpoints(c.registry) {
			if _, taken := static[e.passwordEnv]; taken || !slices.Contains(refs, e.passwordEnv) {
				continue
			}
			if err := c.setPassword(d.AppDatabase, e.passwordEnv); err != nil {
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
