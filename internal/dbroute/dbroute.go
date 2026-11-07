// Package dbroute keeps the management cluster's routes to the databases of
// registered apps (ADR 0030, "관리 쪽 DB 경로"): for each app with a database
// pair it creates, in the cluster the API runs in, one Tailscale egress
// Service per environment of the pair while both are the app's environments
// (ADR 0037), named after the environment's database host template, and
// deletes them when the app or one of the environments goes. The Tailscale operator
// turns each Service into a proxy to the app's database in that environment.
//
// It only creates and deletes Services, never updates them: the operator
// owns spec.externalName and the finalizer. A Service of the same name
// without our labels is never touched; it is reported as a conflict.
package dbroute

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/apps"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// Labels every Service it creates carries. Only Services with all three, of
// the name the route expects, are ever deleted.
const (
	LabelManagedBy = "app.kubernetes.io/managed-by"
	ManagedBy      = "hopcharlie-platform"
	LabelApp       = "hopcharlie.dev/app"
	LabelRoute     = "hopcharlie.dev/db-route" // the environment
)

// The Tailscale operator annotations of an egress Service.
const (
	AnnotationTailnetFQDN = "tailscale.com/tailnet-fqdn"
	AnnotationHostname    = "tailscale.com/hostname"
	AnnotationTags        = "tailscale.com/tags"
	AnnotationProxyClass  = "tailscale.com/proxy-class"
)

// Placeholder is the externalName a new Service is created with; the
// operator replaces it with its proxy's address.
const Placeholder = "placeholder"

// Interval is how often Run reconciles.
const Interval = 2 * time.Minute

// ErrConflict is a Service of a route's name that is not ours.
var ErrConflict = errors.New("a service of the same name is not managed by the platform")

// Pairs reads the database pairs of registered apps and the apps
// themselves, for their environments (store.Store).
type Pairs interface {
	GetAppDatabase(ctx context.Context, app string) (store.AppDatabase, error)
	ListAppDatabases(ctx context.Context) ([]store.AppDatabase, error)
	GetApp(ctx context.Context, name string) (store.App, error)
	ListApps(ctx context.Context) ([]store.App, error)
}

// envs are the environments of pair d that get a route: those with a route,
// and only while both environments of the pair are environments of app a.
// The routes are for migrating between the two, so a pair whose peer was
// never added, or was removed again, has none, and no egress proxy stands
// for it (ADR 0037).
func (m *Manager) envs(d store.AppDatabase, a store.App) []string {
	if !slices.Contains(a.Environments, d.PrimaryEnv) || !slices.Contains(a.Environments, d.PeerEnv) {
		return nil
	}
	var out []string
	for _, env := range []string{d.PrimaryEnv, d.PeerEnv} {
		if _, ok := m.cfg.Routes[env]; ok {
			out = append(out, env)
		}
	}
	return out
}

// Route is where one environment's databases are reached from the
// management cluster: Service <Prefix><instance><Suffix> in Namespace.
type Route struct {
	Namespace      string
	Prefix, Suffix string
}

// Name is the Service name of the route for instance.
func (r Route) Name(instance string) string { return r.Prefix + instance + r.Suffix }

// ParseRoutes reads the routes from the database host templates
// (PLATFORM_APP_DB_HOST_TEMPLATES, apps.ParseHostTemplates). Each must be
// <service>.<namespace>.svc.cluster.local with {instance} in <service> only.
func ParseRoutes(tmpls map[string]string) (map[string]Route, error) {
	out := map[string]Route{}
	for _, env := range slices.Sorted(maps.Keys(tmpls)) {
		t := tmpls[env]
		host, ok := strings.CutSuffix(t, ".svc.cluster.local")
		svc, ns, ok2 := strings.Cut(host, ".")
		prefix, suffix, ok3 := strings.Cut(svc, "{instance}")
		if !ok || !ok2 || !ok3 || strings.Contains(ns, ".") || strings.Contains(suffix, "{instance}") ||
			len(validation.IsDNS1123Label(ns)) > 0 || len(validation.IsDNS1035Label(prefix+"a"+suffix)) > 0 {
			return nil, fmt.Errorf("%s: %q is not <service with {instance}>.<namespace>.svc.cluster.local", env, t)
		}
		out[env] = Route{Namespace: ns, Prefix: prefix, Suffix: suffix}
	}
	return out, nil
}

// Config configures a Manager.
type Config struct {
	Client kubernetes.Interface // the management cluster
	Pairs  Pairs
	Routes map[string]Route // by environment
	// Tailnet is the tailnet's DNS name (tail567fe8.ts.net): the database of
	// instance in env is hop-db-<env>-<instance>.<Tailnet>, as the app chart
	// names its ingress.
	Tailnet    string
	ProxyClass string // the operator ProxyClass of the proxies
	Tag        string // the proxies' tailnet tag
	Log        *slog.Logger
}

// Manager creates and deletes the routes.
type Manager struct{ cfg Config }

// New returns a Manager.
func New(cfg Config) *Manager {
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	return &Manager{cfg: cfg}
}

// FromEnv builds a Manager from PLATFORM_APP_DB_ROUTE_TAILNET,
// PLATFORM_APP_DB_ROUTE_PROXY_CLASS and PLATFORM_APP_DB_ROUTE_TAG with the
// in-cluster client. With none of them set it returns nil: the feature is off.
// It needs all three and PLATFORM_APP_DB_HOST_TEMPLATES.
func FromEnv(getenv func(string) string, environments []string, pairs Pairs, log *slog.Logger) (*Manager, error) {
	tailnet, class, tag := getenv("PLATFORM_APP_DB_ROUTE_TAILNET"), getenv("PLATFORM_APP_DB_ROUTE_PROXY_CLASS"), getenv("PLATFORM_APP_DB_ROUTE_TAG")
	if tailnet == "" && class == "" && tag == "" {
		return nil, nil
	}
	if tailnet == "" || class == "" || tag == "" {
		return nil, errors.New("set all of PLATFORM_APP_DB_ROUTE_TAILNET, PLATFORM_APP_DB_ROUTE_PROXY_CLASS and PLATFORM_APP_DB_ROUTE_TAG, or none")
	}
	v := getenv("PLATFORM_APP_DB_HOST_TEMPLATES")
	if v == "" {
		return nil, errors.New("PLATFORM_APP_DB_ROUTE_* needs PLATFORM_APP_DB_HOST_TEMPLATES for the Service names")
	}
	tmpls, err := apps.ParseHostTemplates(v, environments)
	if err != nil {
		return nil, err
	}
	routes, err := ParseRoutes(tmpls)
	if err != nil {
		return nil, err
	}
	rc, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("the management cluster: %w", err)
	}
	rc.Timeout = 10 * time.Second
	cs, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return nil, err
	}
	m := New(Config{Client: cs, Pairs: pairs, Routes: routes, Tailnet: tailnet, ProxyClass: class, Tag: tag, Log: log})
	m.cfg.Log.Info("management database routes enabled", "routes", routes, "tailnet", tailnet, "proxyClass", class, "tag", tag)
	return m, nil
}

// Service is the Service of the route to instance's database in env.
func (m *Manager) Service(env, app, instance string) *corev1.Service {
	r := m.cfg.Routes[env]
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      r.Name(instance),
			Namespace: r.Namespace,
			Labels:    map[string]string{LabelManagedBy: ManagedBy, LabelApp: app, LabelRoute: env},
			Annotations: map[string]string{
				AnnotationTailnetFQDN: "hop-db-" + env + "-" + instance + "." + m.cfg.Tailnet,
				AnnotationHostname:    "hop-db-management-to-" + env + "-" + instance,
				AnnotationTags:        m.cfg.Tag,
				AnnotationProxyClass:  m.cfg.ProxyClass,
			},
		},
		Spec: corev1.ServiceSpec{
			Type:         corev1.ServiceTypeExternalName,
			ExternalName: Placeholder,
			Ports: []corev1.ServicePort{{
				Name: "postgres", Port: 5432, TargetPort: intstr.FromInt32(5432), Protocol: corev1.ProtocolTCP,
			}},
		},
	}
}

// ours reports whether svc is the route Service of an app the platform made,
// and which app and environment: all three labels, and the name the route
// of that environment gives that app.
func (m *Manager) ours(svc *corev1.Service) (app, env string, ok bool) {
	app, env = svc.Labels[LabelApp], svc.Labels[LabelRoute]
	r, known := m.cfg.Routes[env]
	if svc.Labels[LabelManagedBy] != ManagedBy || app == "" || !known ||
		svc.Namespace != r.Namespace || svc.Name != r.Name(app) {
		return "", "", false
	}
	return app, env, true
}

// Ensure creates the missing route Services of d's pair in the
// environments of a, its app. One that exists is left as it is (the
// operator changes it). A Service of the same name that is not ours is
// ErrConflict.
func (m *Manager) Ensure(ctx context.Context, d store.AppDatabase, a store.App) error {
	var errs []error
	for _, env := range m.envs(d, a) {
		if err := m.ensure(ctx, env, d.App); err != nil {
			errs = append(errs, fmt.Errorf("route of %s to %s: %w", d.App, env, err))
		}
	}
	return errors.Join(errs...)
}

func (m *Manager) ensure(ctx context.Context, env, app string) error {
	want := m.Service(env, app, app) // the default instance
	if msgs := validation.IsDNS1035Label(want.Name); len(msgs) > 0 {
		return fmt.Errorf("service name %s: %s", want.Name, strings.Join(msgs, "; "))
	}
	svcs := m.cfg.Client.CoreV1().Services(want.Namespace)
	got, err := svcs.Get(ctx, want.Name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		_, err = svcs.Create(ctx, want, metav1.CreateOptions{FieldManager: ManagedBy})
		if apierrors.IsAlreadyExists(err) {
			return nil // made meanwhile; the next round checks whose it is
		}
		if err == nil {
			m.cfg.Log.Info("database route created", "app", app, "env", env, "service", want.Namespace+"/"+want.Name)
		}
		return err
	case err != nil:
		return err
	}
	if a, _, ok := m.ours(got); !ok || a != app {
		m.cfg.Log.Warn("database route not created: a service of its name is not the platform's; it is left alone",
			"app", app, "env", env, "service", want.Namespace+"/"+want.Name, "managedBy", got.Labels[LabelManagedBy], "labelApp", got.Labels[LabelApp])
		return ErrConflict
	}
	if got.DeletionTimestamp != nil {
		m.cfg.Log.Info("database route is being deleted; it is created again once it is gone", "app", app, "env", env, "service", want.Namespace+"/"+want.Name)
		return nil
	}
	if !annotationsMatch(want, got) {
		// Never updated: a changed setting applies to new routes only.
		m.cfg.Log.Warn("database route differs from the platform's settings; it is kept as it is", "app", app, "env", env, "service", want.Namespace+"/"+want.Name)
	}
	return nil
}

// annotationsMatch reports whether got has want's annotations with want's
// values; got may have more.
func annotationsMatch(want, got *corev1.Service) bool {
	for k, v := range want.Annotations {
		if got.Annotations[k] != v {
			return false
		}
	}
	return true
}

// Remove deletes the route Services of app in every environment. A Service
// of a route's name that is not ours is left alone.
func (m *Manager) Remove(ctx context.Context, app string) error {
	return m.remove(ctx, app, nil)
}

// remove deletes the route Services of app in every environment but keep.
func (m *Manager) remove(ctx context.Context, app string, keep []string) error {
	var errs []error
	for _, env := range slices.Sorted(maps.Keys(m.cfg.Routes)) {
		if slices.Contains(keep, env) {
			continue
		}
		r := m.cfg.Routes[env]
		got, err := m.cfg.Client.CoreV1().Services(r.Namespace).Get(ctx, r.Name(app), metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(err):
			continue
		case err != nil:
			errs = append(errs, err)
			continue
		}
		if a, _, ok := m.ours(got); !ok || a != app {
			m.cfg.Log.Warn("database route not deleted: the service of its name is not the platform's", "app", app, "env", env, "service", got.Namespace+"/"+got.Name)
			continue
		}
		if err := m.delete(ctx, got, app, env); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// delete deletes svc if it is still the object that was read.
func (m *Manager) delete(ctx context.Context, svc *corev1.Service, app, env string) error {
	if svc.DeletionTimestamp != nil {
		return nil
	}
	uid := svc.UID
	err := m.cfg.Client.CoreV1().Services(svc.Namespace).Delete(ctx, svc.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("delete route %s/%s: %w", svc.Namespace, svc.Name, err)
	}
	m.cfg.Log.Info("database route deleted", "app", app, "env", env, "service", svc.Namespace+"/"+svc.Name)
	return nil
}

// Sync makes the routes of app what its pair and its environments say:
// the missing ones are created and those of an environment the app no
// longer has are deleted; without a pair every route of the app goes.
func (m *Manager) Sync(ctx context.Context, app string) error {
	d, err := m.cfg.Pairs.GetAppDatabase(ctx, app)
	if errors.Is(err, store.ErrNotFound) {
		return m.Remove(ctx, app)
	}
	if err != nil {
		return fmt.Errorf("read the database pair of %s: %w", app, err)
	}
	a, err := m.cfg.Pairs.GetApp(ctx, app)
	if err != nil {
		return fmt.Errorf("read app %s: %w", app, err)
	}
	return errors.Join(m.Ensure(ctx, d, a), m.remove(ctx, app, m.envs(d, a)))
}

// Reconcile creates the missing routes of every pair, deletes the routes of
// apps whose pair is gone and those of an environment a paired app no
// longer has. It deletes nothing when the pairs or the apps cannot be
// listed or there are no pairs, and a route of an app without a listed pair
// only when reading its app's pair says it does not exist.
func (m *Manager) Reconcile(ctx context.Context) error {
	pairs, err := m.cfg.Pairs.ListAppDatabases(ctx)
	if err != nil {
		return fmt.Errorf("list database pairs: %w", err)
	}
	apps, err := m.cfg.Pairs.ListApps(ctx)
	if err != nil {
		return fmt.Errorf("list apps: %w", err)
	}
	byName := map[string]store.App{}
	for _, a := range apps {
		byName[a.Name] = a
	}
	var errs []error
	paired := map[string]bool{}
	wanted := map[string][]string{} // by app; absent for a pair whose app was not listed
	for _, d := range pairs {
		paired[d.App] = true
		a, ok := byName[d.App]
		if !ok {
			continue // registered or deleted between the two lists
		}
		wanted[d.App] = m.envs(d, a)
		if err := m.Ensure(ctx, d, a); err != nil {
			errs = append(errs, err)
		}
	}
	if len(pairs) == 0 {
		return errors.Join(errs...)
	}
	namespaces := map[string]bool{}
	for _, r := range m.cfg.Routes {
		namespaces[r.Namespace] = true
	}
	for _, ns := range slices.Sorted(maps.Keys(namespaces)) {
		list, err := m.cfg.Client.CoreV1().Services(ns).List(ctx, metav1.ListOptions{LabelSelector: LabelManagedBy + "=" + ManagedBy})
		if err != nil {
			errs = append(errs, fmt.Errorf("list routes in %s: %w", ns, err))
			continue
		}
		for i := range list.Items {
			svc := &list.Items[i]
			app, env, ok := m.ours(svc)
			if !ok {
				continue
			}
			if paired[app] {
				if keep, known := wanted[app]; known && !slices.Contains(keep, env) {
					if err := m.delete(ctx, svc, app, env); err != nil {
						errs = append(errs, err)
					}
				}
				continue
			}
			_, err := m.cfg.Pairs.GetAppDatabase(ctx, app)
			switch {
			case errors.Is(err, store.ErrNotFound):
				if err := m.delete(ctx, svc, app, env); err != nil {
					errs = append(errs, err)
				}
			case err != nil:
				errs = append(errs, fmt.Errorf("read the database pair of %s: %w", app, err))
			}
		}
	}
	return errors.Join(errs...)
}

// Run reconciles now and then every Interval until ctx ends.
func (m *Manager) Run(ctx context.Context) {
	t := time.NewTicker(Interval)
	defer t.Stop()
	for {
		rctx, cancel := context.WithTimeout(ctx, time.Minute)
		if err := m.Reconcile(rctx); err != nil {
			m.cfg.Log.Warn("database routes not reconciled", "err", err)
		}
		cancel()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Added creates the routes of a pair just registered, in the background.
// A failure is only logged; Run makes them later.
func (m *Manager) Added(d store.AppDatabase) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := m.Sync(ctx, d.App); err != nil {
			m.cfg.Log.Warn("database routes of a new app not created yet; retried every few minutes", "app", d.App, "err", err)
		}
	}()
}

// Changed makes the routes of an app whose environments just changed (ADR
// 0037) follow them, in the background. A failure is only logged; Run
// reconciles later.
func (m *Manager) Changed(app string) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := m.Sync(ctx, app); err != nil {
			m.cfg.Log.Warn("database routes of an app whose environments changed not updated yet; retried every few minutes", "app", app, "err", err)
		}
	}()
}

// Removed deletes the routes of an app just deleted, in the background. A
// failure is only logged; Run deletes them later.
func (m *Manager) Removed(app string) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := m.Remove(ctx, app); err != nil {
			m.cfg.Log.Warn("database routes of a deleted app not deleted yet; retried every few minutes", "app", app, "err", err)
		}
	}()
}
