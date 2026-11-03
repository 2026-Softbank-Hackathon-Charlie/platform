package api

import (
	"bufio"
	"context"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/deploy"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/runs"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

// fakeRemover records the apps it removed; err fails the removal.
type fakeRemover struct {
	mu      sync.Mutex
	err     error
	removed []string
}

func (f *fakeRemover) RemoveApp(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.removed = append(f.removed, name)
	return nil
}

func withRemover(rm *fakeRemover) option {
	return func(_ *harness, _ *runs.Config, d *Deps) { d.Remover = rm }
}

// registerShop registers acme/shop for gke and eks and returns its first deployment.
func (h *harness) registerShop() apiv1.Deployment {
	h.t.Helper()
	code, b := h.do("POST", "/v1/apps", opToken, map[string]any{"repo": shopRepo, "environments": []string{"gke", "eks"}})
	if code != 201 {
		h.t.Fatalf("register: %d %s", code, b)
	}
	return decode[apiv1.AppCreated](h.t, b).Deployment
}

func TestCreateDeployment(t *testing.T) {
	gh, dep := shopGitHub(), &fakeDeployer{}
	gh.repo(shopRepo, "broken", pushSHA, "name: shop\nservices:\n  - name: web\n    port: 0\n", "Dockerfile")
	gh.repo(shopRepo, "renamed", "2222222222222222222222222222222222222222", strings.Replace(shopSpec, "name: shop", "name: store", 1), "Dockerfile", "worker/Dockerfile")
	h := newHarness(t, withApps(""), withGitHub(gh, dep))
	first := h.registerShop()

	if code, b := h.do("POST", "/v1/apps/shop/deployments", viewerToken, nil); code != 403 {
		t.Fatalf("viewer deploys: %d %s", code, b)
	}
	code, b := h.do("POST", "/v1/apps/shop/deployments", opToken, nil)
	if code != 202 {
		t.Fatalf("deploy: %d %s", code, b)
	}
	d := decode[apiv1.Deployment](t, b)
	if d.Trigger != apiv1.DeploymentTriggerManual || d.TriggeredBy != "op" || d.Ref != "main" || d.Commit != headSHA ||
		d.Status != apiv1.DeploymentStatusQueued || len(d.Environments) != 2 || d.Environments[0].Env != "gke" || d.Id == first.Id {
		t.Fatalf("deployment %+v", d)
	}
	if ids := dep.enqueued(); !slices.Equal(ids, []string{first.Id, d.Id}) {
		t.Fatalf("enqueued %v", ids)
	}
	audit, _ := h.store.ListAudit(t.Context(), 0, 1)
	if a := audit[0]; a.Action != "POST /v1/apps/{name}/deployments" || a.Status != 202 || !strings.Contains(string(a.Detail), d.Id) || !strings.Contains(string(a.Detail), headSHA) {
		t.Fatalf("audit %+v %s", a, a.Detail)
	}

	// A subset, in the app's order whatever the order asked.
	code, b = h.do("POST", "/v1/apps/shop/deployments", opToken, map[string]any{"environments": []string{"eks"}, "ref": "main"})
	if d := decode[apiv1.Deployment](t, b); code != 202 || len(d.Environments) != 1 || d.Environments[0].Env != "eks" || d.Ref != "main" {
		t.Fatalf("subset: %d %s", code, b)
	}
	code, b = h.do("POST", "/v1/apps/shop/deployments", opToken, map[string]any{"environments": []string{"eks", "gke"}})
	if d := decode[apiv1.Deployment](t, b); code != 202 || d.Environments[0].Env != "gke" {
		t.Fatalf("order: %d %s", code, b)
	}

	for _, tc := range []struct {
		name, app string
		body      any
		status    int
		code      string
	}{
		{"environment not registered", "shop", map[string]any{"environments": []string{"onprem"}}, 400, "invalid_request"},
		{"environment twice", "shop", map[string]any{"environments": []string{"eks", "eks"}}, 400, "invalid_request"},
		{"no environments", "shop", map[string]any{"environments": []string{}}, 400, "invalid_request"},
		{"empty ref", "shop", map[string]any{"ref": ""}, 400, "invalid_request"},
		{"unknown ref", "shop", map[string]any{"ref": "nope"}, 400, "invalid_request"},
		{"unknown field", "shop", map[string]any{"branch": "main"}, 400, "invalid_request"},
		{"invalid spec", "shop", map[string]any{"ref": "broken"}, 422, "invalid_app_spec"},
		{"renamed app", "shop", map[string]any{"ref": "renamed"}, 422, "invalid_app_spec"},
		{"file app", "moimroom", nil, 409, "app_not_registered"},
		{"unknown app", "nope", nil, 404, "app_not_found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, b := h.do("POST", "/v1/apps/"+tc.app+"/deployments", opToken, tc.body)
			if code != tc.status || errorCode(t, b) != tc.code {
				t.Fatalf("%d %s, want %d %s", code, b, tc.status, tc.code)
			}
			if tc.status == 422 {
				inv := decode[apiv1.AppSpecInvalid](t, b)
				if inv.Validation.Valid || len(inv.Validation.Errors) == 0 {
					t.Fatalf("validation %+v", inv.Validation)
				}
				if tc.name == "renamed app" && inv.Validation.Errors[0].Path != "name" {
					t.Fatalf("errors %+v", inv.Validation.Errors)
				}
			}
		})
	}
	if n := len(dep.enqueued()); n != 4 {
		t.Errorf("enqueued %d deployments, want 4", n)
	}

	gh.fail(errors.New("connection reset"))
	if code, b := h.do("POST", "/v1/apps/shop/deployments", opToken, nil); code != 502 || errorCode(t, b) != "github_unreachable" {
		t.Fatalf("github down: %d %s", code, b)
	}
}

func TestCreateDeploymentWithoutGitHub(t *testing.T) {
	h := newHarness(t, withApps(""))
	if err := h.store.CreateApp(t.Context(), store.App{Name: "shop", Repo: shopRepo, Branch: "main", Environments: []string{"gke"}, Spec: []byte(`{}`), CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if code, b := h.do("POST", "/v1/apps/shop/deployments", opToken, nil); code != 503 || errorCode(t, b) != "github_not_configured" {
		t.Fatalf("%d %s", code, b)
	}
}

func TestListAndGetDeployments(t *testing.T) {
	h := newHarness(t, withApps(""), withGitHub(shopGitHub(), nil))
	first := h.registerShop()
	_, b := h.do("POST", "/v1/apps/shop/deployments", opToken, nil)
	second := decode[apiv1.Deployment](t, b)

	code, b := h.do("GET", "/v1/apps/shop/deployments", viewerToken, nil)
	list := decode[apiv1.DeploymentList](t, b).Deployments
	if code != 200 || len(list) != 2 || list[0].Id != second.Id || list[1].Id != first.Id {
		t.Fatalf("list: %d %s", code, b)
	}
	if code, b = h.do("GET", "/v1/apps/shop/deployments?limit=1", viewerToken, nil); code != 200 || len(decode[apiv1.DeploymentList](t, b).Deployments) != 1 {
		t.Fatalf("limit: %d %s", code, b)
	}
	if code, b = h.do("GET", "/v1/apps/shop/deployments?limit=0", viewerToken, nil); code != 400 {
		t.Fatalf("limit 0: %d %s", code, b)
	}
	if code, b = h.do("GET", "/v1/apps/moimroom/deployments", viewerToken, nil); code != 200 || len(decode[apiv1.DeploymentList](t, b).Deployments) != 0 {
		t.Fatalf("file app: %d %s", code, b)
	}
	if code, b = h.do("GET", "/v1/apps/nope/deployments", viewerToken, nil); code != 404 || errorCode(t, b) != "app_not_found" {
		t.Fatalf("unknown app: %d %s", code, b)
	}
	if code, b = h.do("GET", "/v1/deployments/"+first.Id, viewerToken, nil); code != 200 || decode[apiv1.Deployment](t, b).Trigger != apiv1.DeploymentTriggerRegister {
		t.Fatalf("get: %d %s", code, b)
	}
	if code, b = h.do("GET", "/v1/deployments/dep_nope", viewerToken, nil); code != 404 || errorCode(t, b) != "deployment_not_found" {
		t.Fatalf("get unknown: %d %s", code, b)
	}
}

func TestDeleteApp(t *testing.T) {
	rm := &fakeRemover{}
	h := newHarness(t, withApps(""), withGitHub(shopGitHub(), nil), withRemover(rm))
	h.registerShop()

	for _, tc := range []struct {
		name, app, token string
		err              error
		status           int
		code             string
	}{
		{"viewer", "shop", viewerToken, nil, 403, "forbidden"},
		{"file app", "moimroom", opToken, nil, 409, "app_not_registered"},
		{"unknown app", "nope", opToken, nil, 404, "app_not_found"},
		{"writes held", "shop", opToken, deploy.ErrWritesHeld, 409, "cluster_busy"},
		{"gitops down", "shop", opToken, deploy.ErrGitOps, 502, "github_unreachable"},
		{"gateway down", "shop", opToken, deploy.ErrGateway, 502, "gateway_unreachable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rm.err = tc.err
			code, b := h.do("DELETE", "/v1/apps/"+tc.app, tc.token, nil)
			if code != tc.status || errorCode(t, b) != tc.code {
				t.Fatalf("%d %s, want %d %s", code, b, tc.status, tc.code)
			}
		})
	}
	rm.err = nil
	if code, b := h.do("DELETE", "/v1/apps/shop", opToken, nil); code != 202 || len(b) != 0 || !slices.Equal(rm.removed, []string{"shop"}) {
		t.Fatalf("delete: %d %s, removed %v", code, b, rm.removed)
	}
	audit, _ := h.store.ListAudit(t.Context(), 0, 1)
	if a := audit[0]; a.Action != "DELETE /v1/apps/{name}" || a.Status != 202 || !strings.Contains(string(a.Detail), `"app":"shop"`) {
		t.Fatalf("audit %+v", a)
	}

	h2 := newHarness(t, withApps(""), withGitHub(shopGitHub(), nil))
	h2.registerShop()
	if code, b := h2.do("DELETE", "/v1/apps/shop", opToken, nil); code != 503 || errorCode(t, b) != "github_not_configured" {
		t.Fatalf("no remover: %d %s", code, b)
	}
}

// deploymentStream reads a deployment's event stream until it closes or
// stop is closed, checking every payload against openapi.yaml.
func (h *harness) deploymentStream(id, lastEventID string, stop <-chan struct{}) []sseMsg {
	h.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-stop:
			cancel()
		case <-ctx.Done():
		}
	}()
	req, _ := http.NewRequestWithContext(ctx, "GET", h.srv.URL+"/v1/deployments/"+id+"/events", nil)
	req.Header.Set("Authorization", "Bearer "+viewerToken)
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Error(err)
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" {
		h.t.Errorf("events: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
		return nil
	}
	var out []sseMsg
	var cur sseMsg
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if cur.event != "" {
				schema := "DeploymentEvent"
				if cur.event == "snapshot" {
					schema = "Deployment"
				}
				h.validateSchema(schema, []byte(cur.data))
				out = append(out, cur)
			}
			cur = sseMsg{}
		case strings.HasPrefix(line, "id: "):
			cur.id, _ = strconv.ParseInt(strings.TrimPrefix(line, "id: "), 10, 64)
		case strings.HasPrefix(line, "event: "):
			cur.event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			cur.data = strings.TrimPrefix(line, "data: ")
		}
	}
	return out
}

func TestDeploymentEventStream(t *testing.T) {
	old := DeploymentEventPoll
	DeploymentEventPoll = 10 * time.Millisecond
	t.Cleanup(func() { DeploymentEventPoll = old })
	h := newHarness(t, withApps(""), withGitHub(shopGitHub(), nil))
	d := h.registerShop()
	ctx := t.Context()
	appendEv := func(typ, phase, env, msg string) int64 {
		ev, err := h.store.AppendDeploymentEvent(ctx, store.DeploymentEvent{DeploymentID: d.Id, Time: time.Now().UTC(), Type: typ, Phase: phase, Env: env, Message: msg})
		if err != nil {
			t.Fatal(err)
		}
		return ev.Seq
	}

	// Live: the stream stays open while the deployment runs and closes
	// after it finishes, with a final snapshot.
	got := make(chan []sseMsg, 1)
	go func() { got <- h.deploymentStream(d.Id, "", nil) }()
	time.Sleep(30 * time.Millisecond)
	appendEv(deploy.EventPhase, store.DeployBuilding, "", "reading platform.yaml")
	seq := appendEv(deploy.EventLog, store.DeployBuilding, "", "Step 1/2")
	appendEv(deploy.EventEnvironment, store.DeploySyncing, "gke", "Synced, Healthy")
	stored, _ := h.store.GetDeployment(ctx, d.Id)
	now := time.Now().UTC()
	stored.Status, stored.FinishedAt = store.DeployHealthy, &now
	if _, err := h.store.UpdateDeployment(ctx, stored); err != nil {
		t.Fatal(err)
	}
	appendEv(deploy.EventDone, store.DeployHealthy, "", "every environment is synced and healthy")
	var msgs []sseMsg
	select {
	case msgs = <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("the stream did not close after the deployment finished")
	}
	want := []string{"snapshot", "phase", "log", "environment", "done", "snapshot"}
	if n := names(msgs); !slices.Equal(n, want) {
		t.Fatalf("messages %v, want %v", n, want)
	}
	if !strings.Contains(msgs[0].data, `"status":"queued"`) || !strings.Contains(msgs[5].data, `"status":"healthy"`) || msgs[0].id != 0 || msgs[2].id != seq {
		t.Fatalf("snapshots or ids: %+v", msgs)
	}
	if !strings.Contains(msgs[3].data, `"env":"gke"`) {
		t.Fatalf("environment event %s", msgs[3].data)
	}

	// Resume after the log line: only what came later.
	msgs = h.deploymentStream(d.Id, strconv.FormatInt(seq, 10), nil)
	if n := names(msgs); !slices.Equal(n, []string{"snapshot", "environment", "done", "snapshot"}) {
		t.Fatalf("resumed %v", n)
	}
	// Resume after the end: a finished deployment closes after its snapshots.
	msgs = h.deploymentStream(d.Id, strconv.FormatInt(seq+10, 10), nil)
	if n := names(msgs); !slices.Equal(n, []string{"snapshot", "snapshot"}) {
		t.Fatalf("after the end %v", n)
	}
	if code, b := h.do("GET", "/v1/deployments/"+d.Id+"/events?after=x", viewerToken, nil); code != 400 {
		t.Fatalf("bad after: %d %s", code, b)
	}
	if code, b := h.do("GET", "/v1/deployments/dep_nope/events", viewerToken, nil); code != 404 || errorCode(t, b) != "deployment_not_found" {
		t.Fatalf("unknown: %d %s", code, b)
	}

	// A queued deployment's stream stays open (no deployer here).
	_, b := h.do("POST", "/v1/apps/shop/deployments", opToken, nil)
	queued := decode[apiv1.Deployment](t, b)
	stop := make(chan struct{})
	time.AfterFunc(100*time.Millisecond, func() { close(stop) })
	if msgs := h.deploymentStream(queued.Id, "", stop); len(msgs) != 1 || msgs[0].event != "snapshot" {
		t.Fatalf("queued stream %v", names(msgs))
	}
}
