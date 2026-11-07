package api

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/gateway"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/runs"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

// TestStatelessAppIsMigratable: an app registered without a postgres
// resource in two environments (docker-doom) is listed with both, its
// plan is drafted without a database, and the AI runner refuses it (ADR
// 0049).
func TestStatelessAppIsMigratable(t *testing.T) {
	runner := &fakeRuns{}
	gw := &fakeGatewayApps{}
	h := newHarness(t, withApps(gatewayURL), withGitHub(shopGitHub(), &fakeDeployer{}), withoutPostgres(), withRegistry(), withAIRuns(runner),
		func(_ *harness, _ *runs.Config, d *Deps) { d.Plans.SetAppDatabases(d.Apps); d.Gateway = gw })
	if code, b := h.do("POST", "/v1/apps", opToken, map[string]any{"repo": shopRepo, "environments": []string{"gke", "eks"}}); code != 201 {
		t.Fatalf("create: %d %s", code, b)
	}
	code, b := h.do("GET", "/v1/apps/shop", viewerToken, nil)
	a := decode[apiv1.App](t, b)
	if code != 200 || len(a.Environments) != 2 || a.Environments[0].Env != "gke" || a.Environments[1].Env != "eks" ||
		a.Environments[0].CnpgCluster != "" || a.Resources != (apiv1.AppResources{}) || a.Database.Name != "" || a.Deploy == nil {
		t.Fatalf("app: %d %s", code, b)
	}
	code, b = h.do("GET", "/v1/apps", viewerToken, nil)
	list := decode[apiv1.AppList](t, b)
	for _, x := range list.Apps {
		if x.Name == "moimroom" && !x.Resources.Postgres {
			t.Errorf("moimroom resources %+v", x.Resources)
		}
		if x.Name == "shop" && len(x.Environments) != 2 {
			t.Errorf("listed shop %+v", x)
		}
	}

	// The gateway must route shop to both environments already.
	draft := apiv1.PlanDraftRequest{App: "shop", Source: "eks", Target: "gke"}
	gw.set(gateway.AppInfo{Name: "shop", Backends: map[string]string{"eks": "http://e"}})
	if code, b := h.do("POST", "/v1/plans/draft", opToken, draft); code != 400 || errCode(t, b) != "gateway_backend_missing" {
		t.Fatalf("draft without the gke backend: %d %s", code, b)
	}
	gw.set(gateway.AppInfo{Name: "shop", Backends: map[string]string{"eks": "http://e", "gke": "http://g"}})
	code, b = h.do("POST", "/v1/plans/draft", opToken, draft)
	p := decode[apiv1.EnginePlan](t, b)
	if code != 200 || p.Source.Kind == nil || *p.Source.Kind != apiv1.EnginePlanEndpointKindNone || p.Source.Dsn != "" ||
		p.Gateway == nil || *p.Gateway.Url != gatewayURL+"/apps/shop" || p.Files != nil {
		t.Fatalf("draft: %d %s", code, b)
	}

	if code, b := h.do("POST", "/v1/ai-runs", opToken, apiv1.AIRunRequest{App: "shop", Source: "eks", Target: "gke"}); code != 409 || errCode(t, b) != "no_database" {
		t.Fatalf("ai run: %d %s", code, b)
	}
	if len(runner.started) != 0 {
		t.Fatalf("started %+v", runner.started)
	}

	// The draft goes through submit, approval and start like any plan.
	raw, _ := json.Marshal(p)
	ref := h.approved(string(raw))
	code, b = h.do("POST", "/v1/migrations", opToken, apiv1.StartMigrationRequest{PlanRef: ref})
	if code != 202 {
		t.Fatalf("start: %d %s", code, b)
	}
	started := decode[apiv1.StartMigrationResponse](t, b)
	_, b = h.do("GET", "/v1/migrations/"+started.RunId, viewerToken, nil)
	if run := decode[apiv1.Run](t, b); run.App == nil || *run.App != "shop" {
		t.Fatalf("run %s", b)
	}
	// Another app's scope, or a database app's, is refused at submit.
	other := strings.Replace(string(raw), "/apps/shop", "/apps/moimroom", 1)
	if code, b := h.do("POST", "/v1/plans", agentToken, other); code != 400 {
		t.Fatalf("moimroom's scope without its database: %d %s", code, b)
	}
}
