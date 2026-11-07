package plans

import (
	"strings"
	"testing"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"
)

// statelessPolicy knows docker-doom as an app without a database in eks and
// gke, and moimroom as an app with one (ADR 0049).
func statelessPolicy() Policy {
	p := DefaultPolicy()
	p.GatewayURL = "http://gw:9901"
	p.AppDBHosts = map[string][]string{"moimroom": {"db-eks:5432", "db-gke:5432"}}
	envs := map[string]StatelessEnv{"eks": {Backend: "eks", Bucket: "eks:hopcharlie-eks-docker-doom"}, "gke": {Backend: "gke-backend", Bucket: "gke:hopcharlie-gke-docker-doom"}}
	p.StatelessApps = map[string]map[string]StatelessEnv{"docker-doom": envs, "moimroom": envs}
	p.Buckets = []string{"eks:hopcharlie-eks-docker-doom", "gke:hopcharlie-gke-docker-doom", "gke:hopcharlie-gke-moimroom"}
	return p
}

// statelessPlan is the plan Draft makes for docker-doom, as plan.Parse
// decodes it: the endpoints are only an environment.
func statelessPlan(gatewayURL string) *plan.Plan {
	p := &plan.Plan{ID: "docker-doom-eks-to-gke", Source: plan.Endpoint{Kind: KindNone, Env: "eks"}, Target: plan.Endpoint{Kind: KindNone, Env: "gke"}}
	p.Gateway = plan.GatewaySpec{Mode: "http", URL: gatewayURL, Token: "${GATEWAY_TOKEN}", SourceBackend: "eks", TargetBackend: "gke-backend"}
	return p
}

func TestNoDatabaseKinds(t *testing.T) {
	p := statelessPolicy()
	h, err := p.checkRefs([]byte(`{"source":{"kind":"none","env":"eks"},"target":{"kind":"none","env":"gke"},"gateway":{"token":"${GATEWAY_TOKEN}"}}`))
	if err != nil || !h.noDatabase || len(h.all) != 0 {
		t.Fatalf("both none: %+v, %v", h, err)
	}
	for raw, want := range map[string]string{
		`{"source":{"kind":"none","env":"eks"},"target":{"env":"gke","dsn":"postgres://app:${DST_PGPASSWORD}@db-gke:5432/app"}}`:               "both be none",
		`{"source":{"env":"eks","dsn":"postgres://app:${SRC_PGPASSWORD}@db-eks:5432/app"},"target":{"kind":"none","env":"gke"}}`:               "both be none",
		`{"source":{"kind":"none","env":"eks","dsn":"postgres://app:${SRC_PGPASSWORD}@db-eks:5432/app"},"target":{"kind":"none","env":"gke"}}`: "no dsn",
		`{"source":{"kind":"none","env":"eks"},"target":{"kind":"none","env":"gke","dsn":"postgres://app:${DST_PGPASSWORD}@db-gke:5432/app"}}`: "no dsn",
	} {
		if _, err := p.checkRefs([]byte(raw)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", raw, err, want)
		}
	}
}

func TestNoDatabaseGatewayScope(t *testing.T) {
	p := statelessPolicy()
	none := dbHosts{noDatabase: true}
	if err := p.checkParsed(statelessPlan("http://gw:9901/apps/docker-doom/"), none); err != nil {
		t.Fatalf("the app's scope: %v", err)
	}
	withFiles := statelessPlan("http://gw:9901/apps/docker-doom")
	withFiles.Files = &plan.FilesSpec{Source: "eks:hopcharlie-eks-docker-doom", Target: "gke:hopcharlie-gke-docker-doom", Rclone: "rclone"}
	if app, err := p.checkStatelessScope(withFiles); err != nil || app != "docker-doom" {
		t.Fatalf("the app's own buckets: %q %v", app, err)
	}
	for name, c := range map[string]struct {
		pl   *plan.Plan
		want string
	}{
		"root":        {statelessPlan("http://gw:9901"), "scope of a registered app without a database"},
		"unknown app": {statelessPlan("http://gw:9901/apps/shop"), "scope of a registered app without a database"},
		"app with db": {statelessPlan("http://gw:9901/apps/moimroom"), "scope of a registered app without a database"},
		"not registered": {func() *plan.Plan {
			x := statelessPlan("http://gw:9901/apps/docker-doom")
			x.Target.Env = "onprem"
			return x
		}(), "not registered in both"},
		"no gateway": {func() *plan.Plan { x := statelessPlan(""); x.Gateway.Mode = "none"; return x }(), "needs the platform gateway"},
		"other backend": {func() *plan.Plan {
			x := statelessPlan("http://gw:9901/apps/docker-doom")
			x.Gateway.TargetBackend = "gke"
			return x
		}(), "must be \"eks\" and \"gke-backend\""},
		"other bucket": {func() *plan.Plan {
			x := statelessPlan("http://gw:9901/apps/docker-doom")
			x.Files = &plan.FilesSpec{Source: "eks:hopcharlie-eks-docker-doom", Target: "gke:hopcharlie-gke-moimroom", Rclone: "rclone"}
			return x
		}(), "its bucket in"},
		"written records": {func() *plan.Plan {
			x := statelessPlan("http://gw:9901/apps/docker-doom")
			x.Verify.WrittenRecords = "stream:x"
			return x
		}(), "written records"},
	} {
		if err := p.checkParsed(c.pl, none); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
	}
	// A database plan never takes a stateless app's scope.
	db := dbHosts{source: "x:5432", target: "y:5432", all: []string{"x:5432", "y:5432"}}
	dbPlan := statelessPlan("http://gw:9901/apps/docker-doom")
	dbPlan.Source.Cluster, dbPlan.Source.DSN, dbPlan.Target.Cluster, dbPlan.Target.DSN = "c", "d", "c", "d"
	if err := p.checkParsed(dbPlan, db); err == nil {
		t.Error("a database plan took the scope of an app without a database")
	}
}

func TestNoDatabaseSkipsTheDatabaseGates(t *testing.T) {
	s := newService(DefaultPolicy())
	ver, capa, par, res := &fakeVersion{}, &fakeCapacity{}, &fakeParity{}, &fakeGate{}
	s.SetVersionGate(ver)
	s.SetCapacityGate(capa)
	s.SetParityGate(par)
	s.SetResidencyGate(res)
	p := statelessPlan("http://gw:9901/apps/docker-doom")
	if !NoDatabase(p) {
		t.Fatal("NoDatabase = false")
	}
	if c, err := s.checkVersion(ctx, "", p); err != nil || c.Outcome != VersionNoDatabase {
		t.Errorf("version %+v %v", c, err)
	}
	if c, err := s.checkCapacity(ctx, "", p); err != nil || c.Outcome != CapacityNoDatabase {
		t.Errorf("capacity %+v %v", c, err)
	}
	if c := s.checkParity(ctx, "", p); c.Outcome != ParityNoDatabase {
		t.Errorf("parity %+v", c)
	}
	if c, err := s.checkResidency(ctx, "", p); err != nil || c.Outcome != ResidencyNoData {
		t.Errorf("residency without files %+v %v", c, err)
	}
	if ver.seen+capa.seen+par.seen+len(res.seen) != 0 {
		t.Fatal("a gate ran for a plan without a database or files")
	}
	// With files the residency gate still decides, on the files alone.
	p.Files = &plan.FilesSpec{Source: "eks:a", Target: "gke:a", Rclone: "rclone"}
	res.verdict.Allowed = true
	if c, err := s.checkResidency(ctx, "", p); err != nil || c.Outcome != ResidencyAllowed || len(res.seen) != 1 {
		t.Errorf("residency with files %+v %v", c, err)
	}
	// Plans with databases are unchanged.
	dbPlan := &plan.Plan{Source: plan.Endpoint{Cluster: "c", DSN: "d"}, Target: plan.Endpoint{Cluster: "c", DSN: "d"}}
	if NoDatabase(dbPlan) {
		t.Error("NoDatabase of a database plan")
	}
}

// TestNoDatabasePlanParses: the draft's plan passes the engine and the
// policy, and its summary names the app.
func TestNoDatabasePlanParses(t *testing.T) {
	raw := []byte(`{"id":"docker-doom-eks-to-gke","source":{"kind":"none","env":"eks"},"target":{"kind":"none","env":"gke"},
		"gateway":{"mode":"http","url":"http://gw:9901/apps/docker-doom","token":"${GATEWAY_TOKEN}","targetBackend":"gke-backend"}}`)
	s := newService(statelessPolicy())
	p, sum, err := s.parse(ctx, raw)
	if err != nil || !NoDatabase(p) || sum.App == nil || *sum.App != "docker-doom" || sum.Source.Kind == nil || *sum.Source.Kind != "none" {
		t.Fatalf("parse: %+v %v", sum, err)
	}
}

func TestSummaryNamesTheAppWithoutADatabase(t *testing.T) {
	p := statelessPlan("http://gw:9901/apps/docker-doom")
	ft := true
	p.Verify.FenceGate = &ft
	app, err := statelessPolicy().checkStatelessScope(p)
	if err != nil {
		t.Fatal(err)
	}
	sum := summarize(p, dbHosts{noDatabase: true, app: app})
	if sum.App == nil || *sum.App != "docker-doom" || sum.Source.Kind == nil || *sum.Source.Kind != "none" || sum.Target.Kind == nil {
		t.Fatalf("summary %+v", sum)
	}
	// A plan with databases shows neither, as before.
	if db := summarize(p, dbHosts{source: "x:5432"}); db.App != nil || db.Source.Kind != nil {
		t.Fatalf("database summary %+v", db)
	}
}
