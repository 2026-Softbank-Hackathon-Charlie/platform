package apps

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/plans"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

// registerStateless registers app in envs with a public service, the
// resources of types and no database pair, as docker-doom is (ADR 0049).
func (f *registryFixture) registerStateless(app string, envs []string, types ...string) {
	f.t.Helper()
	res := []map[string]string{}
	for _, t := range types {
		res = append(res, map[string]string{"name": t, "type": t})
	}
	services := []map[string]any{{"name": "web", "public": true}}
	spec, _ := json.Marshal(map[string]any{"name": app, "services": services, "resources": res})
	f.createApp(store.App{Name: app, Repo: "org/" + app, Branch: "main", Environments: envs, Spec: spec, CreatedAt: time.Now().UTC()})
}

func (f *registryFixture) createApp(a store.App) {
	f.t.Helper()
	if err := f.store.CreateApp(f.t.Context(), a); err != nil {
		f.t.Fatal(err)
	}
}

func TestStatelessAppIsAMigrationCandidate(t *testing.T) {
	f := newRegistryFixture(t)
	f.registerStateless("docker-doom", []string{"eks", "gke"})

	got, ok, err := f.cat.Lookup(t.Context(), "docker-doom")
	if err != nil || !ok {
		t.Fatalf("lookup: %v %v", ok, err)
	}
	want := apiv1.App{
		Name: "docker-doom", Namespace: "docker-doom",
		Database:     apiv1.AppDatabase{Schemas: []string{}},
		Environments: []apiv1.AppEnvironment{{Env: "gke"}, {Env: "eks"}}, // environments-file order
		Resources:    apiv1.AppResources{},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("app\n got %+v\nwant %+v", got, want)
	}
	b, _ := json.Marshal(got.Environments[0])
	if string(b) != `{"env":"gke"}` {
		t.Errorf("environment JSON %s, want no database fields", b)
	}
	st, err := f.cat.RegistryStateless(t.Context())
	if err != nil || !reflect.DeepEqual(st, map[string]map[string]plans.StatelessEnv{"docker-doom": {"eks": {Backend: "eks"}, "gke": {Backend: "gke"}}}) {
		t.Fatalf("RegistryStateless = %v, %v", st, err)
	}
	// A database app's hosts do not include it.
	if _, apps, _ := f.cat.RegistryHosts(t.Context()); len(apps) != 0 {
		t.Errorf("RegistryHosts apps %v", apps)
	}
}

func TestAppsThatAreNotStatelessCandidates(t *testing.T) {
	f := newRegistryFixture(t)
	f.registerStateless("solo", []string{"eks"})
	f.register("needs-pair", "") // postgres without a pair: as before
	// No public service: the gateway routes nothing of it.
	worker, _ := json.Marshal(map[string]any{"name": "worker", "services": []map[string]any{{"name": "w"}}})
	f.createApp(store.App{Name: "worker", Repo: "org/worker", Branch: "main", Environments: []string{"eks", "gke"}, Spec: worker, CreatedAt: time.Now().UTC()})
	// A spec that does not decode: resources unknown, not taken as stateless.
	f.createApp(store.App{Name: "broken", Repo: "org/broken", Branch: "main", Environments: []string{"eks", "gke"}, Spec: []byte(`"x"`), CreatedAt: time.Now().UTC()})
	// A pair whose spec lost its postgres resource: neither path.
	f.register("lost-db", "pw")
	lost, _ := json.Marshal(map[string]any{"name": "lost-db", "services": []map[string]any{{"name": "web", "public": true}}})
	a, _ := f.store.GetApp(t.Context(), "lost-db")
	a.Spec = lost
	if err := f.store.UpdateApp(t.Context(), a); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"solo", "needs-pair", "worker", "broken", "lost-db"} {
		if _, ok, err := f.cat.Lookup(t.Context(), name); ok || err != nil {
			t.Errorf("%s: lookup %v %v, want not found", name, ok, err)
		}
		_, err := f.cat.Draft(t.Context(), apiv1.PlanDraftRequest{App: name, Source: "eks", Target: "gke"})
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: draft %v, want ErrInvalid", name, err)
		}
	}
	if st, _ := f.cat.RegistryStateless(t.Context()); len(st) != 0 {
		t.Errorf("RegistryStateless = %v", st)
	}
}

func TestDraftStatelessApp(t *testing.T) {
	f := newRegistryFixture(t)
	f.registerStateless("docker-doom", []string{"eks", "gke"})
	p, err := f.cat.Draft(t.Context(), apiv1.PlanDraftRequest{App: "docker-doom", Source: "eks", Target: "gke"})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(p)
	want := `{"gateway":{"mode":"http","sourceBackend":"eks","targetBackend":"gke","token":"${GATEWAY_TOKEN}","url":"http://gateway.ts.net:9901/apps/docker-doom"},` +
		`"id":"docker-doom-eks-to-gke","source":{"env":"eks","kind":"none"},"target":{"env":"gke","kind":"none"}}`
	if string(b) != want {
		t.Fatalf("plan\n got %s\nwant %s", b, want)
	}
	for _, req := range []apiv1.PlanDraftRequest{
		{App: "docker-doom", Source: "eks", Target: "gke", SourceStats: ptr(true)},
		{App: "docker-doom", Source: "eks", Target: "gke", WrittenRecordsStream: ptr("app-docker-doom")},
		{App: "docker-doom", Source: "eks", Target: "gke", Files: ptr(true)}, // no s3 resource
		{App: "docker-doom", Source: "eks", Target: "onprem"},                // not registered there
	} {
		if _, err := f.cat.Draft(t.Context(), req); !errors.Is(err, ErrInvalid) {
			t.Errorf("%+v: %v, want ErrInvalid", req, err)
		}
	}
	noGateway := exampleCatalog(t, "").WithRegistry(f.cat.registry)
	if _, err := noGateway.Draft(t.Context(), apiv1.PlanDraftRequest{App: "docker-doom", Source: "eks", Target: "gke"}); !errors.Is(err, ErrInvalid) {
		t.Errorf("without a gateway: %v, want ErrInvalid", err)
	}
}

func TestDraftStatelessAppMovesItsFiles(t *testing.T) {
	f := newRegistryFixture(t)
	f.cat.registry.BucketPrefixes = map[string]string{"gke": "hopcharlie-gke-", "eks": "hopcharlie-eks-"}
	f.registerStateless("album", []string{"eks", "gke"}, "s3")

	a, _, _ := f.cat.Lookup(t.Context(), "album")
	if a.Resources != (apiv1.AppResources{S3: true}) || a.Environments[0].Bucket == nil || *a.Environments[0].Bucket != "gke:hopcharlie-gke-album" {
		t.Fatalf("app %+v", a)
	}
	p, err := f.cat.Draft(t.Context(), apiv1.PlanDraftRequest{App: "album", Source: "eks", Target: "gke"})
	if err != nil {
		t.Fatal(err)
	}
	if p.Files == nil || p.Files.Source != "eks:hopcharlie-eks-album" || p.Files.Target != "gke:hopcharlie-gke-album" {
		t.Fatalf("files %+v", p.Files)
	}
	if p, _ := f.cat.Draft(t.Context(), apiv1.PlanDraftRequest{App: "album", Source: "eks", Target: "gke", Files: ptr(false)}); p.Files != nil {
		t.Errorf("files: false still moved %+v", p.Files)
	}
	got, err := f.cat.RegistryBuckets(t.Context())
	if err != nil || len(got) != 2 {
		t.Fatalf("RegistryBuckets = %v, %v", got, err)
	}
}

func TestResourcesOfFileAndPairedApps(t *testing.T) {
	f := newRegistryFixture(t)
	for _, a := range f.cat.List() {
		if !a.Resources.Postgres || !a.Resources.S3 {
			t.Errorf("file app %s: resources %+v, want postgres and s3 (it has buckets)", a.Name, a.Resources)
		}
	}
	f.register("moimroom-mig", "pw")
	a, ok, err := f.cat.Lookup(t.Context(), "moimroom-mig")
	if err != nil || !ok || a.Resources != (apiv1.AppResources{Postgres: true}) || a.Environments[0].CnpgCluster == "" {
		t.Fatalf("paired app %+v %v %v", a, ok, err)
	}
}
