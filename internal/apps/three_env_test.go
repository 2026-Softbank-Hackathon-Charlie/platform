package apps

import (
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

func addThirdRegistryEnvironment(t *testing.T, f *registryFixture, app string) {
	t.Helper()
	if err := f.store.SetAppEnvironments(t.Context(), app, []string{"gke", "eks"}, []string{"gke", "eks", "onprem"}); err != nil {
		t.Fatal(err)
	}
	f.cat.registry.HostTemplates["onprem"] = "db-onprem-{instance}.hopcharlie-db-network.svc.cluster.local"
}

func preserveRegistryPasswordEnvironment(t *testing.T, app string) {
	t.Helper()
	for _, env := range []string{"gke", "eks", "onprem"} {
		t.Setenv(RegistryPasswordEnv(env, app), "")
	}
}

func TestRegisteredThreeEnvironmentCatalogAndPolicy(t *testing.T) {
	f := newFileFixture(t)
	f.registerResources("mig", "db=postgres", "photos=s3")
	preserveRegistryPasswordEnvironment(t, "mig")

	// Add the third member after the catalog was created: every consumer must
	// read current membership from the store, rather than the original DB pair.
	addThirdRegistryEnvironment(t, f, "mig")
	f.cat.registry.BucketPrefixes["onprem"] = "hopcharlie-op-"
	wantEnvs := []string{"gke", "eks", "onprem"}
	stored, err := f.store.GetApp(t.Context(), "mig")
	if err != nil || !reflect.DeepEqual(stored.Environments, wantEnvs) {
		t.Fatalf("GetApp environments=%v err=%v", stored.Environments, err)
	}
	listed, err := f.store.ListApps(t.Context())
	if err != nil || len(listed) != 1 || !reflect.DeepEqual(listed[0].Environments, wantEnvs) {
		t.Fatalf("ListApps=%+v err=%v", listed, err)
	}
	view, found, err := f.cat.Lookup(t.Context(), "mig")
	if err != nil || !found {
		t.Fatalf("Lookup found=%v err=%v", found, err)
	}
	gotEnvs := make([]string, 0, len(view.Environments))
	for _, env := range view.Environments {
		gotEnvs = append(gotEnvs, env.Env)
		if env.Env == "onprem" && (env.CnpgCluster != "mig-db" || env.DbHost != "db-onprem-mig.hopcharlie-db-network.svc.cluster.local:5432" || env.Bucket == nil || *env.Bucket != "onprem:hopcharlie-op-mig") {
			t.Errorf("onprem view=%+v", env)
		}
	}
	if !reflect.DeepEqual(gotEnvs, wantEnvs) {
		t.Fatalf("Lookup environments=%v", gotEnvs)
	}

	for _, route := range [][2]string{{"eks", "gke"}, {"gke", "onprem"}} {
		p, err := f.cat.Draft(t.Context(), apiv1.PlanDraftRequest{App: "mig", Source: route[0], Target: route[1]})
		if err != nil {
			t.Fatal(err)
		}
		if p.AllowSourceRewire == nil || !*p.AllowSourceRewire {
			t.Errorf("three-member draft %s->%s did not opt in to source rewire", route[0], route[1])
		}
		if p.Source.Env != route[0] || p.Target.Env != route[1] || p.Source.Namespace != "mig" || p.Target.Cluster != "mig-db" {
			t.Errorf("incorrect three-member draft: %+v", p)
		}
		if route[1] == "onprem" && (p.Files == nil || p.Files.Source != "gke:hopcharlie-gke-mig" || p.Files.Target != "onprem:hopcharlie-op-mig" || !strings.Contains(p.Target.Dsn, "@db-onprem-mig.hopcharlie-db-network.svc.cluster.local:5432/app")) {
			t.Errorf("onprem draft files=%+v target=%+v", p.Files, p.Target)
		}
	}
	if os.Getenv("ONPREM_MIG_PGPASSWORD") != "pw" {
		t.Fatal("draft did not resolve the shared DB password for onprem")
	}

	passwords, hosts, err := f.cat.RegistryHosts(t.Context())
	onpremHost := "db-onprem-mig.hopcharlie-db-network.svc.cluster.local:5432"
	if err != nil || !reflect.DeepEqual(passwords["ONPREM_MIG_PGPASSWORD"], []string{onpremHost}) || len(passwords) != 3 || len(hosts["mig"]) != 3 || !slices.Contains(hosts["mig"], onpremHost) {
		t.Fatalf("RegistryHosts passwords=%v hosts=%v err=%v", passwords, hosts, err)
	}
	if err := os.Setenv("ONPREM_MIG_PGPASSWORD", ""); err != nil {
		t.Fatal(err)
	}
	if err := f.cat.SetRegistryPasswords(t.Context(), []string{"ONPREM_MIG_PGPASSWORD"}); err != nil || os.Getenv("ONPREM_MIG_PGPASSWORD") != "pw" {
		t.Fatalf("SetRegistryPasswords failed to resolve onprem password: %v", err)
	}
	buckets, err := f.cat.RegistryBuckets(t.Context())
	if err != nil || !reflect.DeepEqual(buckets, []string{"gke:hopcharlie-gke-mig", "eks:hopcharlie-eks-mig", "onprem:hopcharlie-op-mig"}) {
		t.Fatalf("RegistryBuckets=%v err=%v", buckets, err)
	}
}

func TestRegisteredTwoEnvironmentDraftDoesNotOptInToSourceRewire(t *testing.T) {
	f := newRegistryFixture(t)
	f.register("mig", "pw")
	preserveRegistryPasswordEnvironment(t, "mig")
	p, err := f.cat.Draft(t.Context(), apiv1.PlanDraftRequest{App: "mig", Source: "gke", Target: "eks"})
	if err != nil || p.AllowSourceRewire != nil {
		t.Fatalf("two-member draft source rewire=%v err=%v", p.AllowSourceRewire, err)
	}
}

func TestRegisteredThreeEnvironmentMissingPhotoBucketFailsClosed(t *testing.T) {
	for _, missing := range [][]string{{"onprem"}, {"gke"}, {"gke", "onprem"}} {
		t.Run(strings.Join(missing, "+"), func(t *testing.T) {
			f := newFileFixture(t)
			f.registerResources("mig", "db=postgres", "photos=s3")
			preserveRegistryPasswordEnvironment(t, "mig")
			addThirdRegistryEnvironment(t, f, "mig")
			f.cat.registry.BucketPrefixes["onprem"] = "hopcharlie-op-"
			for _, env := range missing {
				delete(f.cat.registry.BucketPrefixes, env)
			}
			req := apiv1.PlanDraftRequest{App: "mig", Source: "gke", Target: "onprem"}
			for _, files := range []*bool{nil, ptr(true)} {
				req.Files = files
				if _, err := f.cat.Draft(t.Context(), req); !errors.Is(err, ErrInvalid) {
					t.Errorf("photos with missing bucket and files=%v: err=%v, want ErrInvalid", files, err)
				}
			}
			req.Files = ptr(false)
			if p, err := f.cat.Draft(t.Context(), req); err != nil || p.Files != nil {
				t.Fatalf("explicit files=false: files=%+v err=%v", p.Files, err)
			}
		})
	}
}

func enginePlanFromThreeEnvironmentDraft(t *testing.T, f *registryFixture, app, source, target string) plan.Plan {
	t.Helper()
	draft, err := f.cat.Draft(t.Context(), apiv1.PlanDraftRequest{App: app, Source: source, Target: target})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(draft)
	if err != nil {
		t.Fatal(err)
	}
	var p plan.Plan
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatal(err)
	}
	// The real parser expands these process-owned password references before
	// authorizing a plan. Use fixture values without consulting live secrets.
	for _, ep := range []*plan.Endpoint{&p.Source, &p.Target} {
		ep.DSN = strings.ReplaceAll(ep.DSN, "${"+RegistryPasswordEnv(ep.Env, app)+"}", "pw")
	}
	return p
}

func TestSourceRewireAllowedBindsThreeMemberEndpoints(t *testing.T) {
	if allowed, err := exampleCatalog(t, "").SourceRewireAllowed(t.Context(), &plan.Plan{AllowSourceRewire: true}); err != nil || allowed {
		t.Fatalf("static-only catalog accepted source rewiring: allowed=%v err=%v", allowed, err)
	}
	f := newRegistryFixture(t)
	f.register("mig", "pw")
	preserveRegistryPasswordEnvironment(t, "mig")
	addThirdRegistryEnvironment(t, f, "mig")
	valid := enginePlanFromThreeEnvironmentDraft(t, f, "mig", "gke", "onprem")
	if allowed, err := f.cat.SourceRewireAllowed(t.Context(), &valid); err != nil || !allowed {
		t.Fatalf("registered three-member plan allowed=%v err=%v", allowed, err)
	}
	for _, tc := range []struct {
		name   string
		change func(*plan.Plan)
	}{
		{"source env", func(p *plan.Plan) { p.Source.Env = "other" }},
		{"target env", func(p *plan.Plan) { p.Target.Env = "other" }},
		{"same env", func(p *plan.Plan) { p.Target = p.Source }},
		{"source context", func(p *plan.Plan) { p.Source.Context = "other-context" }},
		{"target context", func(p *plan.Plan) { p.Target.Context = "other-context" }},
		{"source namespace", func(p *plan.Plan) { p.Source.Namespace = "other-app" }},
		{"target namespace", func(p *plan.Plan) { p.Target.Namespace = "other-app" }},
		{"source cluster", func(p *plan.Plan) { p.Source.Cluster = "other-db" }},
		{"target cluster", func(p *plan.Plan) { p.Target.Cluster = "other-db" }},
		{"source host", func(p *plan.Plan) { p.Source.DSN = "postgres://app:pw@other.internal:5432/app?sslmode=require" }},
		{"target host", func(p *plan.Plan) { p.Target.DSN = "postgres://app:pw@other.internal:5432/app?sslmode=require" }},
		{"target port", func(p *plan.Plan) { p.Target.DSN = strings.Replace(p.Target.DSN, ":5432/", ":5433/", 1) }},
		{"target database", func(p *plan.Plan) { p.Target.DSN = strings.Replace(p.Target.DSN, "/app?", "/other?", 1) }},
		{"target user", func(p *plan.Plan) { p.Target.DSN = strings.Replace(p.Target.DSN, "//app:", "//other:", 1) }},
		{"malformed DSN", func(p *plan.Plan) { p.Target.DSN = "postgres://[malformed" }},
		{"fallback host", func(p *plan.Plan) {
			p.Target.DSN = "host=db-onprem-mig.hopcharlie-db-network.svc.cluster.local,other.internal port=5432 user=app password=pw dbname=app sslmode=require"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := valid
			tc.change(&p)
			if allowed, err := f.cat.SourceRewireAllowed(t.Context(), &p); err != nil || allowed {
				t.Fatalf("forged plan allowed=%v err=%v", allowed, err)
			}
		})
	}

	// Static apps remain excluded even when a same-named registered row has
	// three environments: catalog configuration takes precedence over it.
	f.register("moimroom", "pw")
	addThirdRegistryEnvironment(t, f, "moimroom")
	preserveRegistryPasswordEnvironment(t, "moimroom")
	static := enginePlanFromThreeEnvironmentDraft(t, f, "moimroom", "gke", "onprem")
	if allowed, err := f.cat.SourceRewireAllowed(t.Context(), &static); err != nil || allowed {
		t.Fatalf("static app allowed=%v err=%v", allowed, err)
	}
	// Membership is checked at authorization time, including a third member
	// removed after drafting a once-valid plan.
	if err := f.store.SetAppEnvironments(t.Context(), "mig", []string{"gke", "eks", "onprem"}, []string{"gke", "eks"}); err != nil {
		t.Fatal(err)
	}
	if allowed, err := f.cat.SourceRewireAllowed(t.Context(), &valid); err != nil || allowed {
		t.Fatalf("removed third member allowed=%v err=%v", allowed, err)
	}
	two := enginePlanFromThreeEnvironmentDraft(t, f, "mig", "gke", "eks")
	if allowed, err := f.cat.SourceRewireAllowed(t.Context(), &two); err != nil || allowed {
		t.Fatalf("two-member app allowed=%v err=%v", allowed, err)
	}
}
