package gate

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/preflight"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/preflight/inventory"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/preflight/residency"
)

const policy = `
version: 1
environments:
  - {id: onprem-kr, country: KR}
  - {id: eks-kr, country: KR}
  - {id: eks-jp, country: JP}
  - {id: gke-us, country: US}
apacCountries: [KR, JP]
catalog:
  - scope: {environment: onprem-kr, namespace: shop, cluster: shop-db}
    datasets:
      - {dataset: {database: app, schema: public, table: products}, label: global}
      - {dataset: {database: app, schema: public, table: customers}, label: LABEL}
buckets:
  - {bucket: "s3:photos", environment: onprem-kr, label: kr-only}
  - {bucket: "gcs:photos-tokyo", environment: eks-jp, label: global}
`

const secretDSN = "postgres://app:TOPSECRET@db.internal.example:5432/app"

type fakeCollector struct {
	inv   preflight.Inventory
	res   inventory.Result
	dsn   string
	scope preflight.ClusterScope
}

func (f *fakeCollector) Collect(_ context.Context, dsn string, scope preflight.ClusterScope) (preflight.Inventory, inventory.Result) {
	f.dsn, f.scope = dsn, scope
	inv := f.inv
	inv.Scope = scope
	return inv, f.res
}

func complete() *fakeCollector {
	return &fakeCollector{inv: preflight.Inventory{Complete: true, Datasets: []preflight.Dataset{
		{Database: "app", Schema: "public", Table: "customers"},
		{Database: "app", Schema: "public", Table: "products"},
	}}}
}

func newGate(t *testing.T, label string, c Collector) *Gate {
	t.Helper()
	cfg, err := residency.ParseConfig([]byte(strings.Replace(policy, "LABEL", label, 1)))
	if err != nil {
		t.Fatal(err)
	}
	g, err := New(context.Background(), cfg, c)
	if err != nil {
		t.Fatal(err)
	}
	if len(g.PolicyDigest()) != 64 {
		t.Fatalf("digest %q", g.PolicyDigest())
	}
	return g
}

func shopPlan(target string) *plan.Plan {
	return &plan.Plan{
		ID:     "shop-move",
		Source: plan.Endpoint{Env: "onprem-kr", Namespace: "shop", Cluster: "shop-db", DSN: secretDSN},
		Target: plan.Endpoint{Env: target, Namespace: "shop", Cluster: "shop-db-new"},
	}
}

func codes(v preflight.ResidencyVerdict) []string {
	var out []string
	for _, r := range v.Reasons {
		out = append(out, r.Code)
	}
	return out
}

func TestCheckAppliesPolicyToCollectedInventory(t *testing.T) {
	for _, tc := range []struct {
		label, target string
		allowed       bool
	}{
		{"kr-only", "eks-kr", true},  // moving inside Korea
		{"kr-only", "eks-jp", false}, // one kr-only table blocks the whole cluster
		{"apac", "eks-jp", true},
		{"apac", "gke-us", false},
		{"global", "gke-us", true},
	} {
		t.Run(tc.label+"->"+tc.target, func(t *testing.T) {
			c := complete()
			g := newGate(t, tc.label, c)
			v, err := g.Check(context.Background(), shopPlan(tc.target))
			if err != nil || v.Allowed != tc.allowed || v.PolicyDigest != g.PolicyDigest() {
				t.Fatalf("verdict %+v err %v", v, err)
			}
			if !tc.allowed && !slices.Contains(codes(v), "country_not_allowed") {
				t.Fatalf("reasons %v", codes(v))
			}
			if c.dsn != secretDSN || c.scope != (preflight.ClusterScope{Environment: "onprem-kr", Namespace: "shop", Cluster: "shop-db"}) {
				t.Fatalf("collector got scope %+v", c.scope)
			}
		})
	}
}

func TestCheckDenies(t *testing.T) {
	for name, tc := range map[string]struct {
		change  func(*plan.Plan, *fakeCollector)
		want    []string
		wantErr bool
	}{
		"files to a bucket the policy does not register": {
			change: func(p *plan.Plan, _ *fakeCollector) {
				p.Files = &plan.FilesSpec{Source: "s3:photos", Target: "gcs:elsewhere"}
			},
			want: []string{"unregistered_bucket"},
		},
		"files with a kr-only bucket to Japan": {
			change: func(p *plan.Plan, _ *fakeCollector) {
				p.Target.Env = "eks-jp"
				p.Files = &plan.FilesSpec{Source: "s3:photos", Target: "gcs:photos-tokyo"}
			},
			want: []string{"bucket_country_not_allowed"},
		},
		"collector failed": {
			change: func(_ *plan.Plan, c *fakeCollector) {
				c.inv.Complete, c.res = false, inventory.Result{Code: inventory.CodeConnectFailed}
			},
			want: []string{CodeInventoryIncomplete, "inventory:connect_failed"},
		},
		"incomplete without a code": {
			change: func(_ *plan.Plan, c *fakeCollector) { c.inv.Complete = false },
			want:   []string{CodeInventoryIncomplete, "inventory:unknown"},
		},
		"new uncataloged table": {
			change: func(_ *plan.Plan, c *fakeCollector) {
				c.inv.Datasets = append(c.inv.Datasets, preflight.Dataset{Database: "app", Schema: "public", Table: "payments"})
			},
			want: []string{"unclassified_dataset"},
		},
		"uncataloged cluster": {
			change: func(p *plan.Plan, _ *fakeCollector) { p.Source.Cluster = "other-db" },
			want:   []string{"unclassified_cluster"},
		},
		"identifier outside the policy": {
			change:  func(p *plan.Plan, _ *fakeCollector) { p.Source.Namespace = "Shop" },
			want:    []string{CodeInvalidInput},
			wantErr: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			c := complete()
			p := shopPlan("eks-kr")
			tc.change(p, c)
			v, err := newGate(t, "global", c).Check(context.Background(), p)
			if v.Allowed || (err != nil) != tc.wantErr {
				t.Fatalf("verdict %+v err %v", v, err)
			}
			for _, code := range tc.want {
				if !slices.Contains(codes(v), code) {
					t.Fatalf("reasons %v, want %v", codes(v), tc.want)
				}
			}
			b, _ := json.Marshal(v)
			for _, secret := range []string{"TOPSECRET", "db.internal.example", "postgres://"} {
				if strings.Contains(string(b), secret) {
					t.Fatalf("verdict leaks %q: %s", secret, b)
				}
			}
		})
	}
}

func TestCheckPassesFilesToThePolicy(t *testing.T) {
	p := shopPlan("eks-kr")
	p.Files = &plan.FilesSpec{Source: "s3:photos", Target: "gcs:photos-tokyo"}
	// kr-only photos may not go to the Tokyo bucket, even on a Korean move.
	v, err := newGate(t, "global", complete()).Check(context.Background(), p)
	if err != nil || v.Allowed || !slices.Contains(codes(v), "bucket_country_not_allowed") {
		t.Fatalf("verdict %+v err %v", v, err)
	}
	p.Files = nil
	if v, err := newGate(t, "global", complete()).Check(context.Background(), p); err != nil || !v.Allowed {
		t.Fatalf("without files: %+v %v", v, err)
	}
}

func TestCheckNotReady(t *testing.T) {
	var g *Gate
	if v, err := g.Check(context.Background(), shopPlan("eks-kr")); err == nil || v.Allowed {
		t.Fatalf("nil gate: %+v %v", v, err)
	}
	if v, err := newGate(t, "global", complete()).Check(context.Background(), nil); err == nil || v.Allowed {
		t.Fatalf("nil plan: %+v %v", v, err)
	}
}
