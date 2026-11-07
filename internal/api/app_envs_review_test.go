package api

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/cnpg"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/deploy"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/gateway"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/runs"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/appspec"
)

// removedFixture is addedFixture after eks was removed again, the
// deployment to gke that followed finished healthy.
func removedFixture(t *testing.T) *envFixture {
	f := addedFixture(t)
	code, b := f.do("DELETE", "/v1/apps/shop/environments/eks", opToken, nil)
	if code != 202 {
		t.Fatalf("remove: %d %s", code, b)
	}
	f.finish(decode[apiv1.AppEnvironmentRemoved](t, b).Deployment.Id, store.DeployHealthy, nil)
	return f
}

// TestAddEnvironmentBackWaitsForCleanup: an environment removed before is
// added back only once Argo CD deleted its Application and nothing is left
// in the namespace there (ADR 0043).
func TestAddEnvironmentBackWaitsForCleanup(t *testing.T) {
	for name, c := range map[string]struct {
		prep   func(f *envFixture)
		status int
		code   string
	}{
		"application still there": {func(f *envFixture) { f.rm.applications = map[string]bool{"shop-eks": true} }, 409, "environment_cleanup_pending"},
		"namespace not empty":     {func(f *envFixture) { f.residue = map[string]string{"eks/shop": "Service shop-db-ts"} }, 409, "environment_cleanup_pending"},
		"argo unreadable":         {func(f *envFixture) { f.rm.applicationsErr = errors.New("timeout") }, 502, "environment_cleanup_unknown"},
		"cluster unreadable":      {func(f *envFixture) { f.residueErr = errors.New("forbidden") }, 502, "environment_cleanup_unknown"},
		"no cluster reads":        {func(f *envFixture) { f.api.d.WorkloadResidue = nil }, 503, "environment_cleanup_unknown"},
	} {
		t.Run(name, func(t *testing.T) {
			f := removedFixture(t)
			c.prep(f)
			code, b := f.add("eks")
			if code != c.status || errCode(t, b) != c.code {
				t.Fatalf("%d %s, want %d %s", code, b, c.status, c.code)
			}
			if a, _ := f.store.GetApp(t.Context(), "shop"); !slices.Equal(a.Environments, []string{"gke"}) {
				t.Fatalf("environments changed to %v", a.Environments)
			}
		})
	}

	t.Run("gone", func(t *testing.T) {
		f := removedFixture(t)
		if code, b := f.add("eks"); code != 202 {
			t.Fatalf("add back: %d %s", code, b)
		}
	})

	// An app deleted and registered again has none of its old deployments,
	// but what the old registration left in eks is still read.
	t.Run("registered again", func(t *testing.T) {
		for name, prep := range map[string]func(f *envFixture){
			"application": func(f *envFixture) { f.rm.applications = map[string]bool{"shop-eks": true} },
			"namespace":   func(f *envFixture) { f.residue = map[string]string{"eks/shop": "Service shop-db-ts"} },
		} {
			t.Run(name, func(t *testing.T) {
				f := newEnvFixture(t)
				f.register("gke")
				prep(f)
				if code, b := f.add("eks"); code != 409 || errCode(t, b) != "environment_cleanup_pending" {
					t.Fatalf("%d %s, want 409 environment_cleanup_pending", code, b)
				}
			})
		}
	})

	t.Run("never deployed there", func(t *testing.T) {
		f := newEnvFixture(t)
		f.register("gke")
		f.api.d.WorkloadResidue = nil // nothing to wait for, so nothing is read
		f.rm.applicationsErr = errors.New("not asked")
		if code, b := f.add("eks"); code != 202 {
			t.Fatalf("first add: %d %s", code, b)
		}
	})
}

// failsAfterRemoval is a store whose reads of the app and its deployments
// fail once removed is set: what the answer to a removal reads after the
// removal itself.
type failsAfterRemoval struct {
	store.Store
	removed *atomic.Bool
}

func (s failsAfterRemoval) GetApp(ctx context.Context, name string) (store.App, error) {
	if s.removed.Load() {
		return store.App{}, errors.New("the database is down")
	}
	return s.Store.GetApp(ctx, name)
}

func (s failsAfterRemoval) ListInstanceDeployments(ctx context.Context, instance string, limit int) ([]store.Deployment, error) {
	if s.removed.Load() {
		return nil, errors.New("the database is down")
	}
	return s.Store.ListInstanceDeployments(ctx, instance, limit)
}

// TestRemoveAppEnvironmentAnswersAfterTheRemoval: once the environment is
// removed, failing reads for the answer still answer 202, with less.
func TestRemoveAppEnvironmentAnswersAfterTheRemoval(t *testing.T) {
	var removed atomic.Bool
	f := addedFixture(t, func(_ *harness, _ *runs.Config, d *Deps) { d.Store = failsAfterRemoval{d.Store, &removed} })
	remove := f.rm.removeEnv
	f.rm.removeEnv = func(app, env string) error {
		err := remove(app, env)
		removed.Store(true)
		return err
	}
	code, b := f.do("DELETE", "/v1/apps/shop/environments/eks", opToken, nil)
	if code != 202 {
		t.Fatalf("remove: %d %s", code, b)
	}
	out := decode[apiv1.AppEnvironmentRemoved](t, b)
	if out.Deployment != nil || out.App.Name != "shop" || !slices.Equal(out.App.Deploy.Environments, []string{"gke"}) {
		t.Fatalf("answer %s", b)
	}
}

// TestRemoveAppEnvironmentWithFilesOutsideTheGateway: an instance with an
// s3 resource and no public service that went healthy to the environment
// may have written files there that no gateway route tells of.
func TestRemoveAppEnvironmentWithFilesOutsideTheGateway(t *testing.T) {
	spec := func(public bool, res ...appspec.Resource) []byte {
		b, _ := json.Marshal(appspec.Spec{Name: "shop", Services: []appspec.Service{{Name: "web", Port: 8080, Public: public}}, Resources: res})
		return b
	}
	s3 := appspec.Resource{Name: "files", Type: appspec.TypeS3}
	pg := appspec.Resource{Name: "db", Type: appspec.TypePostgres}
	dep := func(id string, spec []byte, eks string) store.Deployment {
		return store.Deployment{ID: id, Instance: "shop", Spec: spec, Environments: []store.DeploymentEnvironment{{Env: "gke", Status: deploy.EnvHealthy}, {Env: "eks", Status: eks}}}
	}
	app := store.App{Name: "shop", Spec: spec(false, s3)}
	for name, c := range map[string]struct {
		deps []store.Deployment
		want string
	}{
		"s3, not public, healthy there":   {[]store.Deployment{dep("d1", spec(false, s3), deploy.EnvHealthy)}, "d1"},
		"no spec recorded: the app's":     {[]store.Deployment{dep("d1", nil, deploy.EnvHealthy)}, "d1"},
		"public":                          {[]store.Deployment{dep("d1", spec(true, s3), deploy.EnvHealthy)}, ""},
		"no s3":                           {[]store.Deployment{dep("d1", spec(false, pg), deploy.EnvHealthy)}, ""},
		"never healthy there":             {[]store.Deployment{dep("d1", spec(false, s3), deploy.EnvDegraded), dep("d2", spec(false, s3), deploy.EnvPending)}, ""},
		"healthy there once, then public": {[]store.Deployment{dep("d2", spec(true, s3), deploy.EnvHealthy), dep("d1", spec(false, s3), deploy.EnvHealthy)}, "d1"},
	} {
		got := filesWithoutGateway(app, c.deps, "eks")
		if (got == nil && c.want != "") || (got != nil && got.ID != c.want) {
			t.Errorf("%s: %+v, want %q", name, got, c.want)
		}
	}
}

// TestDBReadinessMigrationAndWriteBackend: a migration in progress, held
// writes and a gateway writing to the replica's side make the pair not
// ready.
func TestDBReadinessMigrationAndWriteBackend(t *testing.T) {
	both := gateway.AppInfo{Name: "shop", Backends: map[string]string{"gke": "http://g", "eks": "http://e"}, WriteBackend: "gke"}
	for name, c := range map[string]struct {
		prep   func(f *envFixture)
		reason string
	}{
		"run running":  {func(f *envFixture) { f.run("run_1", apiv1.RunStatusRunning, "gke", "eks") }, "migration_in_progress"},
		"run awaiting": {func(f *envFixture) { f.run("run_1", apiv1.RunStatusAwaitingSwitch, "gke", "eks") }, "migration_in_progress"},
		// The live moimroom-mig's old incident run: halted, phase done, endedAt.
		"run halted and ended": {func(f *envFixture) { f.endedRun("run_1", apiv1.RunStatusHalted, "eks", "gke") }, "ready"},
		"run interrupted":      {func(f *envFixture) { f.run("run_1", apiv1.RunStatusInterrupted, "gke", "eks") }, "ready"},
		"run unreadable": {func(f *envFixture) {
			now := time.Now().UTC()
			if err := f.store.CreateRun(context.Background(), store.Run{RunID: "run_bad", PlanRef: "pln_bad", SourceEnv: "gke", TargetEnv: "eks", Status: "running", Doc: []byte(`{"runId":"run_bad","source":"gke"}`), CreatedAt: now, UpdatedAt: now}); err != nil {
				f.t.Fatal(err)
			}
		}, "ready"},
		"run finished": {func(f *envFixture) { f.run("run_1", apiv1.RunStatusFailed, "gke", "eks") }, "ready"},
		"writes held": {func(f *envFixture) {
			held := both
			held.WritesHeld = true
			f.gw.set(held)
		}, "migration_in_progress"},
		"writes to the replica": {func(f *envFixture) {
			wrong := both
			wrong.WriteBackend = "eks"
			f.gw.set(wrong)
		}, "write_backend_mismatch"},
		// A later deployment that failed before it got to eks (a build) says
		// nothing about eks.
		"a later build failed": {func(f *envFixture) {
			afterNewest()
			code, b := f.do("POST", "/v1/apps/shop/deployments", opToken, map[string]any{"redeploy": true})
			if code != 202 {
				f.t.Fatalf("redeploy: %d %s", code, b)
			}
			f.finish(decode[apiv1.Deployment](f.t, b).Id, store.DeployFailed, map[string]string{"gke": deploy.EnvPending, "eks": deploy.EnvPending})
		}, "ready"},
	} {
		t.Run(name, func(t *testing.T) {
			f := addedFixture(t)
			f.gw.set(both)
			f.clusters.status = healthyPair()
			c.prep(f)
			if r := f.readiness(); r.Reason != c.reason || r.Ready != (c.reason == "ready") {
				t.Fatalf("%+v, want %s", r, c.reason)
			}
		})
	}
}

// blockingGateway answers only when its context ends.
type blockingGateway struct{}

func (blockingGateway) Apps(ctx context.Context) (gateway.AppList, error) {
	<-ctx.Done()
	return gateway.AppList{}, ctx.Err()
}

// TestDBReadinessReadsInParallelAndCaches: the gateway and both clusters
// are read at the same time, each within DBClusterTimeout, and an app's
// reads are kept for ReadinessCacheTTL.
func TestDBReadinessReadsInParallelAndCaches(t *testing.T) {
	t.Run("timeouts", func(t *testing.T) {
		old := DBClusterTimeout
		DBClusterTimeout = 300 * time.Millisecond
		t.Cleanup(func() { DBClusterTimeout = old })
		f := addedFixture(t)
		f.api.d.Gateway = blockingGateway{}
		f.api.d.DBCluster = func(ctx context.Context, _, _, _ string) (*cnpg.Status, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		start := time.Now()
		r := f.readiness()
		if took := time.Since(start); took >= 2*DBClusterTimeout {
			t.Fatalf("took %v: the reads ran one after another", took)
		}
		if r.Ready || r.Reason != "gateway_unreachable" {
			t.Fatalf("%+v", r)
		}
	})

	t.Run("cache", func(t *testing.T) {
		f := addedFixture(t)
		f.gw.set(gateway.AppInfo{Name: "shop", Backends: map[string]string{"gke": "http://g", "eks": "http://e"}, WriteBackend: "gke"})
		f.clusters.status = healthyPair()
		if r := f.readiness(); !r.Ready {
			t.Fatalf("%+v", r)
		}
		f.clusters.status["eks"].ReadyInstances = 0
		if r := f.readiness(); !r.Ready || len(f.clusters.asked) != 2 {
			t.Fatalf("second read within the TTL: %+v, read %v", r, f.clusters.asked)
		}
		// The deployments are read every time.
		f.redeployQueued()
		if r := f.readiness(); r.Reason != "deploy_pending" || len(f.clusters.asked) != 2 {
			t.Fatalf("with a deployment queued: %+v, read %v", r, f.clusters.asked)
		}
		old := ReadinessCacheTTL
		ReadinessCacheTTL = 0
		t.Cleanup(func() { ReadinessCacheTTL = old })
		f.readiness()
		if len(f.clusters.asked) != 4 {
			t.Fatalf("after the TTL: read %v", f.clusters.asked)
		}
	})

	// A run that ended after the reads were kept may have moved the
	// primary, so they are read again.
	t.Run("run ended since", func(t *testing.T) {
		f := addedFixture(t)
		f.gw.set(gateway.AppInfo{Name: "shop", Backends: map[string]string{"gke": "http://g", "eks": "http://e"}, WriteBackend: "gke"})
		f.clusters.status = healthyPair()
		f.readiness()
		ended := time.Now().UTC().Add(time.Millisecond)
		time.Sleep(2 * time.Millisecond)
		doc, _ := json.Marshal(apiv1.Run{RunId: "run_1", Status: apiv1.RunStatusSucceeded, Phase: apiv1.RunPhaseDone, EndedAt: &ended,
			Source: apiv1.PlanEndpoint{Env: "gke", Namespace: "shop"}, Target: apiv1.PlanEndpoint{Env: "eks", Namespace: "shop"}})
		if err := f.store.CreateRun(context.Background(), store.Run{RunID: "run_1", PlanRef: "pln_1", SourceEnv: "gke", TargetEnv: "eks", Status: "succeeded", Doc: doc, CreatedAt: ended, UpdatedAt: ended}); err != nil {
			t.Fatal(err)
		}
		f.readiness()
		if len(f.clusters.asked) != 4 {
			t.Fatalf("after a run ended: read %v", f.clusters.asked)
		}
		f.readiness()
		if len(f.clusters.asked) != 4 {
			t.Fatalf("the reads after the run are kept again: read %v", f.clusters.asked)
		}
	})

	// Reads cut short because the client went away are not kept.
	t.Run("request cancelled", func(t *testing.T) {
		f := addedFixture(t)
		f.clusters.status = healthyPair()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		f.api.readinessReads(ctx, "shop", []string{"gke", "eks"}, []bool{true, true}, "shop-db", false, time.Time{})
		f.api.ready.mu.Lock()
		n := len(f.api.ready.reads)
		f.api.ready.mu.Unlock()
		if n != 0 {
			t.Fatalf("kept %d reads of a cancelled request", n)
		}
		f.api.readinessReads(context.Background(), "shop", []string{"gke", "eks"}, []bool{true, true}, "shop-db", false, time.Time{})
		if len(f.api.ready.reads) != 1 {
			t.Fatal("reads of a request that finished are kept")
		}
	})
}

// TestSupersededEnvironmentDeploymentAsksForAnotherAdd: a manual deployment
// to the environments it was asked for (here none named, stored before the
// add with the old list) that supersedes the environment's deployment does
// not take the new environment along. Readiness says deploy_pending and to
// send the add again, which queues the deployment (ADR 0043).
func TestSupersededEnvironmentDeploymentAsksForAnotherAdd(t *testing.T) {
	f := newEnvFixture(t)
	f.register("gke")
	code, b := f.add("eks")
	if code != 202 {
		t.Fatalf("add: %d %s", code, b)
	}
	f.finish(decode[apiv1.AppEnvironmentAdded](t, b).Deployment.Id, store.DeploySuperseded, map[string]string{"gke": deploy.EnvPending, "eks": deploy.EnvPending})
	f.gw.set(gateway.AppInfo{Name: "shop", Backends: map[string]string{"gke": "http://g", "eks": "http://e"}, WriteBackend: "gke"})
	f.clusters.status = healthyPair()
	r := f.readiness()
	if r.Reason != "deploy_pending" || !strings.Contains(r.Message, "POST /v1/apps/shop/environments with eks again") {
		t.Fatalf("%+v", r)
	}
	if code, b := f.add("eks"); code != 202 {
		t.Fatalf("add again: %d %s", code, b)
	}
}

// failsCreateDeployment is a store whose CreateDeployment fails while fail
// is set.
type failsCreateDeployment struct {
	store.Store
	fail *atomic.Bool
}

func (s failsCreateDeployment) CreateDeployment(ctx context.Context, d store.Deployment) error {
	if s.fail.Load() {
		return errors.New("the database is down")
	}
	return s.Store.CreateDeployment(ctx, d)
}

// TestAddEnvironmentDeploymentNotQueued: when the environment is stored
// but its deployment cannot be, the answer says so and that the same
// request queues it, which it then does.
func TestAddEnvironmentDeploymentNotQueued(t *testing.T) {
	var fail atomic.Bool
	f := newEnvFixture(t, func(_ *harness, _ *runs.Config, d *Deps) { d.Store = failsCreateDeployment{d.Store, &fail} })
	f.register("gke")
	fail.Store(true)
	code, b := f.add("eks")
	if code != 500 || errCode(t, b) != "deployment_not_queued" || !strings.Contains(string(b), "send the same request again") {
		t.Fatalf("%d %s, want 500 deployment_not_queued", code, b)
	}
	if a, _ := f.store.GetApp(t.Context(), "shop"); !slices.Equal(a.Environments, []string{"gke", "eks"}) {
		t.Fatalf("environments %v", a.Environments)
	}
	fail.Store(false)
	if code, b := f.add("eks"); code != 202 {
		t.Fatalf("again: %d %s", code, b)
	}
}
