package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/deploy"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/github"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/runs"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/client"
)

const (
	headSHA   = "9fceb02d0ae598e95dc970b74767f19372d61af8"
	pushSHA   = "1111111111111111111111111111111111111111"
	shopRepo  = "acme/shop"
	hookToken = "webhook-secret-for-tests"
)

const shopSpec = `name: shop
services:
  - name: web
    port: 8080
    healthCheck: /healthz
    public: true
    env: {LOG_LEVEL: info}
  - name: worker
    path: worker
    port: 9090
resources:
  - {name: db, type: postgres}
`

// fakeGitHub is api.GitHub over maps. err, when set, fails every call;
// pushErr fails PushToken only.
type fakeGitHub struct {
	mu        sync.Mutex
	refs      map[string]string   // "owner/name@ref" -> sha
	files     map[string]string   // "owner/name@sha:path" -> content
	topics    map[string][]string // "owner/name" -> topics; absent is not found
	repos     []github.Repo
	pushToken github.PushToken // what PushToken returns for a repo in acme
	pushed    []string         // repos PushToken returned a token for
	err       error
	pushErr   error
	reads     int
}

func newFakeGitHub() *fakeGitHub {
	return &fakeGitHub{refs: map[string]string{}, files: map[string]string{}, topics: map[string][]string{}}
}

// repo adds a repository whose branch head holds spec and every file in dockerfiles.
func (f *fakeGitHub) repo(repo, branch, sha, spec string, dockerfiles ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refs[repo+"@"+branch] = sha
	if spec != "" {
		f.files[repo+"@"+sha+":platform.yaml"] = spec
	}
	for _, p := range dockerfiles {
		f.files[repo+"@"+sha+":"+p] = "FROM scratch\n"
	}
}

func (f *fakeGitHub) ResolveRef(_ context.Context, repo, ref string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return "", f.err
	}
	if s, ok := f.refs[repo+"@"+ref]; ok {
		return s, nil
	}
	return "", fmt.Errorf("%w: %s@%s", github.ErrNotFound, repo, ref)
}

func (f *fakeGitHub) ReadFile(_ context.Context, repo, ref, path string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	if f.err != nil {
		return nil, f.err
	}
	if c, ok := f.files[repo+"@"+ref+":"+path]; ok {
		return []byte(c), nil
	}
	return nil, fmt.Errorf("%w: %s", github.ErrNotFound, path)
}

func (f *fakeGitHub) ListRepos(context.Context) ([]github.Repo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	return slices.Clone(f.repos), nil
}

func (f *fakeGitHub) RepoTopics(_ context.Context, repo string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	if t, ok := f.topics[repo]; ok {
		return slices.Clone(t), nil
	}
	return nil, fmt.Errorf("%w: %s", github.ErrNotFound, repo)
}

// PushToken refuses repositories outside the org acme, like the client.
func (f *fakeGitHub) PushToken(_ context.Context, repo string) (github.PushToken, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	owner, _, _ := strings.Cut(repo, "/")
	switch {
	case f.err != nil:
		return github.PushToken{}, f.err
	case f.pushErr != nil:
		return github.PushToken{}, f.pushErr
	case !strings.EqualFold(owner, "acme"):
		return github.PushToken{}, fmt.Errorf("%w: %s", github.ErrNotInOrg, repo)
	}
	f.pushed = append(f.pushed, repo)
	return f.pushToken, nil
}

func (f *fakeGitHub) fail(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

// fakeDeployer records what it was handed.
type fakeDeployer struct {
	mu  sync.Mutex
	ids []string
}

func (f *fakeDeployer) Enqueue(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ids = append(f.ids, id)
	return nil
}

func (f *fakeDeployer) enqueued() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.ids)
}

func withGitHub(gh *fakeGitHub, dep *fakeDeployer) option {
	return func(h *harness, _ *runs.Config, d *Deps) {
		h.webhookSecret = hookToken
		if gh != nil {
			d.GitHub = gh
		}
		if dep != nil {
			d.Deployer = dep
		}
	}
}

func shopGitHub() *fakeGitHub {
	gh := newFakeGitHub()
	gh.repo(shopRepo, "main", headSHA, shopSpec, "Dockerfile", "worker/Dockerfile")
	return gh
}

func errorCode(t *testing.T, b []byte) string {
	t.Helper()
	return decode[apiv1.Error](t, b).Error.Code
}

func TestCreateApp(t *testing.T) {
	gh, dep := shopGitHub(), &fakeDeployer{}
	h := newHarness(t, withApps(""), withGitHub(gh, dep))

	code, b := h.do("POST", "/v1/apps", opToken, map[string]any{"repo": shopRepo, "environments": []string{"eks", "gke"}})
	if code != 201 {
		t.Fatalf("create: %d %s", code, b)
	}
	got := decode[apiv1.AppCreated](t, b)
	app, d := got.App, got.Deployment
	if app.Name != "shop" || app.Namespace != "shop" || app.Database.Name != "app" || len(app.Environments) != 0 || app.Deploy == nil {
		t.Fatalf("app = %+v", app)
	}
	deploy := *app.Deploy
	if deploy.Repo != shopRepo || deploy.Branch != "main" || !deploy.AutoDeploy || deploy.SpecCommit != headSHA || deploy.CreatedBy != "op" ||
		!slices.Equal(deploy.Environments, []string{"gke", "eks"}) || deploy.Url == nil || *deploy.Url != "https://shop.hopcharlie.dev" {
		t.Fatalf("deploy = %+v", deploy)
	}
	if len(deploy.Spec.Services) != 2 || deploy.Spec.Services[1].Path != "worker" || deploy.Spec.Services[1].Replicas != 1 {
		t.Fatalf("spec = %+v", deploy.Spec)
	}
	if len(deploy.Status) != 2 || deploy.Status[0].Env != "gke" || deploy.Status[0].Status != apiv1.DeploymentEnvironmentStatusPending {
		t.Fatalf("status = %+v", deploy.Status)
	}
	if d.Trigger != apiv1.DeploymentTriggerRegister || d.Status != apiv1.DeploymentStatusQueued || d.Commit != headSHA || d.Ref != "main" ||
		d.TriggeredBy != "op" || !strings.HasPrefix(d.Id, "dep_") || len(d.Environments) != 2 || d.Environments[1].Env != "eks" ||
		d.Environments[1].Status != apiv1.DeploymentEnvironmentStatusPending || deploy.LastDeployment == nil || deploy.LastDeployment.Id != d.Id {
		t.Fatalf("deployment = %+v", d)
	}
	if ids := dep.enqueued(); !slices.Equal(ids, []string{d.Id}) {
		t.Fatalf("enqueued %v, want %s", ids, d.Id)
	}

	stored, err := h.store.GetApp(t.Context(), "shop")
	if err != nil || stored.SpecCommit != headSHA || !json.Valid(stored.Spec) {
		t.Fatalf("stored app %+v, %v", stored, err)
	}
	if sd, err := h.store.GetDeployment(t.Context(), d.Id); err != nil || sd.Status != store.DeployQueued {
		t.Fatalf("stored deployment %+v, %v", sd, err)
	}

	// The registration is audited with the app and its first deployment.
	audit, _ := h.store.ListAudit(t.Context(), 0, 10)
	if len(audit) == 0 || audit[0].Action != "POST /v1/apps" || audit[0].Actor != "op" || audit[0].Status != 201 ||
		!strings.Contains(string(audit[0].Detail), `"app":"shop"`) || !strings.Contains(string(audit[0].Detail), d.Id) {
		t.Fatalf("audit = %+v", audit)
	}

	// Listed after the file's apps, and readable by name.
	code, b = h.do("GET", "/v1/apps", viewerToken, nil)
	list := decode[apiv1.AppList](t, b).Apps
	if code != 200 || len(list) != 2 || list[0].Name != "moimroom" || list[0].Deploy != nil || list[1].Name != "shop" || list[1].Deploy == nil {
		t.Fatalf("list: %d %s", code, b)
	}
	if code, b = h.do("GET", "/v1/apps/shop", viewerToken, nil); code != 200 || decode[apiv1.App](t, b).Deploy.LastDeployment.Id != d.Id {
		t.Fatalf("get: %d %s", code, b)
	}
	if code, b = h.do("GET", "/v1/apps/moimroom", viewerToken, nil); code != 200 || decode[apiv1.App](t, b).Deploy != nil {
		t.Fatalf("get file app: %d %s", code, b)
	}
	if code, b = h.do("GET", "/v1/apps/nope", viewerToken, nil); code != 404 || errorCode(t, b) != "app_not_found" {
		t.Fatalf("get unknown: %d %s", code, b)
	}

	// The name is now taken.
	if code, b = h.do("POST", "/v1/apps", opToken, map[string]any{"repo": shopRepo, "environments": []string{"gke"}}); code != 409 || errorCode(t, b) != "app_exists" {
		t.Fatalf("again: %d %s", code, b)
	}
}

func TestCreateAppOptions(t *testing.T) {
	gh := shopGitHub()
	gh.repo(shopRepo, "release/v2", pushSHA, shopSpec, "Dockerfile", "worker/Dockerfile")
	h := newHarness(t, withGitHub(gh, nil)) // no deployer: the deployment stays queued
	code, b := h.do("POST", "/v1/apps", opToken, map[string]any{"repo": shopRepo, "branch": "release/v2", "environments": []string{"onprem"}, "autoDeploy": false})
	if code != 201 {
		t.Fatalf("create: %d %s", code, b)
	}
	got := decode[apiv1.AppCreated](t, b)
	if got.App.Deploy.Branch != "release/v2" || got.App.Deploy.AutoDeploy || got.App.Deploy.SpecCommit != pushSHA || got.Deployment.Ref != "release/v2" {
		t.Fatalf("created = %+v", got)
	}
	if d, _ := h.store.GetDeployment(t.Context(), got.Deployment.Id); d.Status != store.DeployQueued {
		t.Fatalf("deployment status %q", d.Status)
	}
}

func TestCreateAppErrors(t *testing.T) {
	gh := shopGitHub()
	gh.repo("acme/nospec", "main", pushSHA, "")
	gh.repo("acme/broken", "main", pushSHA, "name: Broken\nservices:\n  - name: web\n    port: 99999\n", "Dockerfile")
	gh.repo("acme/notyaml", "main", pushSHA, "name: [x\n")
	gh.repo("acme/nodocker", "main", pushSHA, shopSpec, "Dockerfile")
	gh.repo("acme/api", "main", pushSHA, "name: api\nservices:\n  - {name: web, port: 80}\n", "Dockerfile")
	gh.repo("acme/console", "main", pushSHA, "name: console\nservices:\n  - {name: web, port: 80}\n", "Dockerfile")
	h := newHarness(t, withGitHub(gh, nil))
	ok := map[string]any{"repo": shopRepo, "environments": []string{"gke"}}
	with := func(k string, v any) map[string]any {
		m := map[string]any{}
		for k2, v2 := range ok {
			m[k2] = v2
		}
		m[k] = v
		return m
	}

	cases := []struct {
		name      string
		token     string
		body      any
		status    int
		code      string
		fieldPath string // for 422: the first error's path
	}{
		{name: "no token", body: ok, status: 401, code: "unauthorized"},
		{name: "viewer", token: viewerToken, body: ok, status: 403, code: "forbidden"},
		{name: "agent", token: agentToken, body: ok, status: 403, code: "forbidden"},
		{name: "no body", token: opToken, status: 400, code: "invalid_request"},
		{name: "unknown field", token: opToken, body: with("name", "x"), status: 400, code: "invalid_request"},
		{name: "repo without owner", token: opToken, body: with("repo", "shop"), status: 400, code: "invalid_request"},
		{name: "repo with a path", token: opToken, body: with("repo", "acme/shop/../x"), status: 400, code: "invalid_request"},
		{name: "bad branch", token: opToken, body: with("branch", "a..b"), status: 400, code: "invalid_request"},
		{name: "branch with a space", token: opToken, body: with("branch", "a b"), status: 400, code: "invalid_request"},
		{name: "no environments", token: opToken, body: with("environments", []string{}), status: 400, code: "invalid_request"},
		{name: "unknown environment", token: opToken, body: with("environments", []string{"gke", "mars"}), status: 400, code: "invalid_request"},
		{name: "environment twice", token: opToken, body: with("environments", []string{"gke", "gke"}), status: 400, code: "invalid_request"},
		{name: "unknown branch", token: opToken, body: with("branch", "nope"), status: 422, code: "invalid_app_spec", fieldPath: ""},
		{name: "unknown repository", token: opToken, body: with("repo", "acme/nope"), status: 422, code: "invalid_app_spec", fieldPath: ""},
		{name: "no platform.yaml", token: opToken, body: with("repo", "acme/nospec"), status: 422, code: "invalid_app_spec", fieldPath: ""},
		{name: "not YAML", token: opToken, body: with("repo", "acme/notyaml"), status: 422, code: "invalid_app_spec", fieldPath: ""},
		{name: "breaks rules", token: opToken, body: with("repo", "acme/broken"), status: 422, code: "invalid_app_spec", fieldPath: "name"},
		{name: "no Dockerfile", token: opToken, body: with("repo", "acme/nodocker"), status: 422, code: "invalid_app_spec", fieldPath: "services[1].dockerfile"},
		{name: "reserved api", token: opToken, body: with("repo", "acme/api"), status: 409, code: "app_exists"},
		{name: "reserved console", token: opToken, body: with("repo", "acme/console"), status: 409, code: "app_exists"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, b := h.do("POST", "/v1/apps", tc.token, tc.body)
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
	if apps, _ := h.store.ListApps(t.Context()); len(apps) != 0 {
		t.Fatalf("failed registrations stored %+v", apps)
	}

	t.Run("GitHub unreachable", func(t *testing.T) {
		gh.fail(errors.New("connection refused"))
		defer gh.fail(nil)
		if code, b := h.do("POST", "/v1/apps", opToken, ok); code != 502 || errorCode(t, b) != "github_unreachable" {
			t.Fatalf("%d %s", code, b)
		}
	})
	t.Run("GitHub not configured", func(t *testing.T) {
		h := newHarness(t)
		if code, b := h.do("POST", "/v1/apps", opToken, ok); code != 503 || errorCode(t, b) != "github_not_configured" {
			t.Fatalf("%d %s", code, b)
		}
	})
}

func TestListAppsMergesFileAndRegistered(t *testing.T) {
	h := newHarness(t, withApps(""), withGitHub(nil, nil))
	ctx := t.Context()
	spec := func(public bool) []byte {
		return []byte(fmt.Sprintf(`{"name":"x","services":[{"name":"web","path":".","dockerfile":"Dockerfile","port":8080,"public":%v,"replicas":1}]}`, public))
	}
	created := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)
	for _, a := range []store.App{
		{Name: "zeta", Repo: "acme/zeta", Branch: "main", Environments: []string{"gke"}, Spec: spec(false), SpecCommit: headSHA, CreatedBy: "op", CreatedAt: created},
		{Name: "moimroom", Repo: "acme/moimroom", Branch: "main", Environments: []string{"gke", "eks"}, AutoDeploy: true, Spec: spec(true), SpecCommit: headSHA, CreatedBy: "op", CreatedAt: created},
		{Name: "alpha", Repo: "acme/alpha", Branch: "main", Environments: []string{"eks"}, Spec: spec(true), SpecCommit: headSHA, CreatedBy: "op", CreatedAt: created},
	} {
		if err := h.store.CreateApp(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	// moimroom: an older deployment reached both environments; the newest
	// reached gke only and is still syncing there.
	finished := created.Add(10 * time.Minute)
	for _, d := range []store.Deployment{
		{ID: "dep_old", App: "moimroom", Branch: "main", Instance: "moimroom", Ref: "main", Commit: headSHA, Trigger: "register", TriggeredBy: "op", Status: store.DeployQueued, CreatedAt: created,
			Environments: []store.DeploymentEnvironment{{Env: "gke", Status: "healthy", SyncStatus: "Synced", HealthStatus: "Healthy"}, {Env: "eks", Status: "degraded", URL: "https://moimroom.eks.example"}}},
		{ID: "dep_new", App: "moimroom", Branch: "main", Instance: "moimroom", Ref: "main", Commit: pushSHA, Trigger: "push", TriggeredBy: "github:dev", Status: store.DeployQueued, CreatedAt: created.Add(time.Hour),
			Environments: []store.DeploymentEnvironment{{Env: "gke", Status: "syncing"}, {Env: "eks", Status: "pending"}}},
	} {
		if err := h.store.CreateDeployment(ctx, d); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.store.UpdateDeployment(ctx, store.Deployment{ID: "dep_old", Status: store.DeployFailed, FinishedAt: &finished,
		Environments: []store.DeploymentEnvironment{{Env: "gke", Status: "healthy", SyncStatus: "Synced", HealthStatus: "Healthy"}, {Env: "eks", Status: "degraded", URL: "https://moimroom.eks.example"}}}); err != nil {
		t.Fatal(err)
	}

	code, b := h.do("GET", "/v1/apps", viewerToken, nil)
	if code != 200 {
		t.Fatalf("%d %s", code, b)
	}
	list := decode[apiv1.AppList](t, b).Apps
	var names []string
	for _, a := range list {
		names = append(names, a.Name)
	}
	if !slices.Equal(names, []string{"moimroom", "alpha", "zeta"}) {
		t.Fatalf("names %v", names)
	}
	moim := list[0]
	if len(moim.Environments) != 3 || moim.Database.Schemas[0] != "app" || moim.Deploy == nil {
		t.Fatalf("moimroom keeps its file entry: %+v", moim)
	}
	st := moim.Deploy.Status
	if len(st) != 2 || st[0].Env != "gke" || st[0].Status != "syncing" || *st[0].Commit != pushSHA ||
		st[1].Env != "eks" || st[1].Status != "degraded" || *st[1].Commit != headSHA || *st[1].Url != "https://moimroom.eks.example" || !st[1].UpdatedAt.Equal(finished) {
		t.Fatalf("moimroom status %s", mustJSON(t, st))
	}
	if moim.Deploy.LastDeployment.Id != "dep_new" || *moim.Deploy.Url != "https://moimroom.hopcharlie.dev" {
		t.Fatalf("moimroom deploy %s", mustJSON(t, moim.Deploy))
	}
	// The default instance repeats the top-level fields.
	if in := moim.Deploy.Instances; len(in) != 1 || in[0].Name != "moimroom" || !in[0].Default || in[0].Branch != "main" ||
		*in[0].Url != *moim.Deploy.Url || in[0].LastDeployment.Id != "dep_new" || string(mustJSON(t, in[0].Status)) != string(mustJSON(t, st)) {
		t.Fatalf("moimroom instances %s", mustJSON(t, moim.Deploy.Instances))
	}
	alpha, zeta := list[1], list[2]
	if alpha.Namespace != "alpha" || alpha.Database.User != "app" || len(alpha.Environments) != 0 ||
		alpha.Deploy.LastDeployment != nil || alpha.Deploy.Status[0].Status != "pending" || alpha.Deploy.Status[0].Commit != nil {
		t.Fatalf("alpha %s", mustJSON(t, alpha))
	}
	if zeta.Deploy.Url != nil {
		t.Fatalf("zeta has no public service but a URL: %s", mustJSON(t, zeta))
	}
}

func TestValidateAppSpec(t *testing.T) {
	gh := shopGitHub()
	gh.repo("acme/nodocker", "main", pushSHA, shopSpec, "Dockerfile")
	h := newHarness(t, withGitHub(gh, nil))
	big := "name: x\n" + strings.Repeat("#", 64<<10)
	cases := []struct {
		name      string
		token     string
		body      any
		status    int
		code      string
		valid     bool
		commit    string
		errorPath string
	}{
		{name: "valid spec", token: viewerToken, body: map[string]any{"spec": shopSpec}, status: 200, valid: true},
		{name: "invalid spec", token: agentToken, body: map[string]any{"spec": "name: shop\nservices: []\n"}, status: 200, errorPath: "services"},
		{name: "unknown key", token: viewerToken, body: map[string]any{"spec": "name: shop\nimage: x\n"}, status: 200, errorPath: ""},
		{name: "valid repo", token: viewerToken, body: map[string]any{"repo": shopRepo}, status: 200, valid: true, commit: headSHA},
		{name: "repo without a Dockerfile", token: viewerToken, body: map[string]any{"repo": "acme/nodocker", "branch": "main"}, status: 200, commit: pushSHA, errorPath: "services[1].dockerfile"},
		{name: "unknown branch", token: viewerToken, body: map[string]any{"repo": shopRepo, "branch": "nope"}, status: 200, errorPath: ""},
		{name: "both", token: viewerToken, body: map[string]any{"repo": shopRepo, "spec": shopSpec}, status: 400, code: "invalid_request"},
		{name: "neither", token: viewerToken, body: map[string]any{}, status: 400, code: "invalid_request"},
		{name: "spec with a branch", token: viewerToken, body: map[string]any{"spec": shopSpec, "branch": "main"}, status: 400, code: "invalid_request"},
		{name: "bad repo", token: viewerToken, body: map[string]any{"repo": "nope"}, status: 400, code: "invalid_request"},
		{name: "spec too large", token: viewerToken, body: map[string]any{"spec": big}, status: 413, code: "too_large"},
		{name: "no token", body: map[string]any{"spec": shopSpec}, status: 401, code: "unauthorized"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, b := h.do("POST", "/v1/apps/validate", tc.token, tc.body)
			if code != tc.status {
				t.Fatalf("%d %s, want %d", code, b, tc.status)
			}
			if code != 200 {
				if got := errorCode(t, b); got != tc.code {
					t.Fatalf("code %q, want %q", got, tc.code)
				}
				return
			}
			v := decode[apiv1.AppSpecValidation](t, b)
			if v.Valid != tc.valid || deref(v.Commit) != tc.commit {
				t.Fatalf("validation %s", b)
			}
			if !tc.valid && (len(v.Errors) == 0 || v.Errors[0].Path != tc.errorPath) {
				t.Fatalf("errors %s", b)
			}
			if tc.valid && (len(v.Errors) != 0 || v.Spec == nil || v.Spec.Name != "shop") {
				t.Fatalf("valid result %s", b)
			}
		})
	}

	t.Run("spec needs no GitHub", func(t *testing.T) {
		h := newHarness(t)
		if code, b := h.do("POST", "/v1/apps/validate", viewerToken, map[string]any{"spec": shopSpec}); code != 200 || !decode[apiv1.AppSpecValidation](t, b).Valid {
			t.Fatalf("%d %s", code, b)
		}
		if code, b := h.do("POST", "/v1/apps/validate", viewerToken, map[string]any{"repo": shopRepo}); code != 503 || errorCode(t, b) != "github_not_configured" {
			t.Fatalf("%d %s", code, b)
		}
	})
	t.Run("GitHub unreachable", func(t *testing.T) {
		gh.fail(errors.New("timeout"))
		defer gh.fail(nil)
		if code, b := h.do("POST", "/v1/apps/validate", viewerToken, map[string]any{"repo": shopRepo}); code != 502 || errorCode(t, b) != "github_unreachable" {
			t.Fatalf("%d %s", code, b)
		}
	})
}

func TestListGitHubRepos(t *testing.T) {
	gh := newFakeGitHub()
	gh.repos = []github.Repo{
		{FullName: "acme/zeta", DefaultBranch: "main", HTMLURL: "https://github.com/acme/zeta"},
		{FullName: "acme/alpha", Private: true, DefaultBranch: "trunk", HTMLURL: "https://github.com/acme/alpha"},
	}
	h := newHarness(t, withGitHub(gh, nil))
	code, b := h.do("GET", "/v1/github/repos", viewerToken, nil)
	repos := decode[apiv1.GitHubRepoList](t, b).Repos
	if code != 200 || len(repos) != 2 || repos[0].FullName != "acme/alpha" || !repos[0].Private || repos[0].DefaultBranch != "trunk" || repos[1].HtmlUrl != "https://github.com/acme/zeta" {
		t.Fatalf("%d %s", code, b)
	}
	gh.fail(errors.New("boom"))
	if code, b := h.do("GET", "/v1/github/repos", viewerToken, nil); code != 502 || errorCode(t, b) != "github_unreachable" {
		t.Fatalf("%d %s", code, b)
	}
	if code, b := newHarness(t).do("GET", "/v1/github/repos", viewerToken, nil); code != 503 || errorCode(t, b) != "github_not_configured" {
		t.Fatalf("%d %s", code, b)
	}
}

func TestCreatePushToken(t *testing.T) {
	const secret = "ghs_push_token_for_tests"
	expires := time.Date(2026, 11, 5, 13, 0, 0, 0, time.UTC)
	imported := map[string]any{"repo": "acme/imported"}
	cases := []struct {
		name       string
		token      string
		body       any
		setup      func(*fakeGitHub)
		noGitHub   bool
		wantStatus int
		wantCode   string
		wantMsg    string // in the error message
	}{
		{name: "agent", token: agentToken, body: imported, wantStatus: 201},
		{name: "operator", token: opToken, body: imported, wantStatus: 201},
		{name: "viewer", token: viewerToken, body: imported, wantStatus: 403, wantCode: "forbidden"},
		{name: "no token", body: imported, wantStatus: 401, wantCode: "unauthorized"},
		{name: "no topic", token: agentToken, body: map[string]any{"repo": "acme/plain"}, wantStatus: 403, wantCode: "repo_not_imported", wantMsg: "scripts/import-repo.sh"},
		{name: "gitops", token: agentToken, body: map[string]any{"repo": deploy.DefaultGitOpsRepo}, wantStatus: 403, wantCode: "repo_not_imported", wantMsg: "gitops repository"},
		{name: "gitops in another case", token: opToken, body: map[string]any{"repo": strings.ToLower(deploy.DefaultGitOpsRepo)}, wantStatus: 403, wantCode: "repo_not_imported", wantMsg: "gitops repository"},
		{name: "another org", token: agentToken, body: map[string]any{"repo": "other/imported"}, wantStatus: 403, wantCode: "repo_not_imported", wantMsg: "organization"},
		{name: "not owner/name", token: agentToken, body: map[string]any{"repo": "imported"}, wantStatus: 400, wantCode: "invalid_request"},
		{name: "path in name", token: agentToken, body: map[string]any{"repo": "acme/imported/../x"}, wantStatus: 400, wantCode: "invalid_request"},
		{name: "unknown field", token: agentToken, body: map[string]any{"repo": "acme/imported", "permissions": "admin"}, wantStatus: 400, wantCode: "invalid_request"},
		{name: "invisible", token: agentToken, body: map[string]any{"repo": "acme/none"}, wantStatus: 404, wantCode: "repo_not_found"},
		{name: "topics unreadable", token: agentToken, body: imported, setup: func(f *fakeGitHub) { f.err = errors.New("boom") }, wantStatus: 502, wantCode: "github_unreachable"},
		{name: "token refused", token: agentToken, body: imported, setup: func(f *fakeGitHub) { f.pushErr = errors.New("boom") }, wantStatus: 502, wantCode: "github_unreachable"},
		{name: "not configured", token: agentToken, body: imported, noGitHub: true, wantStatus: 503, wantCode: "github_not_configured"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gh := newFakeGitHub()
			gh.topics["acme/imported"] = []string{"go", github.ImportedTopic}
			gh.topics["acme/plain"] = []string{"go"}
			gh.topics["other/imported"] = []string{github.ImportedTopic}
			gh.topics[deploy.DefaultGitOpsRepo] = []string{github.ImportedTopic}
			gh.pushToken = github.PushToken{Token: secret, ExpiresAt: expires}
			if tc.setup != nil {
				tc.setup(gh)
			}
			var h *harness
			if tc.noGitHub {
				h = newHarness(t)
			} else {
				h = newHarness(t, withGitHub(gh, nil))
			}
			var auth []string
			if tc.token != "" {
				auth = []string{"Authorization", "Bearer " + tc.token}
			}
			code, hdr, b := h.public("POST", "/v1/github/push-tokens", tc.body, auth...)
			if code != tc.wantStatus {
				t.Fatalf("%d %s, want %d", code, b, tc.wantStatus)
			}
			a := h.lastAudit()
			if a.Action != "POST /v1/github/push-tokens" || a.Status != code || strings.Contains(string(a.Detail), secret) {
				t.Fatalf("audit %+v %s", a, a.Detail)
			}
			if tc.wantStatus != 201 {
				if errorCode(t, b) != tc.wantCode || !strings.Contains(decode[apiv1.Error](t, b).Error.Message, tc.wantMsg) {
					t.Fatalf("%s", b)
				}
				if strings.Contains(string(b), secret) || len(gh.pushed) != 0 {
					t.Fatalf("refused, but a token was issued for %v: %s", gh.pushed, b)
				}
				return
			}
			got := decode[apiv1.PushToken](t, b)
			if got.Repo != "acme/imported" || got.Token != secret || !got.ExpiresAt.Equal(expires) || !slices.Equal(gh.pushed, []string{"acme/imported"}) {
				t.Fatalf("%s, pushed %v", b, gh.pushed)
			}
			if hdr.Get("Cache-Control") != "no-store" {
				t.Fatalf("Cache-Control %q", hdr.Get("Cache-Control"))
			}
			for k, v := range hdr {
				if strings.Contains(strings.Join(v, " "), secret) {
					t.Fatalf("header %s has the token", k)
				}
			}
			if !strings.Contains(string(a.Detail), `"repo":"acme/imported"`) || !strings.Contains(string(a.Detail), `"expiresAt":"2026-11-05T13:00:00Z"`) {
				t.Fatalf("audit %s", a.Detail)
			}
		})
	}

	h := newHarness(t, withGitHub(newFakeGitHub(), nil))
	if code, b := h.do("POST", "/v1/github/push-tokens", agentToken, nil); code != 400 || errorCode(t, b) != "invalid_request" {
		t.Fatalf("no body: %d %s", code, b)
	}
}

// --- webhook ---

func sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func pushPayload(repo, ref, after string, deleted bool) string {
	b, _ := json.Marshal(map[string]any{
		"ref": ref, "before": headSHA, "after": after, "deleted": deleted,
		"repository": map[string]any{"full_name": repo, "private": true},
		"pusher":     map[string]any{"name": "pusher-name", "email": "dev@example.com"},
		"sender":     map[string]any{"login": "dev"},
		"commits":    []any{map[string]any{"id": after, "message": "secret-looking commit message"}},
	})
	return string(b)
}

// webhookHarness registers shop (main, autoDeploy), shop-docs from the same
// repository and branch, shop-manual (autoDeploy off), shop-release (another
// branch), manual (the only app of its repository, autoDeploy off) and other.
func webhookHarness(t *testing.T, opts ...option) (*harness, *fakeDeployer) {
	t.Helper()
	dep := &fakeDeployer{}
	h := newHarness(t, append([]option{withGitHub(nil, dep)}, opts...)...)
	for _, a := range []store.App{
		{Name: "shop", Repo: "Acme/Shop", Branch: "main", AutoDeploy: true, Environments: []string{"gke", "eks"}},
		{Name: "shop-docs", Repo: shopRepo, Branch: "main", AutoDeploy: true, Environments: []string{"onprem"}},
		{Name: "shop-manual", Repo: shopRepo, Branch: "main", AutoDeploy: false, Environments: []string{"gke"}},
		{Name: "shop-release", Repo: shopRepo, Branch: "release", AutoDeploy: true, Environments: []string{"gke"}},
		{Name: "manual", Repo: "acme/manual-only", Branch: "main", AutoDeploy: false, Environments: []string{"gke"}},
		{Name: "other", Repo: "acme/other", Branch: "main", AutoDeploy: true, Environments: []string{"gke"}},
	} {
		a.Spec, a.SpecCommit, a.CreatedBy, a.CreatedAt = []byte(`{"name":"`+a.Name+`","services":[]}`), headSHA, "op", time.Now().UTC()
		if err := h.store.CreateApp(t.Context(), a); err != nil {
			t.Fatal(err)
		}
	}
	return h, dep
}

func (h *harness) webhook(event, delivery, body, signature string) (int, []byte) {
	h.t.Helper()
	hdr := []string{"X-GitHub-Event", event, "X-GitHub-Delivery", delivery}
	if signature != "" {
		hdr = append(hdr, "X-Hub-Signature-256", signature)
	}
	return h.do("POST", "/v1/webhooks/github", "", body, hdr...)
}

func TestWebhookPushQueuesDeployments(t *testing.T) {
	h, dep := webhookHarness(t)
	body := pushPayload(shopRepo, "refs/heads/main", pushSHA, false)
	code, b := h.webhook("push", "delivery-1", body, sign(hookToken, []byte(body)))
	if code != 202 {
		t.Fatalf("push: %d %s", code, b)
	}
	list := decode[apiv1.DeploymentList](t, b).Deployments
	if len(list) != 2 {
		t.Fatalf("want shop and shop-docs, got %s", b)
	}
	byApp := map[string]apiv1.Deployment{}
	for _, d := range list {
		byApp[d.App] = d
		if d.Trigger != apiv1.DeploymentTriggerPush || d.Commit != pushSHA || d.Ref != "main" || d.TriggeredBy != "github:dev" || d.Status != apiv1.DeploymentStatusQueued {
			t.Fatalf("deployment %+v", d)
		}
	}
	if shop := byApp["shop"]; len(shop.Environments) != 2 || shop.Environments[0].Env != "gke" || byApp["shop-docs"].Environments[0].Env != "onprem" {
		t.Fatalf("deployments %s", b)
	}
	if ids := dep.enqueued(); len(ids) != 2 || !slices.Contains(ids, byApp["shop"].Id) || !slices.Contains(ids, byApp["shop-docs"].Id) {
		t.Fatalf("enqueued %v", ids)
	}

	// The same delivery again does nothing.
	if code, b := h.webhook("push", "delivery-1", body, sign(hookToken, []byte(body))); code != 204 {
		t.Fatalf("redelivery: %d %s", code, b)
	}
	if deps, _ := h.store.ListDeployments(t.Context(), "shop", 0); len(deps) != 1 {
		t.Fatalf("redelivery created deployments: %d", len(deps))
	}
	if n := len(dep.enqueued()); n != 2 {
		t.Fatalf("redelivery enqueued: %d", n)
	}

	// Audited as github, without the payload.
	audit, _ := h.store.ListAudit(t.Context(), 0, 10)
	if len(audit) != 1 || audit[0].Actor != "github" || audit[0].Action != "POST /v1/webhooks/github" || audit[0].Status != 202 {
		t.Fatalf("audit %+v", audit)
	}
	detail := string(audit[0].Detail)
	if !strings.Contains(detail, byApp["shop"].Id) || !strings.Contains(detail, "delivery-1") || strings.Contains(detail, "secret-looking") || strings.Contains(detail, "dev@example.com") {
		t.Fatalf("audit detail %s", detail)
	}

	// The client signs the same way.
	c := client.New(h.srv.URL, "")
	again := pushPayload("acme/shop", "refs/heads/release", pushSHA, false)
	got, err := c.SendGitHubWebhook(t.Context(), "push", "delivery-2", []byte(again), hookToken)
	if err != nil || len(got.Deployments) != 1 || got.Deployments[0].App != "shop-release" {
		t.Fatalf("client: %+v, %v", got, err)
	}
}

func TestWebhookIgnoresAndRejects(t *testing.T) {
	h, dep := webhookHarness(t)
	push := pushPayload(shopRepo, "refs/heads/main", pushSHA, false)
	cases := []struct {
		name, event, delivery, body, signature string
		status                                 int
		code                                   string
	}{
		{name: "no signature", event: "push", delivery: "d1", body: push, status: 401, code: "invalid_signature"},
		{name: "wrong secret", event: "push", delivery: "d2", body: push, signature: sign("other", []byte(push)), status: 401, code: "invalid_signature"},
		{name: "signature of another body", event: "push", delivery: "d3", body: push, signature: sign(hookToken, []byte(push+" ")), status: 401, code: "invalid_signature"},
		{name: "sha1 signature", event: "push", delivery: "d4", body: push, signature: strings.Replace(sign(hookToken, []byte(push)), "sha256=", "sha1=", 1), status: 401, code: "invalid_signature"},
		{name: "signature not hex", event: "push", delivery: "d5", body: push, signature: "sha256=zz", status: 401, code: "invalid_signature"},
		{name: "ping", event: "ping", delivery: "d6", body: `{"zen":"Keep it simple."}`, status: 204},
		{name: "other event", event: "issues", delivery: "d7", body: `{"action":"opened"}`, status: 204},
		{name: "other branch", event: "push", delivery: "d8", body: pushPayload(shopRepo, "refs/heads/feature", pushSHA, false), status: 204},
		{name: "tag", event: "push", delivery: "d9", body: pushPayload(shopRepo, "refs/tags/v1.0", pushSHA, false), status: 204},
		{name: "deleted branch", event: "push", delivery: "d10", body: pushPayload(shopRepo, "refs/heads/main", strings.Repeat("0", 40), true), status: 204},
		{name: "unregistered repository", event: "push", delivery: "d11", body: pushPayload("acme/unknown", "refs/heads/main", pushSHA, false), status: 204},
		{name: "autoDeploy off only", event: "push", delivery: "d12", body: pushPayload("acme/manual-only", "refs/heads/main", pushSHA, false), status: 204},
		{name: "not JSON", event: "push", delivery: "d13", body: `not json`, status: 400, code: "invalid_request"},
		{name: "not a push payload", event: "push", delivery: "d14", body: `{"zen":"x"}`, status: 400, code: "invalid_request"},
		{name: "no delivery id", event: "push", body: push, status: 400, code: "invalid_request"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sig := tc.signature
			if sig == "" && tc.code != "invalid_signature" {
				sig = sign(hookToken, []byte(tc.body))
			}
			code, b := h.webhook(tc.event, tc.delivery, tc.body, sig)
			if code != tc.status {
				t.Fatalf("%d %s, want %d", code, b, tc.status)
			}
			if tc.code != "" && errorCode(t, b) != tc.code {
				t.Fatalf("body %s, want %s", b, tc.code)
			}
		})
	}
	for _, app := range []string{"shop", "shop-docs", "shop-manual", "shop-release", "manual", "other"} {
		if deps, _ := h.store.ListDeployments(t.Context(), app, 0); len(deps) != 0 {
			t.Fatalf("%s got deployments %+v", app, deps)
		}
	}
	if ids := dep.enqueued(); len(ids) != 0 {
		t.Fatalf("enqueued %v", ids)
	}

	// Bad signatures are audited as anonymous; nothing else is.
	audit, _ := h.store.ListAudit(t.Context(), 0, 50)
	if len(audit) != 5 {
		t.Fatalf("%d audit entries, want the 5 bad signatures: %+v", len(audit), audit)
	}
	for _, a := range audit {
		if a.Actor != "anonymous" || a.Status != 401 || strings.Contains(string(a.Detail), "refs/heads") {
			t.Fatalf("audit %+v %s", a, a.Detail)
		}
	}

	t.Run("a refused delivery is not recorded", func(t *testing.T) {
		// Sent unsigned first, the same delivery id still deploys once signed.
		if code, _ := h.webhook("push", "d-late", push, ""); code != 401 {
			t.Fatalf("unsigned: %d", code)
		}
		if code, b := h.webhook("push", "d-late", push, sign(hookToken, []byte(push))); code != 202 {
			t.Fatalf("signed: %d %s", code, b)
		}
	})
}

func TestWebhookNotConfigured(t *testing.T) {
	h := newHarness(t) // no webhook secret
	body := pushPayload(shopRepo, "refs/heads/main", pushSHA, false)
	if code, b := h.webhook("push", "d1", body, sign("", []byte(body))); code != 503 || errorCode(t, b) != "github_not_configured" {
		t.Fatalf("%d %s", code, b)
	}
}
