package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/engine"
	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/apps"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/envs"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/plans"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/preflight/capacity"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/runs"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

// moimroomPlan is planJSON for the demo app; extra is spliced in as more top-level fields.
func moimroomPlan(id, srcCtx, dstCtx, extra string) string {
	if extra != "" {
		extra = "," + extra
	}
	return `{"id":"` + id + `",
	"source":{"env":"gke","context":"` + srcCtx + `","namespace":"moimroom","cluster":"moimroom-db-gke","dsn":"postgres://app:${SRC_PGPASSWORD}@moimroom-db-gke-rw.moimroom.gke.ts.net/app"},
	"target":{"env":"eks","context":"` + dstCtx + `","namespace":"moimroom","cluster":"moimroom-db-eks","dsn":"postgres://app:${DST_PGPASSWORD}@moimroom-db-eks-rw.moimroom.eks.ts.net/app"}` + extra + `}`
}

func withManifest(planJSON, manifest string) string {
	return strings.Replace(planJSON, `"cluster":"moimroom-db-eks"`, `"cluster":"moimroom-db-eks","manifest":"`+manifest+`"`, 1)
}

// toolsCapture records the plans and options the runner builds engines with.
type toolsCapture struct {
	stateDir string

	mu          sync.Mutex
	plans       []*plan.Plan
	opts        []engine.Options
	blockVerify chan struct{} // the next engine's verify step waits for it
}

// planTools gives the plan service the runner's state directory, as main
// does, and records every engine the runner builds.
func planTools(c *toolsCapture) option {
	return func(h *harness, rc *runs.Config, d *Deps) {
		c.stateDir = rc.StateDir
		d.Plans.SetStateDir(rc.StateDir)
		inner := rc.Factory
		rc.Factory = func(p *plan.Plan, o engine.Options) (runs.Engine, error) {
			e, err := inner(p, o)
			if err != nil {
				return nil, err
			}
			c.mu.Lock()
			defer c.mu.Unlock()
			c.plans, c.opts = append(c.plans, p), append(c.opts, o)
			if c.blockVerify != nil { // the run's goroutines do not exist yet
				e.(*fakeEngine).block[engine.StepVerify] = c.blockVerify
				c.blockVerify = nil
			}
			return e, nil
		}
	}
}

func (c *toolsCapture) engine(i int) (*plan.Plan, engine.Options) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.plans[i], c.opts[i]
}

func (c *toolsCapture) blockNextVerify() chan struct{} {
	ch := make(chan struct{})
	c.mu.Lock()
	c.blockVerify = ch
	c.mu.Unlock()
	return ch
}

// withApps configures the moimroom app and a plan policy that allows its
// password variables, as main does with PLATFORM_APPS_FILE.
func withApps(gateway string) option {
	return func(h *harness, rc *runs.Config, d *Deps) {
		var environments []envs.EnvConfig
		for _, e := range d.Envs.List(h.t.Context()) {
			environments = append(environments, envs.EnvConfig{Name: e.Name, Provider: string(e.Provider), KubeContext: e.KubeContext, GatewayBackend: e.GatewayBackend})
		}
		env := func(name, bucket string) apps.Environment {
			return apps.Environment{CnpgCluster: "moimroom-db-" + name, DBHost: "moimroom-db-" + name + "-rw.moimroom." + name + ".ts.net",
				PasswordEnv: strings.ToUpper(name) + "_MOIMROOM_PGPASSWORD", Bucket: bucket}
		}
		cat, err := apps.New(apps.Config{Apps: []apps.App{{
			Name: "moimroom", Namespace: "moimroom", Database: apps.Database{Name: "app", User: "app", Schemas: []string{"app"}},
			Environments: map[string]apps.Environment{
				"gke": env("gke", "gcs:moimroom-photos-gke"), "eks": env("eks", "s3:moimroom-photos-eks"), "onprem": env("onprem", "minio:moimroom-photos"),
			},
		}}}, environments, gateway)
		if err != nil {
			h.t.Fatal(err)
		}
		policy := plans.DefaultPolicy()
		policy.GatewayURL = gateway
		policy.HostPasswordEnvs = cat.PasswordHosts()
		policy.Buckets = cat.Buckets()
		d.Plans = plans.New(policy, h.store)
		d.Plans.SetStateDir(rc.StateDir)
		d.Apps = cat
	}
}

func (h *harness) idle() {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for h.runner.Active() > 0 {
		if time.Now().After(deadline) {
			h.t.Fatal("runs are still active")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestWrittenRecordsAPI(t *testing.T) {
	h := newHarness(t)
	path := "/v1/written-records/moimroom-demo"
	body := `{"records":[
		{"table":"app.reservations","pk":"42","at":"2026-11-07T10:00:05+09:00"},
		{"table":"app.reservation_slots","pk":"(3,2026-11-07,14)","at":"2026-11-07T01:00:09Z"},
		{"table":"reviews","pk":"7","at":"2026-11-07T01:00:01Z"}]}`
	code, b := h.do("POST", path, agentToken, body)
	if code != 202 || decode[apiv1.WrittenRecordsAccepted](t, b).Accepted != 3 {
		t.Fatalf("append: %d %s", code, b)
	}
	if code, b := h.do("POST", path, viewerToken, body); code != 403 {
		t.Fatalf("viewer append: %d %s", code, b)
	}
	code, b = h.do("GET", path, viewerToken, nil)
	st := decode[apiv1.WrittenRecordStats](t, b)
	if code != 200 || st.Stream != "moimroom-demo" || st.Count != 3 || st.LatestAt == nil || !st.LatestAt.Equal(time.Date(2026, 11, 7, 1, 0, 9, 0, time.UTC)) {
		t.Fatalf("stats: %d %s", code, b)
	}
	code, b = h.do("GET", "/v1/written-records/nobody-wrote-here", viewerToken, nil)
	if st := decode[apiv1.WrittenRecordStats](t, b); code != 200 || st.Count != 0 || st.LatestAt != nil {
		t.Fatalf("empty stream: %d %s", code, b)
	}

	// Without at, the record is stamped when it arrives.
	before := time.Now().UTC().Add(-time.Second)
	if code, b := h.do("POST", path, agentToken, `{"records":[{"table":"app.reviews","pk":"8"}]}`); code != 202 {
		t.Fatalf("append without at: %d %s", code, b)
	}
	_, b = h.do("GET", path, viewerToken, nil)
	if st := decode[apiv1.WrittenRecordStats](t, b); st.Count != 4 || st.LatestAt.Before(before) {
		t.Fatalf("stats after a record without at: %s", b)
	}

	many := make([]string, MaxWrittenRecords+1)
	for i := range many {
		many[i] = fmt.Sprintf(`{"table":"app.reservations","pk":"%d"}`, i)
	}
	for name, tc := range map[string]struct{ path, body string }{
		"stream name":   {"/v1/written-records/Moimroom_Demo", body},
		"no records":    {path, `{"records":[]}`},
		"too many":      {path, `{"records":[` + strings.Join(many, ",") + `]}`},
		"table":         {path, `{"records":[{"table":"app.reservations; drop table x","pk":"1"}]}`},
		"three parts":   {path, `{"records":[{"table":"db.app.reservations","pk":"1"}]}`},
		"empty pk":      {path, `{"records":[{"table":"app.reservations","pk":""}]}`},
		"long pk":       {path, `{"records":[{"table":"app.reservations","pk":"` + strings.Repeat("9", 201) + `"}]}`},
		"bad at":        {path, `{"records":[{"table":"app.reservations","pk":"1","at":"yesterday"}]}`},
		"unknown field": {path, `{"records":[{"table":"app.reservations","pk":"1","id":1}]}`},
	} {
		if code, b := h.do("POST", tc.path, agentToken, tc.body); code != 400 || errCode(t, b) != "invalid_request" {
			t.Errorf("%s: %d %s", name, code, b)
		}
	}
	if code, b := h.do("POST", path, agentToken, `{"records":[{"table":"app.reservations","pk":"`+strings.Repeat("9", 200)+`"}]}`); code != 202 {
		t.Fatalf("200-character pk: %d %s", code, b)
	}
}

func TestWrittenRecordsReachVerifyAlsoOnResume(t *testing.T) {
	c := &toolsCapture{}
	h := newHarness(t, planTools(c))
	stream := "/v1/written-records/moimroom-demo"
	if code, b := h.do("POST", stream, agentToken, `{"records":[{"table":"app.reservations","pk":"42"},{"table":"app.reservation_slots","pk":"(3,2026-11-07,14)"}]}`); code != 202 {
		t.Fatalf("append: %d %s", code, b)
	}
	ref := h.approved(moimroomPlan("p1", "a", "b", `"verify":{"schemas":["app"],"writtenRecords":"stream:moimroom-demo"}`))
	id := h.start(apiv1.StartMigrationRequest{PlanRef: ref})
	h.waitRun(id, status(apiv1.RunStatusAwaitingSwitch))

	file := filepath.Join(c.stateDir, id+".written.jsonl")
	if p, _ := c.engine(0); p.Verify.WrittenRecords != file {
		t.Fatalf("engine plan reads %q, want %q", p.Verify.WrittenRecords, file)
	}

	// The second attempt gets a fresh engine and plan, and the records that
	// arrived in between.
	if code, b := h.do("POST", "/v1/migrations/"+id+"/abort", opToken, nil); code != 200 {
		t.Fatalf("abort: %d %s", code, b)
	}
	h.waitRun(id, ended(apiv1.RunStatusAborted))
	if code, b := h.do("POST", stream, agentToken, `{"records":[{"table":"app.reviews","pk":"7"}]}`); code != 202 {
		t.Fatalf("append: %d %s", code, b)
	}
	block := c.blockNextVerify()
	if code, b := h.do("POST", "/v1/migrations/"+id+"/resume", opToken, nil); code != 200 {
		t.Fatalf("resume: %d %s", code, b)
	}
	h.waitRun(id, status(apiv1.RunStatusAwaitingSwitch))
	p, o := c.engine(1)
	if o.RunID != id || p.Verify.WrittenRecords != file {
		t.Fatalf("resumed engine: run %q reads %q", o.RunID, p.Verify.WrittenRecords)
	}
	if code, b := h.do("POST", "/v1/migrations/"+id+"/switch", opToken, nil); code != 200 {
		t.Fatalf("switch: %d %s", code, b)
	}
	h.waitRun(id, func(r apiv1.Run) bool { return r.Engine.Steps[engine.StepVerify].Status == apiv1.StepStatusRunning })

	got, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"table":"app.reservations","pk":"42"}` + "\n" +
		`{"table":"app.reservation_slots","pk":"(3,2026-11-07,14)"}` + "\n" +
		`{"table":"app.reviews","pk":"7"}` + "\n"
	if string(got) != want {
		t.Fatalf("written records file:\n%s\nwant:\n%s", got, want)
	}
	if runtime.GOOS != "windows" { // no Unix permission bits there
		if fi, _ := os.Stat(file); fi.Mode().Perm() != 0o600 {
			t.Errorf("file mode = %v", fi.Mode().Perm())
		}
	}
	close(block)
	h.waitRun(id, ended(apiv1.RunStatusSucceeded))
	evs, err := h.runner.Events(t.Context(), id, 0)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, e := range evs {
		found = found || (e.Kind == engine.EventLog && deref(e.Message) == "3 written records of stream moimroom-demo are ready for verification")
	}
	if !found {
		t.Fatal("no event says the written records are ready")
	}
}

func TestPlanAttachmentsAPI(t *testing.T) {
	c := &toolsCapture{}
	h := newHarness(t, planTools(c))
	// An operator's plan: this test is about attachments and revisions, not
	// the rehearsal evidence an agent's plan needs (TestApproveAIPlanNeedsRehearsal).
	code, b := h.do("POST", "/v1/plans", opToken, withManifest(moimroomPlan("p1", "a", "b", ""), "attachment:target.yaml"))
	if code != 201 {
		t.Fatalf("submit: %d %s", code, b)
	}
	rec := decode[apiv1.PlanRecord](t, b)
	if rec.Attachments == nil || len(*rec.Attachments) != 0 {
		t.Fatalf("attachments of a new plan: %v", rec.Attachments)
	}
	base := "/v1/plans/" + rec.Ref + "/attachments/"
	octet := []string{"Content-Type", "application/octet-stream"}
	manifest := "apiVersion: postgresql.cnpg.io/v1\nkind: Cluster\nmetadata:\n  name: moimroom-db-eks\n"

	if code, b := h.do("POST", "/v1/plans/"+rec.Ref+"/approve", opToken, nil); code != 409 || errCode(t, b) != "plan_state_conflict" || !strings.Contains(string(b), "target.yaml") {
		t.Fatalf("approve without the attachment: %d %s", code, b)
	}
	if code, b := h.do("PUT", base+"target.yaml", viewerToken, manifest, octet...); code != 403 {
		t.Fatalf("viewer upload: %d %s", code, b)
	}
	if code, b := h.do("PUT", base+"bad!name", agentToken, manifest, octet...); code != 400 || errCode(t, b) != "invalid_request" {
		t.Fatalf("bad name: %d %s", code, b)
	}
	if code, b := h.do("PUT", base+"target.yaml", agentToken, "", octet...); code != 400 {
		t.Fatalf("empty upload: %d %s", code, b)
	}
	if code, b := h.do("PUT", base+"target.yaml", agentToken, strings.Repeat("x", MaxBodyBytes+1), octet...); code != 413 || errCode(t, b) != "too_large" {
		t.Fatalf("large upload: %d %s", code, b)
	}
	if code, b := h.do("PUT", "/v1/plans/pln_missing/attachments/target.yaml", agentToken, manifest, octet...); code != 404 || errCode(t, b) != "plan_not_found" {
		t.Fatalf("upload to a missing plan: %d %s", code, b)
	}

	code, b = h.do("PUT", base+"target.yaml", agentToken, manifest, octet...)
	info := decode[apiv1.PlanAttachment](t, b)
	if code != 200 || info.Name != "target.yaml" || info.Size != int64(len(manifest)) || len(info.Sha256) != 64 {
		t.Fatalf("upload: %d %s", code, b)
	}
	if code, b := h.do("GET", base+"target.yaml", viewerToken, nil); code != 200 || string(b) != manifest {
		t.Fatalf("download: %d %q", code, b)
	}
	if code, b := h.do("GET", base+"other.yaml", viewerToken, nil); code != 404 || errCode(t, b) != "attachment_not_found" {
		t.Fatalf("missing attachment: %d %s", code, b)
	}
	_, b = h.do("GET", "/v1/plans/"+rec.Ref, viewerToken, nil)
	reviewed := decode[apiv1.PlanRecord](t, b)
	if reviewed.Attachments == nil || len(*reviewed.Attachments) != 1 || (*reviewed.Attachments)[0] != info || reviewed.Revision != 2 {
		t.Fatalf("plan attachments: %s", b)
	}

	// The attachment is replaced after the operator reviewed revision 2.
	if code, b := h.do("PUT", base+"target.yaml", agentToken, manifest+"  instances: 9", octet...); code != 200 {
		t.Fatalf("second upload: %d %s", code, b)
	}
	approve := "/v1/plans/" + rec.Ref + "/approve"
	if code, b := h.do("POST", approve, opToken, nil); code != 409 || errCode(t, b) != "plan_state_conflict" || !strings.Contains(string(b), "needs the revision you reviewed") {
		t.Fatalf("approve without a revision: %d %s", code, b)
	}
	if code, b := h.do("POST", approve, opToken, map[string]any{"revision": reviewed.Revision}); code != 409 || errCode(t, b) != "plan_state_conflict" ||
		!strings.Contains(string(b), "changed since you reviewed it (revision 2, now 3)") {
		t.Fatalf("approve a stale revision: %d %s", code, b)
	}
	if code, b := h.do("POST", approve, opToken, map[string]any{"revision": 0}); code != 400 || errCode(t, b) != "invalid_request" {
		t.Fatalf("approve revision 0: %d %s", code, b)
	}
	if code, b := h.do("PUT", base+"target.yaml", agentToken, manifest, octet...); code != 200 {
		t.Fatalf("third upload: %d %s", code, b)
	}
	code, b = h.do("POST", approve, opToken, map[string]any{"revision": 4, "comment": "manifest checked"})
	if p := decode[apiv1.PlanRecord](t, b); code != 200 || p.Status != apiv1.PlanStatusApproved || p.Revision != 4 {
		t.Fatalf("approve: %d %s", code, b)
	}
	if code, b := h.do("PUT", base+"target.yaml", agentToken, "kind: Changed\n", octet...); code != 409 || errCode(t, b) != "plan_state_conflict" {
		t.Fatalf("upload after approval: %d %s", code, b)
	}
	_, b = h.do("GET", "/v1/plans", viewerToken, nil)
	if l := decode[apiv1.PlanList](t, b); len(l.Plans) != 1 || l.Plans[0].Attachments == nil || len(*l.Plans[0].Attachments) != 1 {
		t.Fatalf("plan list attachments: %s", b)
	}

	h.start(apiv1.StartMigrationRequest{PlanRef: rec.Ref})
	p, _ := c.engine(0)
	want := filepath.Join(c.stateDir, "plans", rec.Ref, "target.yaml")
	if p.Target.Manifest != want {
		t.Fatalf("engine manifest = %q, want %q", p.Target.Manifest, want)
	}
	if got, err := os.ReadFile(want); err != nil || string(got) != manifest {
		t.Fatalf("materialized manifest = %q, %v", got, err)
	}

	// The policy still refuses paths outside the server's working directory.
	for _, m := range []string{"/etc/passwd", "../target.yaml", "attachment:../x"} {
		if code, b := h.do("POST", "/v1/plans", agentToken, withManifest(moimroomPlan("p2", "c", "d", ""), m)); code != 400 || errCode(t, b) != "invalid_plan" {
			t.Errorf("manifest %q: %d %s", m, code, b)
		}
	}
}

func TestAppsAndPlanDrafts(t *testing.T) {
	h := newHarness(t, withApps(gatewayURL))
	code, b := h.do("GET", "/v1/apps", viewerToken, nil)
	list := decode[apiv1.AppList](t, b)
	if code != 200 || len(list.Apps) != 1 || list.Apps[0].Name != "moimroom" || len(list.Apps[0].Environments) != 3 ||
		list.Apps[0].Environments[1].Env != "eks" || list.Apps[0].Environments[1].DbHost != "moimroom-db-eks-rw.moimroom.eks.ts.net:5432" {
		t.Fatalf("apps: %d %s", code, b)
	}

	code, b = h.do("POST", "/v1/plans/draft", agentToken, map[string]any{"app": "moimroom", "source": "gke", "target": "eks", "writtenRecordsStream": "moimroom-demo"})
	if code != 200 {
		t.Fatalf("draft: %d %s", code, b)
	}
	d := decode[apiv1.EnginePlan](t, b)
	if d.Id != "moimroom-gke-to-eks" || deref(d.Source.Context) != "gke-ctx" || d.Target.Cluster != "moimroom-db-eks" ||
		d.Source.Dsn != "postgres://app:${GKE_MOIMROOM_PGPASSWORD}@moimroom-db-gke-rw.moimroom.gke.ts.net:5432/app?sslmode=require" ||
		d.Files == nil || d.Files.Target != "s3:moimroom-photos-eks" ||
		deref(d.Gateway.Url) != gatewayURL || deref(d.Gateway.TargetBackend) != "eks" || deref(d.Verify.WrittenRecords) != "stream:moimroom-demo" {
		t.Fatalf("draft: %s", b)
	}
	// The draft is accepted as is.
	rec := h.submit(string(b))
	if rec.Summary.PlanId != "moimroom-gke-to-eks" || !rec.Summary.Files || rec.Summary.GatewayMode != "http" {
		t.Fatalf("submitted draft: %+v", rec.Summary)
	}

	code, b = h.do("POST", "/v1/plans/draft", agentToken, map[string]any{"app": "moimroom", "source": "eks", "target": "onprem", "files": false})
	if d := decode[apiv1.EnginePlan](t, b); code != 200 || d.Files != nil || d.Id != "moimroom-eks-to-onprem" {
		t.Fatalf("draft without files: %d %s", code, b)
	}
	for name, body := range map[string]map[string]any{
		"unknown app": {"app": "shop", "source": "gke", "target": "eks"},
		"unknown env": {"app": "moimroom", "source": "gke", "target": "aks"},
		"same env":    {"app": "moimroom", "source": "gke", "target": "gke"},
		"bad stream":  {"app": "moimroom", "source": "gke", "target": "eks", "writtenRecordsStream": "Demo!"},
		"bad field":   {"app": "moimroom", "source": "gke", "target": "eks", "canary": true},
	} {
		if code, b := h.do("POST", "/v1/plans/draft", agentToken, body); code != 400 || errCode(t, b) != "invalid_request" {
			t.Errorf("%s: %d %s", name, code, b)
		}
	}
	if code, b := h.do("POST", "/v1/plans/draft", viewerToken, map[string]any{"app": "moimroom", "source": "gke", "target": "eks"}); code != 403 {
		t.Fatalf("viewer draft: %d %s", code, b)
	}

	// Without a gateway the draft uses mode none.
	h2 := newHarness(t, withoutGateway(), withApps(""))
	_, b = h2.do("POST", "/v1/plans/draft", agentToken, map[string]any{"app": "moimroom", "source": "gke", "target": "eks"})
	if d := decode[apiv1.EnginePlan](t, b); *d.Gateway.Mode != apiv1.EnginePlanGatewayModeNone || d.Gateway.Url != nil {
		t.Fatalf("draft without gateway: %s", b)
	}

	// Without PLATFORM_APPS_FILE there are no apps to draft for.
	h3 := newHarness(t)
	if code, b := h3.do("GET", "/v1/apps", viewerToken, nil); code != 200 || len(decode[apiv1.AppList](t, b).Apps) != 0 {
		t.Fatalf("apps without a file: %d %s", code, b)
	}
	if code, b := h3.do("POST", "/v1/plans/draft", agentToken, map[string]any{"app": "moimroom", "source": "gke", "target": "eks"}); code != 400 {
		t.Fatalf("draft without a file: %d %s", code, b)
	}
}

// fakeSource answers MeasureSource and records the DSN it was given.
type fakeSource struct {
	mu   sync.Mutex
	dsns []string
	ok   bool
}

func (f *fakeSource) MeasureSource(_ context.Context, dsn string) (capacity.SourceStats, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dsns = append(f.dsns, dsn)
	if !f.ok {
		return capacity.SourceStats{}, false
	}
	size, wal := int64(2147483648), 1048576.0
	return capacity.SourceStats{Sessions: 80, MaxConnections: 200, MaxWorkerProcesses: 8, Bytes: &size, WALBytesPerSecond: &wal,
		Window: 5 * time.Second, MeasuredAt: time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)}, true
}

func TestPlanDraftSourceStats(t *testing.T) {
	src := &fakeSource{ok: true}
	h := newHarness(t, withApps(gatewayURL), func(_ *harness, _ *runs.Config, d *Deps) { d.SourceStats = src })
	body := map[string]any{"app": "moimroom", "source": "gke", "target": "eks", "sourceStats": true}

	// The password variable is unset: the source is not tried.
	t.Setenv("GKE_MOIMROOM_PGPASSWORD", "")
	code, b := h.do("POST", "/v1/plans/draft", agentToken, body)
	res := decode[apiv1.PlanDraftResult](t, b)
	if code != 200 || res.Plan.Id != "moimroom-gke-to-eks" || res.SourceStats != nil ||
		res.SourceStatsReason == nil || *res.SourceStatsReason != apiv1.PlanDraftResultSourceStatsReasonNotMeasured || len(src.dsns) != 0 {
		t.Fatalf("unset password: %d %s (tried %d)", code, b, len(src.dsns))
	}

	t.Setenv("GKE_MOIMROOM_PGPASSWORD", "s3cret-pw")
	code, b = h.do("POST", "/v1/plans/draft", agentToken, body)
	res = decode[apiv1.PlanDraftResult](t, b)
	if code != 200 || res.SourceStats == nil || res.SourceStatsReason != nil ||
		res.SourceStats.Sessions != 80 || res.SourceStats.MaxConnections != 200 || res.SourceStats.MaxWorkerProcesses != 8 ||
		res.SourceStats.Bytes == nil || *res.SourceStats.Bytes != 2147483648 || res.SourceStats.WindowSeconds != 5 {
		t.Fatalf("measured: %d %s", code, b)
	}
	// Measured with the expanded DSN; the response keeps ${VAR} and never the password.
	if src.dsns[0] != "postgres://app:s3cret-pw@moimroom-db-gke-rw.moimroom.gke.ts.net:5432/app?sslmode=require" {
		t.Fatalf("measured DSN %q", src.dsns[0])
	}
	if strings.Contains(string(b), "s3cret-pw") || !strings.Contains(string(b), "${GKE_MOIMROOM_PGPASSWORD}") {
		t.Fatalf("response leaks or loses the password reference: %s", b)
	}
	// The plan inside is submitted as is.
	draft, _ := json.Marshal(res.Plan)
	if rec := h.submit(string(draft)); rec.Summary.PlanId != "moimroom-gke-to-eks" {
		t.Fatalf("submitted: %+v", rec.Summary)
	}

	// A source that cannot be measured.
	src.ok = false
	_, b = h.do("POST", "/v1/plans/draft", agentToken, body)
	if res := decode[apiv1.PlanDraftResult](t, b); res.SourceStats != nil || res.SourceStatsReason == nil {
		t.Fatalf("unmeasured: %s", b)
	}

	// Without the flag the response is the plan, and nothing is measured.
	n := len(src.dsns)
	_, b = h.do("POST", "/v1/plans/draft", agentToken, map[string]any{"app": "moimroom", "source": "gke", "target": "eks"})
	if d := decode[apiv1.EnginePlan](t, b); d.Id != "moimroom-gke-to-eks" || len(src.dsns) != n {
		t.Fatalf("plain draft: %s", b)
	}

	// No measurer configured.
	h2 := newHarness(t, withApps(gatewayURL))
	_, b = h2.do("POST", "/v1/plans/draft", agentToken, body)
	if res := decode[apiv1.PlanDraftResult](t, b); res.SourceStats != nil || res.SourceStatsReason == nil {
		t.Fatalf("no measurer: %s", b)
	}
}

func TestRollback(t *testing.T) {
	c := &toolsCapture{}
	h := newHarness(t, planTools(c))
	extra := `"files":{"source":"gcs:moimroom-photos-gke","target":"s3:moimroom-photos-eks"},` +
		`"gateway":{"mode":"http","url":"` + gatewayURL + `","token":"${GATEWAY_TOKEN}","sourceBackend":"gke","targetBackend":"eks"}`
	rec := h.submit(withManifest(moimroomPlan("p1", "gke-ctx", "eks-ctx", extra), "attachment:target.yaml"))
	if code, b := h.do("PUT", "/v1/plans/"+rec.Ref+"/attachments/target.yaml", agentToken, "kind: Cluster\n"); code != 200 {
		t.Fatalf("upload: %d %s", code, b)
	}
	sum := sha256.Sum256([]byte("kind: Cluster\n"))
	rev := h.rehearsed(rec.Ref, withManifest(moimroomPlan("p1", "gke-ctx", "eks-ctx", extra), "attachment:target.yaml"), hex.EncodeToString(sum[:]))
	if code, b := h.do("POST", "/v1/plans/"+rec.Ref+"/approve", opToken, map[string]any{"revision": rev}); code != 200 {
		t.Fatalf("approve: %d %s", code, b)
	}
	id := h.start(apiv1.StartMigrationRequest{PlanRef: rec.Ref, AutoSwitch: ptr(true)})
	h.waitRun(id, ended(apiv1.RunStatusSucceeded))
	h.idle()

	if code, b := h.do("POST", "/v1/migrations/"+id+"/rollback", agentToken, nil); code != 403 {
		t.Fatalf("agent rollback: %d %s", code, b)
	}
	if code, b := h.do("POST", "/v1/migrations/nope/rollback", opToken, nil); code != 404 || errCode(t, b) != "run_not_found" {
		t.Fatalf("unknown run: %d %s", code, b)
	}
	code, b := h.do("POST", "/v1/migrations/"+id+"/rollback", opToken, map[string]any{"reason": "eks bill too high"})
	if code != 202 {
		t.Fatalf("rollback: %d %s", code, b)
	}
	resp := decode[apiv1.StartMigrationResponse](t, b)
	run := h.waitRun(resp.RunId, ended(apiv1.RunStatusSucceeded))
	if deref(run.RollbackOf) != id || run.Source.Env != "eks" || run.Target.Env != "gke" || run.PlanId != "p1-rollback" ||
		!run.AutoSwitch || run.StartedBy != "op" || run.PlanRef != resp.PlanRef {
		t.Fatalf("rollback run: %+v", run)
	}
	p, _ := c.engine(1)
	if p.Source.Context != "eks-ctx" || p.Target.Cluster != "moimroom-db-gke" || p.Target.Manifest != "" ||
		p.Files.Source != "s3:moimroom-photos-eks" || p.Gateway.SourceBackend != "eks" || p.Gateway.TargetBackend != "gke" {
		t.Fatalf("reverse plan: %+v", p)
	}
	_, b = h.do("GET", "/v1/plans/"+resp.PlanRef, viewerToken, nil)
	pr := decode[apiv1.PlanRecord](t, b)
	if pr.Status != apiv1.PlanStatusStarted || deref(pr.ReviewedBy) != "op" || deref(pr.ReviewComment) != "rollback of run "+id+": eks bill too high" || deref(pr.RunId) != resp.RunId ||
		deref(pr.RollbackOf) != id || deref(run.Source.DbHost) != "moimroom-db-eks-rw.moimroom.eks.ts.net:5432" {
		t.Fatalf("rollback plan: %s", b)
	}
	row, err := h.store.GetPlan(t.Context(), resp.PlanRef)
	if err != nil || strings.Contains(string(row.Raw), "gw-token") || !strings.Contains(string(row.Raw), "${GATEWAY_TOKEN}") {
		t.Fatalf("stored reverse plan must keep ${VAR} references: %s %v", row.Raw, err)
	}

	_, b = h.do("GET", "/v1/audit", viewerToken, nil)
	var detail map[string]any
	for _, e := range decode[apiv1.AuditList](t, b).Entries {
		if e.Action == "POST /v1/migrations/{runId}/rollback" && e.Status == 202 {
			detail = *e.Detail
		}
	}
	if detail["fromRun"] != id || detail["planRef"] != resp.PlanRef || detail["runId"] != resp.RunId {
		t.Fatalf("audit detail: %v", detail)
	}

	h.idle()
	if code, b := h.do("POST", "/v1/migrations/"+id+"/rollback", opToken, nil); code != 409 || errCode(t, b) != "run_state_conflict" {
		t.Fatalf("second rollback: %d %s", code, b)
	}
	// A rollback is a run like any other: it can be rolled back too, here
	// waiting for /switch.
	code, b = h.do("POST", "/v1/migrations/"+resp.RunId+"/rollback", opToken, map[string]any{"autoSwitch": false})
	if code != 202 {
		t.Fatalf("rollback of the rollback: %d %s", code, b)
	}
	again := h.waitRun(decode[apiv1.StartMigrationResponse](t, b).RunId, status(apiv1.RunStatusAwaitingSwitch))
	if again.PlanId != "p1-rollback-rollback" || again.AutoSwitch || again.Source.Env != "gke" {
		t.Fatalf("second-level rollback: %+v", again)
	}
	if code, b := h.do("POST", "/v1/migrations/"+again.RunId+"/rollback", opToken, nil); code != 409 || errCode(t, b) != "run_state_conflict" {
		t.Fatalf("rollback of a waiting run: %d %s", code, b)
	}
}
