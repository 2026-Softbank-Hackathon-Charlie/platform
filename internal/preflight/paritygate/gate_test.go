package paritygate

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	authorizationclient "k8s.io/client-go/kubernetes/typed/authorization/v1"
	"k8s.io/client-go/rest"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/preflight/parity"
)

const config = `
version: 1
mode: audit
environments:
  eks: {prometheus: {url: "PROM", maxAge: 1m}}
apps:
  - name: shop
    namespace: shop
    serviceAccount: shop-api
    prometheusJob: shop-api
    rbac:
      - {id: read-own-config, verb: get, resource: configmaps, allowed: true}
      - {id: no-secrets, verb: list, resource: secrets, allowed: false}
`

// sarServer answers SubjectAccessReviews: shop-api may read configmaps in
// its namespace and nothing else, unless broken is set.
func sarServer(t *testing.T, broken *atomic.Bool) authorizationclient.SubjectAccessReviewInterface {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var review authorizationv1.SubjectAccessReview
		if err := json.NewDecoder(r.Body).Decode(&review); err != nil || review.Spec.User != "system:serviceaccount:shop:shop-api" {
			t.Errorf("review %+v err %v", review.Spec, err)
		}
		a := review.Spec.ResourceAttributes
		allowed := a.Namespace == "shop" && a.Verb == "get" && a.Resource == "configmaps"
		if broken != nil && broken.Load() {
			allowed = true // the target grants the app every secret
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(authorizationv1.SubjectAccessReview{TypeMeta: metav1.TypeMeta{APIVersion: "authorization.k8s.io/v1", Kind: "SubjectAccessReview"},
			Status: authorizationv1.SubjectAccessReviewStatus{Allowed: allowed}})
	}))
	t.Cleanup(s.Close)
	c, err := authorizationclient.NewForConfig(&rest.Config{Host: s.URL, ContentConfig: rest.ContentConfig{ContentType: "application/json"}})
	if err != nil {
		t.Fatal(err)
	}
	return c.SubjectAccessReviews()
}

// promServer reports the app's one target as up, scraped 5 seconds ago.
func promServer(t *testing.T, up string) string {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		now, _ := strconv.ParseFloat(r.URL.Query().Get("time"), 64)
		metric := map[string]string{"namespace": "shop", "job": "shop-api", "instance": "pod-1"}
		value := up
		if strings.HasPrefix(r.URL.Query().Get("query"), "timestamp(") {
			value = strconv.FormatFloat(now-5, 'f', 6, 64)
		}
		json.NewEncoder(w).Encode(map[string]any{"status": "success", "data": map[string]any{"resultType": "vector",
			"result": []any{map[string]any{"metric": metric, "value": []any{now, value}}}}})
	}))
	t.Cleanup(s.Close)
	return s.URL
}

func shopPlan() *plan.Plan {
	return &plan.Plan{ID: "shop-move",
		Source: plan.Endpoint{Env: "gke", Namespace: "shop", Cluster: "db"},
		Target: plan.Endpoint{Env: "eks", Namespace: "shop", Cluster: "db"}}
}

func newGate(t *testing.T, promURL string, sar SARFactory) *Gate {
	t.Helper()
	cfg, err := ParseConfig([]byte(strings.Replace(config, "PROM", promURL, 1)), []string{"gke", "eks"})
	if err != nil {
		t.Fatal(err)
	}
	return New(cfg, sar, nil)
}

func TestProbeFixtureAcceptsIPv6LoopbackURL(t *testing.T) {
	newGate(t, "http://[::1]:9090", nil)
}

func result(r parity.Report, c parity.Capability) parity.Result {
	for _, x := range r.Results {
		if x.Capability == c {
			return x
		}
	}
	return parity.Result{}
}

func TestCheckRecordsRealProbeResults(t *testing.T) {
	var broken atomic.Bool
	client := sarServer(t, &broken)
	var asked []string
	g := newGate(t, promServer(t, "1"), func(env string) (authorizationclient.SubjectAccessReviewInterface, error) {
		asked = append(asked, env)
		return client, nil
	})
	r, err := g.Check(context.Background(), shopPlan())
	if err != nil {
		t.Fatal(err)
	}
	id, obs := result(r, parity.Identity), result(r, parity.Observability)
	if id.Status != parity.Pass || id.Code != "rbac_verified" || len(id.Checks) != 2 || len(asked) != 1 || asked[0] != "eks" {
		t.Fatalf("identity %+v asked %v", id, asked)
	}
	// The scrape passes; alert delivery is not probed, so the capability is unknown.
	if obs.Status != parity.Unknown || obs.Checks[0].Status != parity.Pass || obs.Checks[0].Code != "scrape_verified" {
		t.Fatalf("observability %+v", obs)
	}
	for _, c := range []parity.Capability{parity.Secret, parity.Audit, parity.BackupRestore} {
		if x := result(r, c); x.Status != parity.Unknown || x.Code != "probe_not_configured" {
			t.Fatalf("%s %+v", c, x)
		}
	}
	if r.Passed {
		t.Fatal("a report with unprobed capabilities must not pass")
	}
	lines := strings.Join(Lines(r), "\n")
	for _, want := range []string{
		"identity pass rbac_verified: read-own-config=pass(authorization_matches), no-secrets=pass(authorization_matches)",
		"metrics-alerting unknown observability_incomplete: metrics-target=pass(scrape_verified), alert-delivery=unknown(alert_probe_not_implemented)",
		"secret unknown probe_not_configured",
	} {
		if !strings.Contains(lines, want) {
			t.Fatalf("lines missing %q:\n%s", want, lines)
		}
	}

	broken.Store(true)
	r, err = g.Check(context.Background(), shopPlan())
	if id := result(r, parity.Identity); err != nil || id.Status != parity.Fail || id.Code != "rbac_mismatch" {
		t.Fatalf("over-permissive target: %+v %v", id, err)
	}
}

func TestCheckProbeProblemsAreRecordedNotRaised(t *testing.T) {
	g := newGate(t, promServer(t, "0"), func(string) (authorizationclient.SubjectAccessReviewInterface, error) {
		return nil, errors.New("no kubeconfig context")
	})
	r, err := g.Check(context.Background(), shopPlan())
	if err != nil {
		t.Fatal(err)
	}
	if id := result(r, parity.Identity); id.Status != parity.Unknown || id.Code != "probe_failed" {
		t.Fatalf("identity %+v", id)
	}
	if obs := result(r, parity.Observability); obs.Status != parity.Fail || obs.Checks[0].Code != "target_not_up" {
		t.Fatalf("observability %+v", obs)
	}

	// No SAR factory and no Prometheus for the target: both unconfigured.
	g = newGate(t, "http://prom.invalid:9090", nil)
	p := shopPlan()
	p.Target.Env, p.Source.Env = "gke", "eks"
	r, err = g.Check(context.Background(), p)
	if err != nil || result(r, parity.Identity).Code != "probe_not_configured" || result(r, parity.Observability).Code != "probe_not_configured" {
		t.Fatalf("%+v %v", r, err)
	}
}

func TestCheckWithoutAppIsNotConfigured(t *testing.T) {
	g := newGate(t, "http://prom.invalid:9090", nil)
	p := shopPlan()
	p.Target.Namespace = "other"
	if _, err := g.Check(context.Background(), p); !errors.Is(err, parity.ErrNotConfigured) {
		t.Fatalf("err %v", err)
	}
}

func TestCheckIsBounded(t *testing.T) {
	block := make(chan struct{})
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-block }))
	defer func() { close(block); s.Close() }()
	g := newGate(t, s.URL, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	r, _ := g.Check(ctx, shopPlan())
	if time.Since(start) > 5*time.Second || r.Passed {
		t.Fatalf("took %v, %+v", time.Since(start), r)
	}
}

func TestParseConfig(t *testing.T) {
	envs := []string{"gke", "eks"}
	if _, err := ParseConfig([]byte(strings.Replace(config, "PROM", "http://prometheus.monitoring:9090", 1)), envs); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string][2]string{
		"blocking mode":     {"mode: audit", "mode: enforce"},
		"unknown field":     {"mode: audit", "mode: audit\nblock: true"},
		"unregistered env":  {"  eks: {", "  mars: {"},
		"prometheus creds":  {"url: PROM", "url: http://user:pw@prom:9090"},
		"prometheus query":  {"url: PROM", "url: http://prom:9090/?x=1"},
		"maxAge too long":   {"maxAge: 1m", "maxAge: 1h"},
		"only allowed":      {"verb: list, resource: secrets, allowed: false", "verb: list, resource: secrets, allowed: true"},
		"wildcard verb":     {"verb: get, resource: configmaps", "verb: '*', resource: configmaps"},
		"no account":        {"    serviceAccount: shop-api\n", ""},
		"bad namespace":     {"namespace: shop", "namespace: Shop"},
		"duplicate rule id": {"id: no-secrets", "id: read-own-config"},
		"allowed omitted":   {"resource: configmaps, allowed: true}", "resource: configmaps}"},
	} {
		t.Run(name, func(t *testing.T) {
			raw := strings.Replace(strings.Replace(config, change[0], change[1], 1), "PROM", "http://prometheus.monitoring:9090", 1)
			if _, err := ParseConfig([]byte(raw), envs); !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("accepted: %v", err)
			}
		})
	}
}
