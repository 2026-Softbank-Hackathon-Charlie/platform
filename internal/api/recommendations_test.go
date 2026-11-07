package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/gateway"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/recommend"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/runs"
)

type recommendationLauncher struct{ jobs chan recommend.Launch }

func (r recommendationLauncher) StartRecommendation(_ context.Context, l recommend.Launch) error {
	r.jobs <- l
	return nil
}

func TestRecommendationRouteExistsWithoutAIConfiguration(t *testing.T) {
	h := newHarness(t)
	for _, path := range []string{"/v1/migration-recommendations", "/v1/migration-recommendations/status"} {
		code, raw := h.do("GET", path, viewerToken, "")
		if code != 200 || decode[recommend.Report](t, raw).Status != "not_configured" {
			t.Fatalf("unconfigured route must not be 404: %s %d", path, code)
		}
	}
}

func TestRecommendationInventoryUsesCatalogAndRegisteredAppsWithoutSecrets(t *testing.T) {
	routes := &fakeGatewayApps{}
	routes.set(gateway.AppInfo{Name: "moimroom", WriteBackend: "gke"}, gateway.AppInfo{Name: "shop", WriteBackend: "eks"})
	h := newHarness(t, withApps(""), withGitHub(shopGitHub(), &fakeDeployer{}), withSecrets(testCipher(t)), withAppDBTopology(), withRegistry(), func(_ *harness, _ *runs.Config, d *Deps) { d.Gateway = routes })
	first := h.registerShop()
	h.finishDeployment(first.Id, "healthy")
	candidates, err := h.api.loadRecommendationCandidates(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var catalogOnprem, registeredGKE bool
	var paths []string
	for _, c := range candidates {
		path := c.App
		if c.Source != nil {
			path += ":" + *c.Source
		}
		if c.Target != nil {
			path += "->" + *c.Target
		}
		paths = append(paths, path)
		if c.App == "moimroom" && c.Source != nil && *c.Source == "gke" && c.Target != nil && *c.Target == "onprem" {
			catalogOnprem = true
		}
		if c.App == "shop" && c.Source != nil && *c.Source == "eks" && c.Target != nil && *c.Target == "gke" {
			registeredGKE = true
		}
		if slices.Contains(c.AllowedActions, "move") || c.EstimatedMonthlySavingsUSD != nil {
			t.Fatal("missing full-workload estimates became a move/saving")
		}
	}
	if !catalogOnprem || !registeredGKE {
		t.Fatalf("inventory lost catalog/onprem=%v or registered/GKE=%v: %v", catalogOnprem, registeredGKE, paths)
	}
	raw, err := json.Marshal(candidates)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"PGPASSWORD", "moimroom-db-", ".ts.net", "gcs:", "s3:", "minio:", "passwordEnv", "backendURL"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("private inventory field leaked into recommendation snapshot: %s", forbidden)
		}
	}
}

func TestRecommendationUnknownWriteRouteNeverInventsEnvironment(t *testing.T) {
	for _, cause := range []string{"gateway_unavailable", "unknown_backend", "duplicate_backend"} {
		t.Run(cause, func(t *testing.T) {
			routes := &fakeGatewayApps{}
			routes.set(gateway.AppInfo{Name: "moimroom", WriteBackend: "gke"})
			h := newHarness(t, withApps(""), func(_ *harness, _ *runs.Config, d *Deps) {
				d.Gateway = routes
				switch cause {
				case "gateway_unavailable":
					routes.err = errors.New("unavailable")
				case "unknown_backend":
					routes.set(gateway.AppInfo{Name: "moimroom", WriteBackend: "unregistered"})
				case "duplicate_backend":
					environments := d.Envs.List(context.Background())
					for i := range environments {
						if environments[i].Name == "eks" {
							environments[i].GatewayBackend = "gke"
						}
					}
					d.Envs = fakeEnvs{list: environments}
				}
			})
			candidates, err := h.api.loadRecommendationCandidates(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(candidates) != 1 || candidates[0].Source != nil || candidates[0].Target != nil || slices.Contains(candidates[0].AllowedActions, "move") {
				t.Fatal("unknown or ambiguous source created a routable recommendation")
			}
		})
	}
}

func TestRecommendationAPIViewerReadAgentNonceAndCacheOnlyStatus(t *testing.T) {
	jobs := make(chan recommend.Launch, 2)
	source, target := "gke", "onprem"
	loads := 0
	svc := recommend.New(func(context.Context) ([]recommend.Candidate, error) {
		loads++
		return []recommend.Candidate{{Item: recommend.Item{ID: "shop-route", App: "shop", Source: &source, Target: &target, Title: "shop 이전 조건 비교", Reasons: []string{"입력 근거 확인"}, Missing: []string{}, Sources: []recommend.Source{}}, AllowedActions: []string{"stay", "insufficient"}}}, nil
	}, recommendationLauncher{jobs}, time.Now)
	h := newHarness(t, func(_ *harness, _ *runs.Config, d *Deps) { d.Recommendations = svc })
	if code, _ := h.do("GET", "/v1/migration-recommendations/status", viewerToken, ""); code != http.StatusOK || loads != 0 {
		t.Fatal("status caused evidence work")
	}
	if code, _ := h.do("GET", "/v1/migration-recommendations", "", ""); code != http.StatusUnauthorized {
		t.Fatal("anonymous budget consumption")
	}
	if code, _ := h.do("GET", "/v1/migration-recommendations", viewerToken, ""); code != http.StatusOK {
		t.Fatal("viewer could not obtain report")
	}
	var launch recommend.Launch
	select {
	case launch = <-jobs:
	case <-time.After(2 * time.Second):
		t.Fatal("recommendation Job not started")
	}
	body := `{"nonce":"wrong","snapshotId":"` + launch.ID + `","status":"ready","model":"claude-test","recommendations":[]}`
	if code, _ := h.do("POST", "/v1/migration-recommendations/"+launch.ID+"/result", viewerToken, body); code != http.StatusForbidden {
		t.Fatal("viewer posted AI result")
	}
	if code, _ := h.do("POST", "/v1/migration-recommendations/"+launch.ID+"/result", agentToken, body); code != http.StatusConflict {
		t.Fatal("wrong nonce accepted")
	}
	body = `{"nonce":"` + launch.Nonce + `","snapshotId":"` + launch.ID + `","status":"ready","model":"claude-test","recommendations":[{"id":"shop-route","action":"insufficient","reasons":["입력 근거 확인"]}]}`
	if code, _ := h.do("POST", "/v1/migration-recommendations/"+launch.ID+"/result", agentToken, body); code != http.StatusOK {
		t.Fatal("bound result rejected")
	}
	code, raw := h.do("GET", "/v1/migration-recommendations/status", viewerToken, "")
	report := decode[recommend.Report](t, raw)
	if code != http.StatusOK || report.Status != "ready" || len(report.Recommendations) != 1 || loads != 1 {
		t.Fatal("status contract or coalescing failed")
	}
	if code, _ := h.do("GET", "/v1/migration-recommendations?force=1", viewerToken, ""); code != http.StatusBadRequest {
		t.Fatal("query override accepted")
	}
}
