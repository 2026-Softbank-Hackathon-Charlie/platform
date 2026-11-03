package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/deploy"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

const devSHA = "3333333333333333333333333333333333333333"

// instanceGitHub is shopGitHub with more branches of acme/shop: dev,
// feature/x, feature-x and staging hold shop's platform.yaml; nospec has
// none, renamed names another app and broken breaks a rule.
func instanceGitHub() *fakeGitHub {
	gh := shopGitHub()
	for _, b := range []string{"dev", "feature/x", "feature-x", "staging"} {
		gh.repo(shopRepo, b, devSHA, shopSpec, "Dockerfile", "worker/Dockerfile")
	}
	gh.repo(shopRepo, "nospec", "4444444444444444444444444444444444444444", "")
	gh.repo(shopRepo, "renamed", "5555555555555555555555555555555555555555", strings.Replace(shopSpec, "name: shop", "name: store", 1), "Dockerfile", "worker/Dockerfile")
	gh.repo(shopRepo, "broken", "6666666666666666666666666666666666666666", "name: shop\nservices:\n  - name: web\n    port: 0\n", "Dockerfile")
	return gh
}

// addInstance adds branch to shop and returns the answer.
func (h *harness) addInstance(body map[string]any) apiv1.AppInstanceCreated {
	h.t.Helper()
	code, b := h.do("POST", "/v1/apps/shop/instances", opToken, body)
	if code != 201 {
		h.t.Fatalf("add %v: %d %s", body, code, b)
	}
	return decode[apiv1.AppInstanceCreated](h.t, b)
}

func TestCreateAppInstance(t *testing.T) {
	gh, dep := instanceGitHub(), &fakeDeployer{}
	h := newHarness(t, withApps(""), withGitHub(gh, dep))
	first := h.registerShop()

	got := h.addInstance(map[string]any{"branch": "dev"})
	in, d := got.Instance, got.Deployment
	if in.Name != "shop--dev" || in.Branch != "dev" || in.Default || !in.AutoDeploy || in.SpecCommit != devSHA || in.CreatedBy != "op" ||
		in.Url == nil || *in.Url != "https://shop--dev.hopcharlie.dev" || len(in.Spec.Services) != 2 || in.Spec.Name != "shop" {
		t.Fatalf("instance %s", mustJSON(t, in))
	}
	if len(in.Status) != 2 || in.Status[0].Env != "gke" || in.Status[1].Env != "eks" || in.Status[0].Status != apiv1.DeploymentEnvironmentStatusPending ||
		in.LastDeployment == nil || in.LastDeployment.Id != d.Id {
		t.Fatalf("instance status %s", mustJSON(t, in))
	}
	if d.Trigger != apiv1.DeploymentTriggerRegister || d.Status != apiv1.DeploymentStatusQueued || d.App != "shop" || d.Branch != "dev" || d.Instance != "shop--dev" ||
		d.Ref != "dev" || d.Commit != devSHA || d.TriggeredBy != "op" || len(d.Environments) != 2 || d.Environments[0].Env != "gke" {
		t.Fatalf("deployment %+v", d)
	}
	if ids := dep.enqueued(); !slices.Equal(ids, []string{first.Id, d.Id}) {
		t.Fatalf("enqueued %v", ids)
	}

	ctx := t.Context()
	rows, err := h.store.ListAppBranches(ctx, "shop")
	if err != nil || len(rows) != 1 {
		t.Fatalf("stored branches %+v, %v", rows, err)
	}
	if r := rows[0]; r.Branch != "dev" || r.Instance != "shop--dev" || !r.AutoDeploy || r.SpecCommit != devSHA || r.CreatedBy != "op" || !json.Valid(r.Spec) || !r.CreatedAt.Equal(in.CreatedAt) {
		t.Fatalf("stored branch %+v", r)
	}
	if sd, err := h.store.GetDeployment(ctx, d.Id); err != nil || sd.Branch != "dev" || sd.Instance != "shop--dev" || sd.Trigger != "register" || sd.Status != store.DeployQueued {
		t.Fatalf("stored deployment %+v, %v", sd, err)
	}
	audit, _ := h.store.ListAudit(ctx, 0, 1)
	if a := audit[0]; a.Action != "POST /v1/apps/{name}/instances" || a.Actor != "op" || a.Status != 201 ||
		!strings.Contains(string(a.Detail), `"instance":"shop--dev"`) || !strings.Contains(string(a.Detail), d.Id) {
		t.Fatalf("audit %+v %s", a, a.Detail)
	}

	// The app lists the default instance first, then the added one.
	code, b := h.do("GET", "/v1/apps/shop", viewerToken, nil)
	app := decode[apiv1.App](t, b)
	if code != 200 || len(app.Deploy.Instances) != 2 || app.Deploy.Instances[0].Name != "shop" || app.Deploy.Instances[1].Name != "shop--dev" ||
		app.Deploy.Instances[1].LastDeployment.Id != d.Id || app.Deploy.LastDeployment.Id != first.Id || app.Deploy.Instances[0].LastDeployment.Id != first.Id {
		t.Fatalf("app: %d %s", code, b)
	}

	// autoDeploy can be turned off.
	got = h.addInstance(map[string]any{"branch": "feature/x", "autoDeploy": false})
	if got.Instance.Name != "shop--feature-x" || got.Instance.AutoDeploy || got.Deployment.Instance != "shop--feature-x" {
		t.Fatalf("feature/x %s", mustJSON(t, got))
	}
	if rows, _ := h.store.ListAppBranches(ctx, "shop"); len(rows) != 2 || rows[1].Branch != "feature/x" || rows[1].AutoDeploy {
		t.Fatalf("stored branches %+v", rows)
	}
}

func TestCreateAppInstanceErrors(t *testing.T) {
	gh := instanceGitHub()
	h := newHarness(t, withApps(""), withGitHub(gh, nil))
	h.registerShop()

	cases := []struct {
		name, app, token string
		body             any
		status           int
		code             string
		fieldPath        string // for 422: the first error's path
	}{
		{name: "no token", app: "shop", body: map[string]any{"branch": "dev"}, status: 401, code: "unauthorized"},
		{name: "viewer", app: "shop", token: viewerToken, body: map[string]any{"branch": "dev"}, status: 403, code: "forbidden"},
		{name: "no body", app: "shop", token: opToken, status: 400, code: "invalid_request"},
		{name: "unknown field", app: "shop", token: opToken, body: map[string]any{"branch": "dev", "ref": "x"}, status: 400, code: "invalid_request"},
		{name: "no branch", app: "shop", token: opToken, body: map[string]any{}, status: 400, code: "invalid_request"},
		{name: "bad branch", app: "shop", token: opToken, body: map[string]any{"branch": "a..b"}, status: 400, code: "invalid_request"},
		{name: "empty slug", app: "shop", token: opToken, body: map[string]any{"branch": "_"}, status: 400, code: "invalid_request"},
		{name: "unknown app", app: "nope", token: opToken, body: map[string]any{"branch": "dev"}, status: 404, code: "app_not_found"},
		{name: "file app", app: "moimroom", token: opToken, body: map[string]any{"branch": "dev"}, status: 409, code: "app_not_registered"},
		{name: "the app's own branch", app: "shop", token: opToken, body: map[string]any{"branch": "main"}, status: 409, code: "branch_exists"},
		{name: "unknown branch", app: "shop", token: opToken, body: map[string]any{"branch": "nope"}, status: 422, code: "invalid_app_spec", fieldPath: ""},
		{name: "no platform.yaml", app: "shop", token: opToken, body: map[string]any{"branch": "nospec"}, status: 422, code: "invalid_app_spec", fieldPath: ""},
		{name: "another app's name", app: "shop", token: opToken, body: map[string]any{"branch": "renamed"}, status: 422, code: "invalid_app_spec", fieldPath: "name"},
		{name: "breaks rules", app: "shop", token: opToken, body: map[string]any{"branch": "broken"}, status: 422, code: "invalid_app_spec", fieldPath: "services[0].port"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, b := h.do("POST", "/v1/apps/"+tc.app+"/instances", tc.token, tc.body)
			if code != tc.status {
				t.Fatalf("%d %s, want %d", code, b, tc.status)
			}
			if code == 422 {
				inv := decode[apiv1.AppSpecInvalid](t, b)
				if inv.Error.Code != tc.code || inv.Validation.Valid || len(inv.Validation.Errors) == 0 || inv.Validation.Errors[0].Path != tc.fieldPath {
					t.Fatalf("422 body %s", b)
				}
				return
			}
			if got := errorCode(t, b); got != tc.code {
				t.Fatalf("code %q, want %q: %s", got, tc.code, b)
			}
		})
	}
	if rows, _ := h.store.ListAppBranches(t.Context(), "shop"); len(rows) != 0 {
		t.Fatalf("failed adds stored %+v", rows)
	}

	// An added branch is taken, and so is its instance name; the app holds
	// MaxInstances instances, the default one included.
	h.addInstance(map[string]any{"branch": "feature/x"})
	h.addInstance(map[string]any{"branch": "dev"})
	for _, tc := range []struct {
		branch, code string
	}{
		{"feature/x", "branch_exists"},
		{"feature-x", "instance_exists"},
		{"staging", "instance_limit"},
	} {
		if code, b := h.do("POST", "/v1/apps/shop/instances", opToken, map[string]any{"branch": tc.branch}); code != 409 || errorCode(t, b) != tc.code {
			t.Errorf("%s: %d %s, want 409 %s", tc.branch, code, b, tc.code)
		}
	}
	rows, _ := h.store.ListAppBranches(t.Context(), "shop")
	if len(rows) != 2 || rows[0].Branch != "dev" || rows[1].Branch != "feature/x" {
		t.Fatalf("stored branches %+v", rows)
	}
	if deps, _ := h.store.ListDeployments(t.Context(), "shop", 0); len(deps) != 3 {
		t.Fatalf("%d deployments, want the app's and two instances' first ones", len(deps))
	}

	t.Run("GitHub unreachable", func(t *testing.T) {
		gh.fail(errors.New("connection refused"))
		defer gh.fail(nil)
		if code, b := h.do("POST", "/v1/apps/shop/instances", opToken, map[string]any{"branch": "staging"}); code != 502 || errorCode(t, b) != "github_unreachable" {
			t.Fatalf("%d %s", code, b)
		}
	})
	t.Run("GitHub not configured", func(t *testing.T) {
		h := newHarness(t)
		if err := h.store.CreateApp(t.Context(), store.App{Name: "shop", Repo: shopRepo, Branch: "main", Environments: []string{"gke"}, Spec: []byte(`{}`), CreatedAt: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
		if code, b := h.do("POST", "/v1/apps/shop/instances", opToken, map[string]any{"branch": "dev"}); code != 503 || errorCode(t, b) != "github_not_configured" {
			t.Fatalf("%d %s", code, b)
		}
	})
}

func TestDeleteAppInstance(t *testing.T) {
	rm := &fakeRemover{}
	h := newHarness(t, withApps(""), withGitHub(instanceGitHub(), nil), withRemover(rm))
	h.registerShop()
	h.addInstance(map[string]any{"branch": "dev"})

	for _, tc := range []struct {
		name, path, token string
		err               error
		status            int
		code              string
	}{
		{"viewer", "/v1/apps/shop/instances/shop--dev", viewerToken, nil, 403, "forbidden"},
		{"file app", "/v1/apps/moimroom/instances/moimroom--dev", opToken, nil, 409, "app_not_registered"},
		{"unknown app", "/v1/apps/nope/instances/nope--dev", opToken, nil, 404, "app_not_found"},
		{"default instance", "/v1/apps/shop/instances/shop", opToken, nil, 409, "default_instance"},
		{"unknown instance", "/v1/apps/shop/instances/shop--nope", opToken, nil, 404, "instance_not_found"},
		{"removed meanwhile", "/v1/apps/shop/instances/shop--dev", opToken, fmt.Errorf("%w: branch dev", store.ErrNotFound), 404, "instance_not_found"},
		{"writes held", "/v1/apps/shop/instances/shop--dev", opToken, deploy.ErrWritesHeld, 409, "cluster_busy"},
		{"gitops down", "/v1/apps/shop/instances/shop--dev", opToken, deploy.ErrGitOps, 502, "github_unreachable"},
		{"gateway down", "/v1/apps/shop/instances/shop--dev", opToken, deploy.ErrGateway, 502, "gateway_unreachable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rm.err = tc.err
			code, b := h.do("DELETE", tc.path, tc.token, nil)
			if code != tc.status || errorCode(t, b) != tc.code {
				t.Fatalf("%d %s, want %d %s", code, b, tc.status, tc.code)
			}
		})
	}
	if len(rm.removed) != 0 {
		t.Fatalf("removed %v", rm.removed)
	}
	rm.err = nil
	if code, b := h.do("DELETE", "/v1/apps/shop/instances/shop--dev", opToken, nil); code != 202 || len(b) != 0 || !slices.Equal(rm.removed, []string{"shop@dev"}) {
		t.Fatalf("delete: %d %s, removed %v", code, b, rm.removed)
	}
	audit, _ := h.store.ListAudit(t.Context(), 0, 1)
	if a := audit[0]; a.Action != "DELETE /v1/apps/{name}/instances/{instance}" || a.Status != 202 ||
		!strings.Contains(string(a.Detail), `"instance":"shop--dev"`) || !strings.Contains(string(a.Detail), `"branch":"dev"`) {
		t.Fatalf("audit %+v %s", a, a.Detail)
	}

	h2 := newHarness(t, withApps(""), withGitHub(instanceGitHub(), nil))
	h2.registerShop()
	h2.addInstance(map[string]any{"branch": "dev"})
	if code, b := h2.do("DELETE", "/v1/apps/shop/instances/shop--dev", opToken, nil); code != 503 || errorCode(t, b) != "github_not_configured" {
		t.Fatalf("no remover: %d %s", code, b)
	}
}

func TestWebhookPushDeploysAddedBranches(t *testing.T) {
	h, dep := webhookHarness(t)
	ctx := t.Context()
	// shop deploys dev; shop-manual does not deploy its own branch on push,
	// but does deploy its added dev; shop-docs adds feature without autoDeploy.
	for _, b := range []store.AppBranch{
		{App: "shop", Branch: "dev", Instance: "shop--dev", AutoDeploy: true},
		{App: "shop-manual", Branch: "dev", Instance: "shop-manual--dev", AutoDeploy: true},
		{App: "shop-docs", Branch: "feature", Instance: "shop-docs--feature", AutoDeploy: false},
	} {
		b.Spec, b.SpecCommit, b.CreatedBy, b.CreatedAt = []byte(`{}`), headSHA, "op", time.Now().UTC()
		if err := h.store.CreateAppBranch(ctx, b, 2); err != nil {
			t.Fatal(err)
		}
	}
	push := func(delivery, branch string) (int, []apiv1.Deployment) {
		t.Helper()
		body := pushPayload(shopRepo, "refs/heads/"+branch, pushSHA, false)
		code, b := h.webhook("push", delivery, body, sign(hookToken, []byte(body)))
		if code != 202 {
			return code, nil
		}
		return code, decode[apiv1.DeploymentList](t, b).Deployments
	}
	instances := func(list []apiv1.Deployment) []string {
		var out []string
		for _, d := range list {
			out = append(out, d.Instance)
		}
		slices.Sort(out)
		return out
	}

	code, list := push("d-dev", "dev")
	if code != 202 || !slices.Equal(instances(list), []string{"shop--dev", "shop-manual--dev"}) {
		t.Fatalf("push dev: %d %s", code, mustJSON(t, list))
	}
	for _, d := range list {
		if d.Branch != "dev" || d.Ref != "dev" || d.Commit != pushSHA || d.Trigger != apiv1.DeploymentTriggerPush || d.TriggeredBy != "github:dev" {
			t.Fatalf("deployment %+v", d)
		}
		if sd, err := h.store.GetDeployment(ctx, d.Id); err != nil || sd.Instance != d.Instance || sd.Branch != "dev" {
			t.Fatalf("stored %+v, %v", sd, err)
		}
	}
	if n := len(dep.enqueued()); n != 2 {
		t.Fatalf("enqueued %d", n)
	}

	// main deploys only the default instances that deploy pushes.
	code, list = push("d-main", "main")
	if code != 202 || !slices.Equal(instances(list), []string{"shop", "shop-docs"}) {
		t.Fatalf("push main: %d %s", code, mustJSON(t, list))
	}
	for _, d := range list {
		if d.Branch != "main" || d.Instance != d.App {
			t.Fatalf("deployment %+v", d)
		}
	}

	// An added branch without autoDeploy, and a branch nobody deploys.
	for _, branch := range []string{"feature", "untracked"} {
		if code, _ := push("d-"+branch, branch); code != 204 {
			t.Fatalf("push %s: %d, want 204", branch, code)
		}
	}
	if n := len(dep.enqueued()); n != 4 {
		t.Fatalf("enqueued %d, want 4", n)
	}
	if deps, _ := h.store.ListInstanceDeployments(ctx, "shop-docs--feature", 0); len(deps) != 0 {
		t.Fatalf("feature deployed %+v", deps)
	}
}

func TestCreateDeploymentToAddedBranch(t *testing.T) {
	gh, dep := instanceGitHub(), &fakeDeployer{}
	h := newHarness(t, withApps(""), withGitHub(gh, dep))
	h.registerShop()
	h.addInstance(map[string]any{"branch": "dev"})

	// ref defaults to the branch.
	code, b := h.do("POST", "/v1/apps/shop/deployments", opToken, map[string]any{"branch": "dev"})
	d := decode[apiv1.Deployment](t, b)
	if code != 202 || d.Branch != "dev" || d.Instance != "shop--dev" || d.Ref != "dev" || d.Commit != devSHA || d.Trigger != apiv1.DeploymentTriggerManual || len(d.Environments) != 2 {
		t.Fatalf("deploy dev: %d %s", code, b)
	}
	if sd, err := h.store.GetDeployment(t.Context(), d.Id); err != nil || sd.Instance != "shop--dev" {
		t.Fatalf("stored %+v, %v", sd, err)
	}
	// Another ref into the dev instance, to some environments.
	code, b = h.do("POST", "/v1/apps/shop/deployments", opToken, map[string]any{"branch": "dev", "ref": "main", "environments": []string{"eks"}})
	d = decode[apiv1.Deployment](t, b)
	if code != 202 || d.Branch != "dev" || d.Instance != "shop--dev" || d.Ref != "main" || d.Commit != headSHA || len(d.Environments) != 1 {
		t.Fatalf("deploy main into dev: %d %s", code, b)
	}
	// Without a branch, the default instance.
	code, b = h.do("POST", "/v1/apps/shop/deployments", opToken, nil)
	if d := decode[apiv1.Deployment](t, b); code != 202 || d.Branch != "main" || d.Instance != "shop" || d.Ref != "main" {
		t.Fatalf("deploy default: %d %s", code, b)
	}
	if n := len(dep.enqueued()); n != 5 {
		t.Fatalf("enqueued %d, want 5", n)
	}

	for _, tc := range []struct {
		name   string
		body   any
		status int
		code   string
	}{
		{"unknown branch", map[string]any{"branch": "staging"}, 400, "invalid_request"},
		{"empty branch", map[string]any{"branch": ""}, 400, "invalid_request"},
		{"another app's name on the branch", map[string]any{"branch": "dev", "ref": "renamed"}, 422, "invalid_app_spec"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if code, b := h.do("POST", "/v1/apps/shop/deployments", opToken, tc.body); code != tc.status || errorCode(t, b) != tc.code {
				t.Fatalf("%d %s, want %d %s", code, b, tc.status, tc.code)
			}
		})
	}
}

func TestAppInstancesInAppJSON(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	spec := func(service string, public bool) []byte {
		return []byte(fmt.Sprintf(`{"name":"shop","services":[{"name":%q,"path":".","dockerfile":"Dockerfile","port":8080,"public":%v,"replicas":1}]}`, service, public))
	}
	created := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)
	if err := h.store.CreateApp(ctx, store.App{Name: "shop", Repo: shopRepo, Branch: "main", Environments: []string{"gke", "eks"}, AutoDeploy: true,
		Spec: spec("web", true), SpecCommit: headSHA, CreatedBy: "op", CreatedAt: created}); err != nil {
		t.Fatal(err)
	}
	// Added in this order; listed by branch name.
	for _, b := range []store.AppBranch{
		{App: "shop", Branch: "dev", Instance: "shop--dev", AutoDeploy: false, Spec: spec("api", false), SpecCommit: devSHA, CreatedBy: "dev-op", CreatedAt: created.Add(time.Minute)},
		{App: "shop", Branch: "alpha", Instance: "shop--alpha", AutoDeploy: true, Spec: spec("web", true), SpecCommit: pushSHA, CreatedBy: "op", CreatedAt: created.Add(2 * time.Minute)},
	} {
		if err := h.store.CreateAppBranch(ctx, b, 2); err != nil {
			t.Fatal(err)
		}
	}
	// The default instance reached gke once; dev has two newer deployments,
	// the newest syncing in gke and degraded in eks.
	for _, d := range []store.Deployment{
		{ID: "dep_main", App: "shop", Branch: "main", Instance: "shop", Ref: "main", Commit: headSHA, CreatedAt: created,
			Environments: []store.DeploymentEnvironment{{Env: "gke", Status: "healthy", SyncStatus: "Synced", HealthStatus: "Healthy"}, {Env: "eks", Status: "pending"}}},
		{ID: "dep_dev1", App: "shop", Branch: "dev", Instance: "shop--dev", Ref: "dev", Commit: headSHA, CreatedAt: created.Add(time.Hour),
			Environments: []store.DeploymentEnvironment{{Env: "gke", Status: "healthy"}, {Env: "eks", Status: "healthy"}}},
		{ID: "dep_dev2", App: "shop", Branch: "dev", Instance: "shop--dev", Ref: "dev", Commit: devSHA, CreatedAt: created.Add(2 * time.Hour),
			Environments: []store.DeploymentEnvironment{{Env: "gke", Status: "syncing"}, {Env: "eks", Status: "degraded"}}},
	} {
		d.Trigger, d.TriggeredBy, d.Status = "push", "github:dev", store.DeployQueued
		if err := h.store.CreateDeployment(ctx, d); err != nil {
			t.Fatal(err)
		}
	}

	code, b := h.do("GET", "/v1/apps/shop", viewerToken, nil)
	if code != 200 {
		t.Fatalf("%d %s", code, b)
	}
	dep := decode[apiv1.App](t, b).Deploy

	// The top-level fields are the default instance's, ignoring dev's newer deployments.
	if dep.LastDeployment == nil || dep.LastDeployment.Id != "dep_main" || dep.Branch != "main" || !dep.AutoDeploy || dep.SpecCommit != headSHA ||
		dep.Url == nil || *dep.Url != "https://shop.hopcharlie.dev" || dep.Spec.Services[0].Name != "web" {
		t.Fatalf("deploy %s", mustJSON(t, dep))
	}
	if st := dep.Status; len(st) != 2 || st[0].Status != "healthy" || *st[0].Commit != headSHA || st[1].Status != "pending" || st[1].Commit != nil {
		t.Fatalf("status %s", mustJSON(t, st))
	}

	var names []string
	for _, in := range dep.Instances {
		names = append(names, in.Name)
	}
	if !slices.Equal(names, []string{"shop", "shop--alpha", "shop--dev"}) {
		t.Fatalf("instances %v", names)
	}
	def, alpha, dev := dep.Instances[0], dep.Instances[1], dep.Instances[2]
	if !def.Default || def.Branch != "main" || def.LastDeployment.Id != "dep_main" || string(mustJSON(t, def.Status)) != string(mustJSON(t, dep.Status)) ||
		*def.Url != *dep.Url || def.CreatedBy != "op" || !def.CreatedAt.Equal(created) {
		t.Fatalf("default instance %s", mustJSON(t, def))
	}
	if alpha.Default || alpha.Branch != "alpha" || !alpha.AutoDeploy || alpha.SpecCommit != pushSHA || alpha.Url == nil || *alpha.Url != "https://shop--alpha.hopcharlie.dev" ||
		alpha.LastDeployment != nil || len(alpha.Status) != 2 || alpha.Status[0].Status != "pending" || alpha.Status[1].Status != "pending" {
		t.Fatalf("alpha instance %s", mustJSON(t, alpha))
	}
	if dev.Default || dev.Branch != "dev" || dev.AutoDeploy || dev.SpecCommit != devSHA || dev.Spec.Services[0].Name != "api" || dev.Url != nil ||
		dev.CreatedBy != "dev-op" || !dev.CreatedAt.Equal(created.Add(time.Minute)) || dev.LastDeployment == nil || dev.LastDeployment.Id != "dep_dev2" {
		t.Fatalf("dev instance %s", mustJSON(t, dev))
	}
	if st := dev.Status; len(st) != 2 || st[0].Env != "gke" || st[0].Status != "syncing" || *st[0].Commit != devSHA || st[1].Status != "degraded" || *st[1].Commit != devSHA {
		t.Fatalf("dev status %s", mustJSON(t, st))
	}

	// GET /v1/apps shows the same.
	code, b = h.do("GET", "/v1/apps", viewerToken, nil)
	list := decode[apiv1.AppList](t, b).Apps
	if code != 200 || len(list) != 1 || string(mustJSON(t, list[0].Deploy)) != string(mustJSON(t, dep)) {
		t.Fatalf("list: %d %s", code, b)
	}
}
