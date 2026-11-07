package dbroute

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

const ns = "hopcharlie-db-network"

// pairs is a fake Pairs. getErr, when set for an app, is what
// GetAppDatabase returns for it. Every pair's app exists, in envs[app] or
// else in both environments of the pair.
type pairs struct {
	list    []store.AppDatabase
	listErr error
	getErr  map[string]error
	envs    map[string][]string
	appsErr error
}

func (p *pairs) ListApps(context.Context) ([]store.App, error) {
	var out []store.App
	for _, d := range p.list {
		a, _ := p.GetApp(context.Background(), d.App)
		out = append(out, a)
	}
	return out, p.appsErr
}

func (p *pairs) GetApp(_ context.Context, name string) (store.App, error) {
	for _, d := range p.list {
		if d.App == name {
			envs, ok := p.envs[name]
			if !ok {
				envs = []string{d.PrimaryEnv, d.PeerEnv}
			}
			return store.App{Name: name, Environments: envs}, nil
		}
	}
	return store.App{}, store.ErrNotFound
}

// both is the app of pair d in both of its environments.
func both(d store.AppDatabase) store.App {
	return store.App{Name: d.App, Environments: []string{d.PrimaryEnv, d.PeerEnv}}
}

func (p *pairs) ListAppDatabases(context.Context) ([]store.AppDatabase, error) {
	return p.list, p.listErr
}

func (p *pairs) GetAppDatabase(_ context.Context, app string) (store.AppDatabase, error) {
	if err := p.getErr[app]; err != nil {
		return store.AppDatabase{}, err
	}
	for _, d := range p.list {
		if d.App == app {
			return d, nil
		}
	}
	return store.AppDatabase{}, store.ErrNotFound
}

func newManager(t *testing.T, p *pairs, objs ...runtime.Object) (*Manager, *fake.Clientset) {
	t.Helper()
	routes, err := ParseRoutes(map[string]string{
		"eks": "db-eks-{instance}." + ns + ".svc.cluster.local",
		"gke": "db-gke-{instance}." + ns + ".svc.cluster.local",
	})
	if err != nil {
		t.Fatal(err)
	}
	cs := fake.NewClientset(objs...)
	return New(Config{
		Client: cs, Pairs: p, Routes: routes,
		Tailnet: "tail567fe8.ts.net", ProxyClass: "hopcharlie-app-db-egress-management", Tag: "tag:hop-db-client-management",
	}), cs
}

func pair(app string) store.AppDatabase {
	return store.AppDatabase{App: app, PrimaryEnv: "gke", PeerEnv: "eks"}
}

func service(name string, labels map[string]string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels},
		Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeExternalName, ExternalName: "something"},
	}
}

func ourLabels(app, env string) map[string]string {
	return map[string]string{LabelManagedBy: ManagedBy, LabelApp: app, LabelRoute: env}
}

func get(t *testing.T, cs *fake.Clientset, name string) (*corev1.Service, bool) {
	t.Helper()
	svc, err := cs.CoreV1().Services(ns).Get(context.Background(), name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return svc, true
}

// verbs are the write verbs the client was asked for.
func verbs(cs *fake.Clientset) []string {
	var out []string
	for _, a := range cs.Actions() {
		switch v := a.GetVerb(); v {
		case "create", "update", "patch", "delete":
			out = append(out, v+" "+a.GetResource().Resource)
		}
	}
	return out
}

func TestParseRoutes(t *testing.T) {
	r, err := ParseRoutes(map[string]string{"eks": "db-eks-{instance}.hopcharlie-db-network.svc.cluster.local"})
	if err != nil {
		t.Fatal(err)
	}
	if got := r["eks"]; got.Namespace != ns || got.Name("shop") != "db-eks-shop" {
		t.Fatalf("route = %+v, name %s", got, got.Name("shop"))
	}
	for _, bad := range []string{
		"db-eks-{instance}.hopcharlie-db-network", // not a cluster service
		"db-eks.{instance}.svc.cluster.local",     // instance in the namespace
		"db-eks-{instance}.a.b.svc.cluster.local", // dotted namespace
		"db-{instance}-{instance}.ns.svc.cluster.local",
	} {
		if _, err := ParseRoutes(map[string]string{"eks": bad}); err == nil {
			t.Errorf("%s: accepted", bad)
		}
	}
}

func TestEnsureCreatesTheContractService(t *testing.T) {
	m, cs := newManager(t, &pairs{})
	if err := m.Ensure(context.Background(), pair("moimroom-mig"), both(pair("moimroom-mig"))); err != nil {
		t.Fatal(err)
	}
	for _, env := range []string{"eks", "gke"} {
		svc, ok := get(t, cs, "db-"+env+"-moimroom-mig")
		if !ok {
			t.Fatalf("%s: not created", env)
		}
		wantAnn := map[string]string{
			"tailscale.com/tailnet-fqdn": "hop-db-" + env + "-moimroom-mig.tail567fe8.ts.net",
			"tailscale.com/hostname":     "hop-db-management-to-" + env + "-moimroom-mig",
			"tailscale.com/tags":         "tag:hop-db-client-management",
			"tailscale.com/proxy-class":  "hopcharlie-app-db-egress-management",
		}
		if len(svc.Annotations) != len(wantAnn) {
			t.Errorf("%s annotations = %v", env, svc.Annotations)
		}
		for k, v := range wantAnn {
			if svc.Annotations[k] != v {
				t.Errorf("%s annotation %s = %q, want %q", env, k, svc.Annotations[k], v)
			}
		}
		wantLabels := map[string]string{
			"app.kubernetes.io/managed-by": "hopcharlie-platform", "hopcharlie.dev/app": "moimroom-mig", "hopcharlie.dev/db-route": env,
		}
		if len(svc.Labels) != len(wantLabels) {
			t.Errorf("%s labels = %v", env, svc.Labels)
		}
		for k, v := range wantLabels {
			if svc.Labels[k] != v {
				t.Errorf("%s label %s = %q, want %q", env, k, svc.Labels[k], v)
			}
		}
		if svc.Spec.Type != corev1.ServiceTypeExternalName || svc.Spec.ExternalName != "placeholder" {
			t.Errorf("%s spec = %s %s", env, svc.Spec.Type, svc.Spec.ExternalName)
		}
		if p := svc.Spec.Ports; len(p) != 1 || p[0].Name != "postgres" || p[0].Port != 5432 || p[0].TargetPort.IntVal != 5432 || p[0].Protocol != corev1.ProtocolTCP {
			t.Errorf("%s ports = %+v", env, p)
		}
	}
}

func TestEnsureIsIdempotentAndNeverUpdates(t *testing.T) {
	m, cs := newManager(t, &pairs{})
	ctx := context.Background()
	if err := m.Ensure(ctx, pair("shop"), both(pair("shop"))); err != nil {
		t.Fatal(err)
	}
	// The operator points the Service at its proxy.
	svc, _ := get(t, cs, "db-eks-shop")
	svc.Spec.ExternalName = "ts-db-eks-shop-abcde.tailscale.svc.cluster.local"
	if _, err := cs.CoreV1().Services(ns).Update(ctx, svc, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	cs.ClearActions()
	if err := m.Ensure(ctx, pair("shop"), both(pair("shop"))); err != nil {
		t.Fatal(err)
	}
	if v := verbs(cs); len(v) != 0 {
		t.Fatalf("second Ensure wrote %v", v)
	}
	if svc, _ := get(t, cs, "db-eks-shop"); svc.Spec.ExternalName != "ts-db-eks-shop-abcde.tailscale.svc.cluster.local" {
		t.Fatalf("externalName = %s", svc.Spec.ExternalName)
	}
}

func TestEnsureLeavesAForeignServiceAlone(t *testing.T) {
	foreign := service("db-eks-shop", map[string]string{"app.kubernetes.io/managed-by": "Helm"})
	other := service("db-gke-shop", ourLabels("other", "gke")) // ours, but of another app
	m, cs := newManager(t, &pairs{}, foreign, other)
	err := m.Ensure(context.Background(), pair("shop"), both(pair("shop")))
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
	if v := verbs(cs); len(v) != 0 {
		t.Fatalf("wrote %v", v)
	}
	if svc, _ := get(t, cs, "db-eks-shop"); svc.Labels["app.kubernetes.io/managed-by"] != "Helm" || svc.Spec.ExternalName != "something" {
		t.Fatalf("foreign service changed: %+v", svc)
	}
}

func TestReconcile(t *testing.T) {
	ctx := context.Background()
	orphans := func() []runtime.Object {
		return []runtime.Object{
			service("db-eks-gone", ourLabels("gone", "eks")),
			service("db-gke-gone", ourLabels("gone", "gke")),
			service("db-eks", nil), // the static route (infra #99)
			service("db-eks-helm", map[string]string{"app.kubernetes.io/managed-by": "Helm", LabelApp: "helm", LabelRoute: "eks"}),
			service("db-eks-renamed", ourLabels("x", "eks")), // labels do not match its name
		}
	}
	untouched := []string{"db-eks", "db-eks-helm", "db-eks-renamed"}

	t.Run("creates missing routes and deletes those of apps without a pair", func(t *testing.T) {
		m, cs := newManager(t, &pairs{list: []store.AppDatabase{pair("shop")}}, orphans()...)
		if err := m.Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
		for _, n := range []string{"db-eks-shop", "db-gke-shop"} {
			if _, ok := get(t, cs, n); !ok {
				t.Errorf("%s not created", n)
			}
		}
		for _, n := range []string{"db-eks-gone", "db-gke-gone"} {
			if _, ok := get(t, cs, n); ok {
				t.Errorf("%s not deleted", n)
			}
		}
		for _, n := range untouched {
			if _, ok := get(t, cs, n); !ok {
				t.Errorf("%s deleted", n)
			}
		}
		// Again: nothing to do.
		cs.ClearActions()
		if err := m.Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
		if v := verbs(cs); len(v) != 0 {
			t.Fatalf("second round wrote %v", v)
		}
	})

	for name, p := range map[string]Pairs{
		"list fails":      &pairs{listErr: errors.New("database down")},
		"no pairs":        &pairs{},
		"pair read fails": &pairs{list: []store.AppDatabase{pair("shop")}, getErr: map[string]error{"gone": errors.New("timeout")}},
		// Listed without it, read with it: registered again meanwhile.
		"pair appeared again": &appearing{pairs: &pairs{list: []store.AppDatabase{pair("shop")}}, app: "gone"},
	} {
		t.Run("deletes nothing when "+name, func(t *testing.T) {
			m, cs := newManager(t, &pairs{}, orphans()...)
			m.cfg.Pairs = p
			_ = m.Reconcile(ctx)
			for _, v := range verbs(cs) {
				if strings.HasPrefix(v, "delete") {
					t.Fatalf("deleted: %v", verbs(cs))
				}
			}
		})
	}
}

// appearing lists pairs without app but reads app's pair as present.
type appearing struct {
	*pairs
	app string
}

func (a *appearing) GetAppDatabase(ctx context.Context, app string) (store.AppDatabase, error) {
	if app == a.app {
		return pair(app), nil
	}
	return a.pairs.GetAppDatabase(ctx, app)
}

func TestRemove(t *testing.T) {
	m, cs := newManager(t, &pairs{},
		service("db-eks-shop", ourLabels("shop", "eks")),
		service("db-gke-shop", map[string]string{"app.kubernetes.io/managed-by": "Helm"}), // not ours
		service("db-eks-other", ourLabels("other", "eks")),
	)
	if err := m.Remove(context.Background(), "shop"); err != nil {
		t.Fatal(err)
	}
	if _, ok := get(t, cs, "db-eks-shop"); ok {
		t.Error("db-eks-shop not deleted")
	}
	for _, n := range []string{"db-gke-shop", "db-eks-other"} {
		if _, ok := get(t, cs, n); !ok {
			t.Errorf("%s deleted", n)
		}
	}
	// Nothing left: no error.
	if err := m.Remove(context.Background(), "shop"); err != nil {
		t.Fatal(err)
	}
}

func TestFromEnv(t *testing.T) {
	env := func(kv map[string]string) func(string) string { return func(k string) string { return kv[k] } }
	m, err := FromEnv(env(map[string]string{"PLATFORM_APP_DB_HOST_TEMPLATES": "eks=db-eks-{instance}.ns.svc.cluster.local"}), []string{"eks"}, &pairs{}, nil)
	if m != nil || err != nil {
		t.Fatalf("off: %v, %v", m, err)
	}
	for name, kv := range map[string]map[string]string{
		"partial":      {"PLATFORM_APP_DB_ROUTE_TAILNET": "t.ts.net"},
		"no templates": {"PLATFORM_APP_DB_ROUTE_TAILNET": "t.ts.net", "PLATFORM_APP_DB_ROUTE_PROXY_CLASS": "c", "PLATFORM_APP_DB_ROUTE_TAG": "tag:x"},
		"bad template": {"PLATFORM_APP_DB_ROUTE_TAILNET": "t.ts.net", "PLATFORM_APP_DB_ROUTE_PROXY_CLASS": "c", "PLATFORM_APP_DB_ROUTE_TAG": "tag:x",
			"PLATFORM_APP_DB_HOST_TEMPLATES": "eks=db-eks-{instance}.example.com"},
	} {
		if _, err := FromEnv(env(kv), []string{"eks"}, &pairs{}, nil); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

// TestRoutesFollowTheAppsEnvironments: a pair whose peer is not one of the
// app's environments (registered in one, or taken out again, ADR 0037)
// routes only to its primary, and the peer's route goes.
func TestRoutesFollowTheAppsEnvironments(t *testing.T) {
	ctx := context.Background()
	alone := &pairs{list: []store.AppDatabase{pair("shop")}, envs: map[string][]string{"shop": {"gke"}}}

	t.Run("ensure", func(t *testing.T) {
		m, cs := newManager(t, alone)
		if err := m.Ensure(ctx, pair("shop"), store.App{Name: "shop", Environments: []string{"gke"}}); err != nil {
			t.Fatal(err)
		}
		if _, ok := get(t, cs, "db-gke-shop"); !ok {
			t.Fatal("the primary's route is missing")
		}
		if _, ok := get(t, cs, "db-eks-shop"); ok {
			t.Fatal("a route to a peer that is not the app's")
		}
	})

	t.Run("reconcile deletes the route of a removed environment", func(t *testing.T) {
		m, cs := newManager(t, alone, service("db-eks-shop", ourLabels("shop", "eks")), service("db-gke-shop", ourLabels("shop", "gke")))
		if err := m.Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
		if _, ok := get(t, cs, "db-eks-shop"); ok {
			t.Fatal("db-eks-shop not deleted")
		}
		if _, ok := get(t, cs, "db-gke-shop"); !ok {
			t.Fatal("db-gke-shop deleted")
		}
	})

	t.Run("reconcile deletes nothing when the apps cannot be listed", func(t *testing.T) {
		p := &pairs{list: alone.list, envs: alone.envs, appsErr: errors.New("database down")}
		m, cs := newManager(t, p, service("db-eks-shop", ourLabels("shop", "eks")))
		if err := m.Reconcile(ctx); err == nil {
			t.Fatal("no error")
		}
		if v := verbs(cs); len(v) != 0 {
			t.Fatalf("wrote %v", v)
		}
	})

	t.Run("sync after an environment is added and removed", func(t *testing.T) {
		p := &pairs{list: []store.AppDatabase{pair("shop")}, envs: map[string][]string{"shop": {"gke"}}}
		m, cs := newManager(t, p)
		if err := m.Sync(ctx, "shop"); err != nil {
			t.Fatal(err)
		}
		if _, ok := get(t, cs, "db-eks-shop"); ok {
			t.Fatal("route to eks before it was added")
		}
		p.envs["shop"] = []string{"gke", "eks"}
		if err := m.Sync(ctx, "shop"); err != nil {
			t.Fatal(err)
		}
		if _, ok := get(t, cs, "db-eks-shop"); !ok {
			t.Fatal("no route to eks once it was added")
		}
		p.envs["shop"] = []string{"gke"}
		if err := m.Sync(ctx, "shop"); err != nil {
			t.Fatal(err)
		}
		if _, ok := get(t, cs, "db-eks-shop"); ok {
			t.Fatal("route to eks kept after it was removed")
		}
		if _, ok := get(t, cs, "db-gke-shop"); !ok {
			t.Fatal("route to gke deleted")
		}
		// No pair: every route of the app goes.
		p.list = nil
		if err := m.Sync(ctx, "shop"); err != nil {
			t.Fatal(err)
		}
		if _, ok := get(t, cs, "db-gke-shop"); ok {
			t.Fatal("route kept without a pair")
		}
	})
}
