package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/cnpg"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/deploy"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/gateway"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/runs"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

// Adding and removing an environment of a registered app (ADR 0037).

// fakeGatewayApps is GatewayApps over a list; err fails it.
type fakeGatewayApps struct {
	mu   sync.Mutex
	apps []gateway.AppInfo
	err  error
}

func (f *fakeGatewayApps) Apps(context.Context) (gateway.AppList, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return gateway.AppList{Apps: slices.Clone(f.apps)}, f.err
}

func (f *fakeGatewayApps) set(apps ...gateway.AppInfo) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.apps = apps
}

// fakeClusters answers DBCluster from a map by environment.
type fakeClusters struct {
	mu     sync.Mutex
	status map[string]*cnpg.Status
	err    map[string]error
	asked  []string
}

func (f *fakeClusters) read(_ context.Context, env, namespace, name string) (*cnpg.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, env+":"+namespace+"/"+name)
	if err := f.err[env]; err != nil {
		return nil, err
	}
	st, ok := f.status[env]
	if !ok {
		return nil, fmt.Errorf("%s/%s: %w", namespace, name, cnpg.ErrNotFound)
	}
	c := *st
	return &c, nil
}

// envFixture is a harness with shop registered in gke while eks is its
// database pair's peer, its first deployment healthy.
type envFixture struct {
	*harness
	dep      *fakeDeployer
	rm       *fakeRemover
	routes   *fakeDBRoutes
	gw       *fakeGatewayApps
	clusters *fakeClusters
	sealing  map[string]bool
	first    store.Deployment
	// residue is what is left in a namespace of an environment, by
	// "env/namespace"; residueErr fails the read.
	residueMu  sync.Mutex
	residue    map[string]string
	residueErr error
}

func (f *envFixture) readResidue(_ context.Context, env, namespace string) (string, error) {
	f.residueMu.Lock()
	defer f.residueMu.Unlock()
	return f.residue[env+"/"+namespace], f.residueErr
}

func newEnvFixture(t *testing.T, opts ...option) *envFixture {
	t.Helper()
	f := &envFixture{dep: &fakeDeployer{}, routes: &fakeDBRoutes{}, gw: &fakeGatewayApps{}, clusters: &fakeClusters{status: map[string]*cnpg.Status{}, err: map[string]error{}},
		sealing: map[string]bool{"gke": true, "eks": true, "onprem": true}}
	f.rm = &fakeRemover{}
	base := []option{withApps(""), withGitHub(shopGitHub(), f.dep), withSecrets(testCipher(t)), withAppDBTopology(), withRemover(f.rm), withDBRoutes(f.routes),
		func(_ *harness, _ *runs.Config, d *Deps) {
			d.Gateway = f.gw
			d.DBCluster = f.clusters.read
			d.SealingConfigured = func(env string) bool { return f.sealing[env] }
			d.WorkloadResidue = f.readResidue
		}}
	f.harness = newHarness(t, append(base, opts...)...)
	// What RemoveEnvironment does to the store.
	f.rm.removeEnv = func(app, env string) error {
		a, err := f.store.GetApp(context.Background(), app)
		if err != nil {
			return err
		}
		return f.store.SetAppEnvironments(context.Background(), app, a.Environments, slices.DeleteFunc(slices.Clone(a.Environments), func(e string) bool { return e == env }))
	}
	return f
}

// register registers shop in envs and finishes its first deployment
// healthy.
func (f *envFixture) register(envs ...string) {
	f.t.Helper()
	code, b := f.do("POST", "/v1/apps", opToken, map[string]any{"repo": shopRepo, "environments": envs})
	if code != 201 {
		f.t.Fatalf("register: %d %s", code, b)
	}
	f.first = f.finish(decode[apiv1.AppCreated](f.t, b).Deployment.Id, store.DeployHealthy, nil)
}

// finish finishes a deployment with status, the images of shop and the
// app's spec, and envStatus by environment (healthy for a healthy
// deployment, failed otherwise, where not given).
func (f *envFixture) finish(id, status string, envStatus map[string]string) store.Deployment {
	f.t.Helper()
	ctx := context.Background()
	d, err := f.store.GetDeployment(ctx, id)
	if err != nil {
		f.t.Fatal(err)
	}
	a, err := f.store.GetApp(ctx, d.App)
	if err != nil {
		f.t.Fatal(err)
	}
	now := storeTime()
	d.Status, d.FinishedAt = status, &now
	d.Images = []store.DeploymentImage{{Service: "web", Image: "r/web@sha256:aa"}, {Service: "worker", Image: "r/worker@sha256:bb"}}
	d.Spec = a.Spec
	for i := range d.Environments {
		e := &d.Environments[i]
		e.Status = deploy.EnvFailed
		if status == store.DeployHealthy {
			e.Status = deploy.EnvHealthy
		}
		if s, ok := envStatus[e.Env]; ok {
			e.Status = s
		}
	}
	if d.Status == store.DeployFailed {
		d.Error = "Argo CD could not deploy"
	}
	out, err := f.store.UpdateDeployment(ctx, d)
	if err != nil {
		f.t.Fatal(err)
	}
	return out
}

func (f *envFixture) add(env string) (int, []byte) {
	afterNewest()
	return f.do("POST", "/v1/apps/shop/environments", opToken, map[string]any{"environment": env})
}

func (f *envFixture) run(id string, status apiv1.RunStatus, source, target string) {
	f.t.Helper()
	doc, _ := json.Marshal(apiv1.Run{RunId: id, Status: status,
		Source: apiv1.PlanEndpoint{Env: source, Namespace: "shop", Cluster: "shop-db"}, Target: apiv1.PlanEndpoint{Env: target, Namespace: "shop", Cluster: "shop-db"}})
	now := time.Now().UTC()
	if err := f.store.CreateRun(context.Background(), store.Run{RunID: id, PlanRef: "pln_" + id, SourceEnv: source, TargetEnv: target, Status: string(status), Doc: doc, CreatedAt: now, UpdatedAt: now}); err != nil {
		f.t.Fatal(err)
	}
}

// endedRun stores a run that ended with status, shaped like the halted run
// of moimroom-mig on the live server: phase done and endedAt set.
func (f *envFixture) endedRun(id string, status apiv1.RunStatus, source, target string) {
	f.t.Helper()
	started := time.Now().UTC().Add(-time.Hour)
	ended := started.Add(2 * time.Minute)
	reason := "the target was asked to promote, but the source is still primary"
	doc, _ := json.Marshal(apiv1.Run{RunId: id, PlanRef: "pln_" + id, Status: status, Phase: apiv1.RunPhaseDone, StatusReason: &reason,
		StartedAt: started, UpdatedAt: ended, EndedAt: &ended,
		Source: apiv1.PlanEndpoint{Env: source, Namespace: "shop", Cluster: "shop-db"}, Target: apiv1.PlanEndpoint{Env: target, Namespace: "shop", Cluster: "shop-db"}})
	if err := f.store.CreateRun(context.Background(), store.Run{RunID: id, PlanRef: "pln_" + id, SourceEnv: source, TargetEnv: target, Status: string(status), Doc: doc, CreatedAt: started, UpdatedAt: ended}); err != nil {
		f.t.Fatal(err)
	}
}

// TestEndedRunsAreNotInProgress: a run that ended halted (phase done,
// endedAt set, like moimroom-mig's old incident run on the live server),
// or was interrupted by a restart, holds no cluster, so POST
// /v1/migrations would start another run; adding and removing an
// environment are not refused for it either. A run document that cannot
// be read is passed over.
func TestEndedRunsAreNotInProgress(t *testing.T) {
	for name, prep := range map[string]func(f *envFixture){
		"halted and ended": func(f *envFixture) {
			f.endedRun("moimroom-mig-eks-to-gke-20261107-021354", apiv1.RunStatusHalted, "gke", "onprem")
		},
		"interrupted": func(f *envFixture) { f.run("run_1", apiv1.RunStatusInterrupted, "gke", "onprem") },
		"unreadable": func(f *envFixture) {
			now := time.Now().UTC()
			if err := f.store.CreateRun(context.Background(), store.Run{RunID: "run_bad", PlanRef: "pln_bad", SourceEnv: "gke", TargetEnv: "onprem", Status: "running", Doc: []byte(`{"runId":"run_bad","source":"gke"}`), CreatedAt: now, UpdatedAt: now}); err != nil {
				f.t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newEnvFixture(t)
			f.register("gke")
			prep(f)
			code, b := f.add("eks")
			if code != 202 {
				t.Fatalf("add: %d %s", code, b)
			}
			f.finish(decode[apiv1.AppEnvironmentAdded](t, b).Deployment.Id, store.DeployHealthy, nil)
			if code, b := f.do("DELETE", "/v1/apps/shop/environments/eks", opToken, nil); code != 202 {
				t.Fatalf("remove: %d %s", code, b)
			}
		})
	}
}

func TestAddAppEnvironment(t *testing.T) {
	f := newEnvFixture(t)
	f.register("gke")

	code, b := f.add("eks")
	if code != 202 {
		t.Fatalf("add: %d %s", code, b)
	}
	out := decode[apiv1.AppEnvironmentAdded](t, b)
	if !slices.Equal(out.App.Deploy.Environments, []string{"gke", "eks"}) {
		t.Fatalf("environments %v", out.App.Deploy.Environments)
	}
	if tp := out.App.Deploy.DbTopology; tp == nil || tp.Primary != "gke" || tp.Replica != "eks" || !tp.ReplicaPresent {
		t.Fatalf("dbTopology %+v", tp)
	}
	d := out.Deployment
	if d.Trigger != apiv1.DeploymentTriggerEnvironment || d.Commit != f.first.Commit || len(d.Images) != 2 || len(d.Environments) != 2 ||
		d.Environments[0].Env != "gke" || d.Environments[1].Env != "eks" || d.Status != apiv1.DeploymentStatusQueued {
		t.Fatalf("deployment %+v", d)
	}
	if ids := f.dep.enqueued(); !slices.Contains(ids, d.Id) {
		t.Fatalf("enqueued %v", ids)
	}
	if a, _ := f.store.GetApp(t.Context(), "shop"); !slices.Equal(a.Environments, []string{"gke", "eks"}) {
		t.Fatalf("stored environments %v", a.Environments)
	}
	if !slices.Equal(f.routes.changed, []string{"shop"}) {
		t.Fatalf("routes changed %v", f.routes.changed)
	}

	// Sent again while it runs, after it failed, and once it is healthy.
	if code, b := f.add("eks"); code != 409 || errCode(t, b) != "deployment_in_progress" {
		t.Fatalf("again while deploying: %d %s", code, b)
	}
	f.finish(d.Id, store.DeployFailed, map[string]string{"gke": deploy.EnvHealthy})
	code, b = f.add("eks")
	if code != 202 {
		t.Fatalf("again after a failure: %d %s", code, b)
	}
	retry := decode[apiv1.AppEnvironmentAdded](t, b).Deployment
	if retry.Id == d.Id || retry.Trigger != apiv1.DeploymentTriggerEnvironment || len(retry.Environments) != 2 {
		t.Fatalf("retry %+v", retry)
	}
	if a, _ := f.store.GetApp(t.Context(), "shop"); !slices.Equal(a.Environments, []string{"gke", "eks"}) {
		t.Fatalf("stored environments after the retry %v", a.Environments)
	}
	f.finish(retry.Id, store.DeployHealthy, nil)
	if code, b := f.add("eks"); code != 409 || errCode(t, b) != "environment_exists" {
		t.Fatalf("again once healthy: %d %s", code, b)
	}
}

func TestAddAppEnvironmentWithoutPostgres(t *testing.T) {
	f := newEnvFixture(t, withoutPostgres())
	f.register("gke")
	code, b := f.add("onprem") // no pair needed, any environment
	if code != 202 {
		t.Fatalf("add: %d %s", code, b)
	}
	out := decode[apiv1.AppEnvironmentAdded](t, b)
	if !slices.Equal(out.App.Deploy.Environments, []string{"gke", "onprem"}) || out.App.Deploy.DbTopology != nil {
		t.Fatalf("app %+v", out.App.Deploy)
	}
	if len(f.routes.changed) != 0 {
		t.Fatalf("routes changed for an app without a pair: %v", f.routes.changed)
	}
}

func TestAddAppEnvironmentErrors(t *testing.T) {
	type tc struct {
		prep   func(f *envFixture)
		path   string
		body   any
		status int
		code   string
	}
	for name, c := range map[string]tc{
		"no environment":  {body: map[string]any{"environment": ""}, status: 400, code: "invalid_request"},
		"unknown app":     {path: "/v1/apps/nope/environments", status: 404, code: "app_not_found"},
		"unknown env":     {body: map[string]any{"environment": "aks"}, status: 404, code: "env_not_found"},
		"file app":        {path: "/v1/apps/moimroom/environments", status: 409, code: "app_not_registered"},
		"already there":   {body: map[string]any{"environment": "gke"}, status: 409, code: "environment_exists"},
		"deploying":       {prep: func(f *envFixture) { f.redeployQueued() }, status: 409, code: "deployment_in_progress"},
		"run in progress": {prep: func(f *envFixture) { f.run("run_1", apiv1.RunStatusAwaitingSwitch, "gke", "onprem") }, status: 409, code: "migration_in_progress"},
		"writes held": {prep: func(f *envFixture) {
			f.gw.set(gateway.AppInfo{Name: "shop", Backends: map[string]string{"gke": "http://g"}, WriteBackend: "gke", WritesHeld: true})
		}, status: 409, code: "migration_in_progress"},
		"nothing healthy": {prep: func(f *envFixture) {
			now := storeTime()
			f.first.Status, f.first.FinishedAt = store.DeployFailed, &now
			// a later failed one hides nothing: there is no healthy one at all
			f.reset(store.DeployFailed)
		}, status: 409, code: "nothing_to_redeploy"},
		"not the peer":  {body: map[string]any{"environment": "onprem"}, status: 409, code: "db_pair_unavailable"},
		"sealing":       {prep: func(f *envFixture) { f.sealing["eks"] = false }, status: 503, code: "sealing_not_configured"},
		"agent may not": {status: 403, code: "forbidden"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newEnvFixture(t)
			f.register("gke")
			if c.prep != nil {
				c.prep(f)
			}
			path := cmpOr(c.path, "/v1/apps/shop/environments")
			body := c.body
			if body == nil {
				body = map[string]any{"environment": "eks"}
			}
			token := opToken
			if name == "agent may not" {
				token = agentToken
			}
			code, b := f.do("POST", path, token, body)
			if code != c.status || errCode(t, b) != c.code {
				t.Fatalf("%d %s, want %d %s", code, b, c.status, c.code)
			}
			if a, _ := f.store.GetApp(t.Context(), "shop"); !slices.Equal(a.Environments, []string{"gke"}) {
				t.Fatalf("environments changed to %v", a.Environments)
			}
		})
	}

	t.Run("failed and aborted runs do not hold it", func(t *testing.T) {
		f := newEnvFixture(t)
		f.register("gke")
		f.run("run_1", apiv1.RunStatusFailed, "gke", "onprem")
		f.run("run_2", apiv1.RunStatusAborted, "gke", "onprem")
		f.run("run_3", apiv1.RunStatusRunning, "gke", "eks") // of another app's namespace
		ctx := context.Background()
		r, _ := f.store.GetRun(ctx, "run_3")
		r.Doc = []byte(strings.ReplaceAll(string(r.Doc), `"namespace":"shop"`, `"namespace":"cart"`))
		if err := f.store.UpdateRun(ctx, r); err != nil {
			t.Fatal(err)
		}
		if code, b := f.add("eks"); code != 202 {
			t.Fatalf("%d %s", code, b)
		}
	})

	t.Run("no pair", func(t *testing.T) {
		f := newEnvFixture(t, withAppDBEnvs("gke", "eks", "onprem")) // one env of three templates: no pair
		f.register("gke")
		if code, b := f.add("eks"); code != 409 || errCode(t, b) != "db_pair_unavailable" {
			t.Fatalf("%d %s", code, b)
		}
	})

	t.Run("storage", func(t *testing.T) {
		f := newEnvFixture(t, withS3Spec())
		f.register("gke")
		if code, b := f.add("eks"); code != 503 || errCode(t, b) != "storage_not_configured" {
			t.Fatalf("%d %s", code, b)
		}
	})
}

// redeployQueued stores a queued redeploy of the first deployment.
func (f *envFixture) redeployQueued() {
	f.t.Helper()
	afterNewest()
	code, b := f.do("POST", "/v1/apps/shop/deployments", opToken, map[string]any{"redeploy": true})
	if code != 202 {
		f.t.Fatalf("redeploy: %d %s", code, b)
	}
}

// reset finishes the first deployment again, by deleting and storing it
// with status.
func (f *envFixture) reset(status string) {
	f.t.Helper()
	ctx := context.Background()
	a, _ := f.store.GetApp(ctx, "shop")
	if err := f.store.DeleteApp(ctx, "shop"); err != nil {
		f.t.Fatal(err)
	}
	if err := f.store.CreateApp(ctx, a); err != nil {
		f.t.Fatal(err)
	}
	d := f.first
	d.Status = status
	if err := f.store.CreateDeployment(ctx, d); err != nil {
		f.t.Fatal(err)
	}
}

// afterNewest waits until the clock has moved past the newest deployment,
// so the next one sorts after it: the store keeps microseconds, and the
// clock of some systems moves in coarser steps.
func afterNewest() { time.Sleep(2 * time.Millisecond) }

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// withS3Spec serves shop's platform.yaml with an s3 resource instead of
// its postgres one.
func withS3Spec() option {
	return func(_ *harness, _ *runs.Config, d *Deps) {
		gh := newFakeGitHub()
		gh.repo(shopRepo, "main", headSHA, strings.Replace(shopSpec, "{name: db, type: postgres}", "{name: photos, type: s3}", 1), "Dockerfile", "worker/Dockerfile")
		d.GitHub = gh
		d.StorageConfigured = func(env string) bool { return env == "gke" }
	}
}

// addedFixture is an envFixture where eks was added and deployed.
func addedFixture(t *testing.T, opts ...option) *envFixture {
	t.Helper()
	f := newEnvFixture(t, opts...)
	f.register("gke")
	code, b := f.add("eks")
	if code != 202 {
		t.Fatalf("add: %d %s", code, b)
	}
	f.finish(decode[apiv1.AppEnvironmentAdded](t, b).Deployment.Id, store.DeployHealthy, nil)
	f.routes.changed = nil
	return f
}

func TestRemoveAppEnvironment(t *testing.T) {
	f := addedFixture(t)
	code, b := f.do("DELETE", "/v1/apps/shop/environments/eks", opToken, nil)
	if code != 202 {
		t.Fatalf("remove: %d %s", code, b)
	}
	out := decode[apiv1.AppEnvironmentRemoved](t, b)
	if !slices.Equal(out.App.Deploy.Environments, []string{"gke"}) || out.App.Deploy.DbTopology == nil || out.App.Deploy.DbTopology.ReplicaPresent {
		t.Fatalf("app %+v", out.App.Deploy)
	}
	if d := out.Deployment; d == nil || d.Trigger != apiv1.DeploymentTriggerEnvironment || len(d.Environments) != 1 || d.Environments[0].Env != "gke" {
		t.Fatalf("deployment %+v", out.Deployment)
	}
	if !slices.Equal(f.rm.removed, []string{"shop/eks"}) || !slices.Equal(f.routes.changed, []string{"shop"}) {
		t.Fatalf("removed %v, routes changed %v", f.rm.removed, f.routes.changed)
	}
	// The pair stays, so eks can be added again.
	if row, err := f.store.GetAppDatabase(t.Context(), "shop"); err != nil || row.PeerEnv != "eks" {
		t.Fatalf("pair %+v, %v", row, err)
	}
	f.finish(out.Deployment.Id, store.DeployHealthy, nil)
	if code, b := f.add("eks"); code != 202 {
		t.Fatalf("add again: %d %s", code, b)
	}
}

func TestRemoveAppEnvironmentErrors(t *testing.T) {
	for name, c := range map[string]struct {
		prep   func(f *envFixture)
		path   string
		status int
		code   string
	}{
		"unknown app":   {path: "/v1/apps/nope/environments/eks", status: 404, code: "app_not_found"},
		"not the app's": {path: "/v1/apps/shop/environments/onprem", status: 404, code: "env_not_found"},
		"file app":      {path: "/v1/apps/moimroom/environments/eks", status: 409, code: "app_not_registered"},
		"first":         {path: "/v1/apps/shop/environments/gke", status: 409, code: "primary_environment"},
		"migrated there": {prep: func(f *envFixture) { f.run("run_1", apiv1.RunStatusSucceeded, "gke", "eks") },
			status: 409, code: "write_backend"},
		"rolled back from there": {prep: func(f *envFixture) { f.run("run_1", apiv1.RunStatusRolledBack, "eks", "gke") },
			status: 409, code: "write_backend"},
		"deploying": {prep: func(f *envFixture) { f.redeployQueued() }, status: 409, code: "deployment_in_progress"},
		"run of another environment": {prep: func(f *envFixture) { f.run("run_1", apiv1.RunStatusRunning, "gke", "onprem") },
			status: 409, code: "migration_in_progress"},
		"writes held": {prep: func(f *envFixture) {
			f.gw.set(gateway.AppInfo{Name: "shop", Backends: map[string]string{"gke": "http://g", "eks": "http://e"}, WriteBackend: "gke", WritesHeld: true})
		}, status: 409, code: "migration_in_progress"},
		"gateway sends there": {prep: func(f *envFixture) {
			f.rm.err = fmt.Errorf("%w: the traffic gateway sends the requests of shop to eks", deploy.ErrEnvironmentInUse)
		}, status: 409, code: "write_backend"},
		"held under the lock": {prep: func(f *envFixture) { f.rm.err = fmt.Errorf("%w: held", deploy.ErrWritesHeld) }, status: 409, code: "migration_in_progress"},
		"deploying under the lock": {prep: func(f *envFixture) { f.rm.err = fmt.Errorf("%w: dep_1", deploy.ErrDeploymentInProgress) },
			status: 409, code: "deployment_in_progress"},
		"changed under the lock": {prep: func(f *envFixture) { f.rm.err = fmt.Errorf("%w: branches", deploy.ErrAppChanged) }, status: 409, code: "app_changed"},
		"environments changed when stored": {prep: func(f *envFixture) { f.rm.err = fmt.Errorf("store: %w", store.ErrConflict) },
			status: 409, code: "app_changed"},
		"gone under the lock": {prep: func(f *envFixture) { f.rm.err = fmt.Errorf("%w: eks", deploy.ErrNotInEnvironment) }, status: 404, code: "env_not_found"},
		"first under the lock": {prep: func(f *envFixture) { f.rm.err = fmt.Errorf("%w: eks", deploy.ErrPrimaryEnvironment) },
			status: 409, code: "primary_environment"},
		"gitops":  {prep: func(f *envFixture) { f.rm.err = fmt.Errorf("%w: down", deploy.ErrGitOps) }, status: 502, code: "github_unreachable"},
		"gateway": {prep: func(f *envFixture) { f.rm.err = fmt.Errorf("%w: down", deploy.ErrGateway) }, status: 502, code: "gateway_unreachable"},
		"other":   {prep: func(f *envFixture) { f.rm.err = errors.New("boom") }, status: 500, code: "internal"},
	} {
		t.Run(name, func(t *testing.T) {
			f := addedFixture(t)
			if c.prep != nil {
				c.prep(f)
			}
			code, b := f.do("DELETE", cmpOr(c.path, "/v1/apps/shop/environments/eks"), opToken, nil)
			if code != c.status || errCode(t, b) != c.code {
				t.Fatalf("%d %s, want %d %s", code, b, c.status, c.code)
			}
			if a, _ := f.store.GetApp(t.Context(), "shop"); !slices.Equal(a.Environments, []string{"gke", "eks"}) {
				t.Fatalf("environments changed to %v", a.Environments)
			}
		})
	}

	t.Run("no deployer", func(t *testing.T) {
		f := addedFixture(t)
		f.api.d.Remover = nil
		if code, b := f.do("DELETE", "/v1/apps/shop/environments/eks", opToken, nil); code != 503 || errCode(t, b) != "github_not_configured" {
			t.Fatalf("%d %s", code, b)
		}
	})
}

// healthyPair is the two Clusters of shop's pair right after eks was
// added: gke primary, eks following it.
func healthyPair() map[string]*cnpg.Status {
	return map[string]*cnpg.Status{
		"gke": {Phase: cnpg.HealthyPhase, Instances: 1, ReadyInstances: 1, Self: "shop-db-gke", ReplicaPrimary: "shop-db-gke", ReplicaSource: "shop-db-eks"},
		"eks": {Phase: cnpg.HealthyPhase, Instances: 1, ReadyInstances: 1, Self: "shop-db-eks", ReplicaPrimary: "shop-db-gke", ReplicaSource: "shop-db-gke"},
	}
}

func (f *envFixture) readiness() apiv1.AppDBReadiness {
	f.t.Helper()
	code, b := f.do("GET", "/v1/apps/shop", viewerToken, nil)
	if code != 200 {
		f.t.Fatalf("get: %d %s", code, b)
	}
	tp := decode[apiv1.App](f.t, b).Deploy.DbTopology
	if tp == nil || tp.Readiness == nil {
		f.t.Fatalf("no readiness: %s", b)
	}
	return *tp.Readiness
}

func TestDBReadiness(t *testing.T) {
	bothBackends := gateway.AppInfo{Name: "shop", Backends: map[string]string{"gke": "http://g", "eks": "http://e"}, WriteBackend: "gke"}

	t.Run("ready", func(t *testing.T) {
		f := addedFixture(t)
		f.gw.set(bothBackends)
		f.clusters.status = healthyPair()
		r := f.readiness()
		if !r.Ready || r.Reason != "ready" || len(r.Environments) != 2 {
			t.Fatalf("%+v", r)
		}
		g, e := r.Environments[0], r.Environments[1]
		if g.Env != "gke" || g.Role != apiv1.AppDBReadinessEnvironmentRolePrimary || !g.Healthy || !g.Deployed || !g.GatewayBackend || !g.InApp ||
			e.Env != "eks" || e.Role != apiv1.AppDBReadinessEnvironmentRoleReplica || !e.Healthy || e.Phase == nil || *e.Phase != cnpg.HealthyPhase {
			t.Fatalf("environments %+v", r.Environments)
		}
		if want := []string{"eks:shop/shop-db", "gke:shop/shop-db"}; !slices.Equal(slices.Sorted(slices.Values(f.clusters.asked)), want) {
			t.Fatalf("read %v, want %v", f.clusters.asked, want)
		}
		// GET /v1/apps does not read the clusters.
		f.clusters.asked = nil
		if code, b := f.do("GET", "/v1/apps", viewerToken, nil); code != 200 || strings.Contains(string(b), "readiness") || len(f.clusters.asked) != 0 {
			t.Fatalf("list: %d %s, read %v", code, b, f.clusters.asked)
		}
	})

	t.Run("after a migration", func(t *testing.T) {
		f := addedFixture(t)
		migrated := bothBackends
		migrated.WriteBackend = "eks"
		f.gw.set(migrated)
		st := healthyPair()
		st["gke"].ReplicaPrimary, st["gke"].ReplicaSource = "shop-db-eks", "shop-db-eks"
		st["eks"].ReplicaPrimary = "shop-db-eks"
		f.clusters.status = st
		if r := f.readiness(); !r.Ready || r.Environments[0].Role != apiv1.AppDBReadinessEnvironmentRoleReplica {
			t.Fatalf("%+v", r)
		}
	})

	for name, c := range map[string]struct {
		prep   func(f *envFixture)
		reason string
	}{
		"gateway missing eks": {func(f *envFixture) {
			f.gw.set(gateway.AppInfo{Name: "shop", Backends: map[string]string{"gke": "http://g"}, WriteBackend: "gke"})
		}, "gateway_backend_missing"},
		"gateway down":     {func(f *envFixture) { f.gw.err = errors.New("timeout") }, "gateway_unreachable"},
		"eks unreadable":   {func(f *envFixture) { f.clusters.err["eks"] = errors.New("forbidden") }, "cluster_unreadable"},
		"eks not there":    {func(f *envFixture) { delete(f.clusters.status, "eks") }, "cluster_unreadable"},
		"eks not healthy":  {func(f *envFixture) { f.clusters.status["eks"].ReadyInstances = 0 }, "cluster_not_healthy"},
		"two primaries":    {func(f *envFixture) { f.clusters.status["eks"].ReplicaPrimary = "shop-db-eks" }, "topology_mismatch"},
		"wrong source":     {func(f *envFixture) { f.clusters.status["gke"].ReplicaSource = "shop-db-onprem" }, "topology_mismatch"},
		"eks degraded":     {func(f *envFixture) { f.failEks(deploy.EnvDegraded) }, "app_degraded"},
		"eks failed":       {func(f *envFixture) { f.failEks(deploy.EnvFailed) }, "deploy_failed"},
		"deploying again":  {func(f *envFixture) { f.redeployQueued() }, "deploy_pending"}, // not while a deployment runs
		"no cluster reads": {func(f *envFixture) { f.api.d.DBCluster = nil }, "cluster_unreadable"},
	} {
		t.Run(name, func(t *testing.T) {
			f := addedFixture(t)
			f.gw.set(bothBackends)
			f.clusters.status = healthyPair()
			c.prep(f)
			r := f.readiness()
			if r.Reason != c.reason || r.Ready != (c.reason == "ready") || r.Message == "" {
				t.Fatalf("%+v, want %s", r, c.reason)
			}
			if c.reason == "app_degraded" && !strings.Contains(r.Message, "cold mode") {
				t.Fatalf("message %q", r.Message)
			}
		})
	}

	t.Run("peer not added", func(t *testing.T) {
		f := newEnvFixture(t)
		f.register("gke")
		f.clusters.status = healthyPair()
		r := f.readiness()
		if r.Ready || r.Reason != "replica_missing" || r.Environments[1].InApp || r.Environments[1].Role != apiv1.AppDBReadinessEnvironmentRoleUnknown {
			t.Fatalf("%+v", r)
		}
		if slices.ContainsFunc(f.clusters.asked, func(s string) bool { return strings.HasPrefix(s, "eks:") }) {
			t.Fatalf("read eks, which is not the app's: %v", f.clusters.asked)
		}
	})

	t.Run("peer deploying", func(t *testing.T) {
		f := newEnvFixture(t)
		f.register("gke")
		if code, b := f.add("eks"); code != 202 {
			t.Fatalf("add: %d %s", code, b)
		}
		f.clusters.status = healthyPair()
		if r := f.readiness(); r.Reason != "deploy_pending" || r.Environments[1].Deployed {
			t.Fatalf("%+v", r)
		}
	})
}

// failEks stores a newer deployment of shop to gke and eks that failed with
// eks in envStatus.
func (f *envFixture) failEks(envStatus string) {
	f.t.Helper()
	afterNewest()
	code, b := f.do("POST", "/v1/apps/shop/deployments", opToken, map[string]any{"redeploy": true})
	if code != 202 {
		f.t.Fatalf("redeploy: %d %s", code, b)
	}
	f.finish(decode[apiv1.Deployment](f.t, b).Id, store.DeployFailed, map[string]string{"gke": deploy.EnvHealthy, "eks": envStatus})
}

// TestAddedEnvironmentGoesLast: the environment the app was registered in
// stays first even when the added one comes before it in
// PLATFORM_ENVS_FILE, so the added one can be removed and the first not.
func TestAddedEnvironmentGoesLast(t *testing.T) {
	f := newEnvFixture(t)
	f.register("eks") // gke comes first in the environments file
	code, b := f.add("gke")
	if code != 202 {
		t.Fatalf("add: %d %s", code, b)
	}
	out := decode[apiv1.AppEnvironmentAdded](t, b)
	if !slices.Equal(out.App.Deploy.Environments, []string{"eks", "gke"}) {
		t.Fatalf("environments %v", out.App.Deploy.Environments)
	}
	if tp := out.App.Deploy.DbTopology; tp == nil || tp.Primary != "eks" || tp.Replica != "gke" || !tp.ReplicaPresent {
		t.Fatalf("dbTopology %+v", tp)
	}
	f.finish(out.Deployment.Id, store.DeployHealthy, nil)
	if code, b := f.do("DELETE", "/v1/apps/shop/environments/eks", opToken, nil); code != 409 || errCode(t, b) != "primary_environment" {
		t.Fatalf("remove the first: %d %s", code, b)
	}
	if code, b := f.do("DELETE", "/v1/apps/shop/environments/gke", opToken, nil); code != 202 {
		t.Fatalf("remove the added one: %d %s", code, b)
	}
	if a, _ := f.store.GetApp(t.Context(), "shop"); !slices.Equal(a.Environments, []string{"eks"}) {
		t.Fatalf("environments %v", a.Environments)
	}
}
