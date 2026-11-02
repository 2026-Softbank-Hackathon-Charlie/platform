package plans

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store/storetest"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

func withManifest(m string) []byte {
	return []byte(strings.Replace(string(planJSON("")), `"cluster":"db-eks"`, `"cluster":"db-eks","manifest":"`+m+`"`, 1))
}

func TestPolicyReferences(t *testing.T) {
	s := newService(DefaultPolicy())
	for name, raw := range map[string][]byte{
		"attachment":    withManifest("attachment:target.yaml"),
		"relative path": withManifest("manifests/target.yaml"),
		"stream":        planJSON(`"verify": {"writtenRecords": "stream:moimroom-demo"}`),
		"relative file": planJSON(`"verify": {"writtenRecords": "k6/written.jsonl"}`),
	} {
		if _, err := s.Parse(raw); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, tc := range map[string]struct {
		raw  []byte
		want string
	}{
		"attachment ..":       {withManifest("attachment:.."), "attachment name"},
		"attachment slash":    {withManifest("attachment:a/b"), "attachment name"},
		"attachment empty":    {withManifest("attachment:"), "attachment name"},
		"absolute path":       {withManifest("/etc/passwd"), "relative path"},
		"stream upper case":   {planJSON(`"verify": {"writtenRecords": "stream:Demo"}`), "stream name"},
		"stream traversal":    {planJSON(`"verify": {"writtenRecords": "stream:../x"}`), "stream name"},
		"written escapes dir": {planJSON(`"verify": {"writtenRecords": "../x.jsonl"}`), "must not leave"},
	} {
		_, err := s.Parse(tc.raw)
		if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
	for _, name := range []string{".", "..", "", "a b", strings.Repeat("x", 101)} {
		if ValidAttachmentName(name) {
			t.Errorf("ValidAttachmentName(%q) = true", name)
		}
	}
	for _, name := range []string{"target.yaml", "...", "a_b-c.1", strings.Repeat("x", 100)} {
		if !ValidAttachmentName(name) {
			t.Errorf("ValidAttachmentName(%q) = false", name)
		}
	}
}

func TestAttachmentLifecycle(t *testing.T) {
	st := storetest.NewMemory()
	s := New(DefaultPolicy(), st)
	dir := t.TempDir()
	s.SetStateDir(dir)

	rec, err := s.Submit(ctx, withManifest("attachment:target.yaml"), "planner")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Attachments == nil || len(*rec.Attachments) != 0 {
		t.Fatalf("attachments of a new plan = %v", rec.Attachments)
	}
	if _, err := s.Approve(ctx, rec.Ref, "op", "", nil); !errors.Is(err, ErrWrongStatus) || !strings.Contains(err.Error(), `attachment "target.yaml"`) {
		t.Fatalf("approve without the attachment: %v", err)
	}
	if _, err := s.GetAttachment(ctx, rec.Ref, "target.yaml"); !errors.Is(err, ErrAttachmentNotFound) {
		t.Fatalf("get missing attachment: %v", err)
	}
	if _, err := s.GetAttachment(ctx, "pln_missing", "target.yaml"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get attachment of a missing plan: %v", err)
	}
	if _, err := s.PutAttachment(ctx, "pln_missing", "target.yaml", []byte("x"), "planner"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("put to a missing plan: %v", err)
	}

	content := []byte("apiVersion: postgresql.cnpg.io/v1\nkind: Cluster\n")
	info, err := s.PutAttachment(ctx, rec.Ref, "target.yaml", content, "planner")
	if err != nil {
		t.Fatal(err)
	}
	if info.Size != int64(len(content)) || len(info.Sha256) != 64 {
		t.Fatalf("info = %+v", info)
	}
	got, err := s.Get(ctx, rec.Ref)
	if err != nil || got.Attachments == nil || len(*got.Attachments) != 1 || (*got.Attachments)[0] != info {
		t.Fatalf("record attachments = %v, %v", got.Attachments, err)
	}
	if got.Revision != 2 {
		t.Fatalf("revision after one upload = %d", got.Revision)
	}
	if _, err := s.Approve(ctx, rec.Ref, "op", "ok", ptr[int64](2)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutAttachment(ctx, rec.Ref, "target.yaml", []byte("changed"), "planner"); !errors.Is(err, ErrWrongStatus) {
		t.Fatalf("put after approval: %v", err)
	}

	res, err := s.Reserve(ctx, rec.Ref)
	if err != nil {
		t.Fatal(err)
	}
	p := res.Plan
	want := filepath.Join(dir, "plans", rec.Ref, "target.yaml")
	if p.Target.Manifest != want {
		t.Fatalf("manifest = %q, want %q", p.Target.Manifest, want)
	}
	b, err := os.ReadFile(want)
	if err != nil || string(b) != string(content) {
		t.Fatalf("materialized = %q, %v", b, err)
	}
	if runtime.GOOS != "windows" { // no Unix permission bits there
		if fi, _ := os.Stat(want); fi.Mode().Perm() != 0o600 {
			t.Errorf("file mode = %v", fi.Mode().Perm())
		}
		if fi, _ := os.Stat(filepath.Dir(want)); fi.Mode().Perm() != 0o700 {
			t.Errorf("dir mode = %v", fi.Mode().Perm())
		}
	}
	if err := s.Started(ctx, rec.Ref, "run_1"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(want); err != nil {
		t.Fatal(err)
	}
	if p, err = s.Load(ctx, rec.Ref); err != nil || p.Target.Manifest != want {
		t.Fatalf("load: %q, %v", p.Target.Manifest, err)
	}
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("load did not write the attachment again: %v", err)
	}
	list, err := s.List(ctx)
	if err != nil || len(list) != 1 || list[0].Attachments == nil || len(*list[0].Attachments) != 1 {
		t.Fatalf("list = %+v, %v", list, err)
	}
}

func TestRollbackReversesThePlanAsSubmitted(t *testing.T) {
	t.Setenv("SRC_PGPASSWORD", "src-secret")
	t.Setenv("DST_PGPASSWORD", "dst-secret")
	t.Setenv("GATEWAY_TOKEN", "gw-secret")
	policy := DefaultPolicy()
	policy.GatewayURL = "http://gateway.ts.net:9901"
	st := storetest.NewMemory()
	s := New(policy, st)

	raw := strings.Replace(string(withManifest("attachment:target.yaml")), `"id": "shop-gke-to-eks"`, `"id": "moimroom-gke-to-eks"`, 1)
	raw = strings.Replace(raw, "\n}", `,
  "files": {"source": "gcs:moimroom-photos-gke", "target": "s3:moimroom-photos-eks", "flags": ["--checksum"]},
  "gateway": {"mode": "http", "url": "http://gateway.ts.net:9901", "token": "${GATEWAY_TOKEN}", "sourceBackend": "gke-be"},
  "verify": {"schemas": ["app"], "writtenRecords": "stream:moimroom-demo"},
  "thresholds": {"maxDowntime": "90s"}
}`, 1)
	orig, err := s.Submit(ctx, []byte(raw), "planner")
	if err != nil {
		t.Fatal(err)
	}

	rec, err := s.Rollback(ctx, "r1", orig.Ref, "op", "rollback of run r1: demo")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Status != apiv1.PlanStatusApproved || deref(rec.ReviewedBy) != "op" || rec.SubmittedBy != "op" || deref(rec.ReviewComment) != "rollback of run r1: demo" ||
		deref(rec.RollbackOf) != "r1" || rec.Revision != 1 {
		t.Fatalf("record = %+v", rec)
	}
	sum := rec.Summary
	if sum.PlanId != "moimroom-gke-to-eks-rollback" || sum.Source.Env != "eks" || sum.Target.Env != "gke" ||
		sum.Source.Cluster != "db-eks" || deref(sum.Target.Context) != "gke-ctx" || !sum.Files || sum.MaxDowntime != "1m30s" {
		t.Fatalf("summary = %+v", sum)
	}

	row, err := st.GetPlan(ctx, rec.Ref)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"src-secret", "dst-secret", "gw-secret"} {
		if strings.Contains(string(row.Raw), secret) {
			t.Fatalf("stored raw contains %s:\n%s", secret, row.Raw)
		}
	}
	var p apiv1.EnginePlan
	if err := json.Unmarshal(row.Raw, &p); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.Source.Dsn, "${DST_PGPASSWORD}@db-eks") || !strings.Contains(p.Target.Dsn, "${SRC_PGPASSWORD}@db-gke") {
		t.Fatalf("dsns = %q, %q", p.Source.Dsn, p.Target.Dsn)
	}
	if p.Source.Manifest != nil || p.Target.Manifest != nil {
		t.Fatalf("manifests kept: %v %v", p.Source.Manifest, p.Target.Manifest)
	}
	if p.Files.Source != "s3:moimroom-photos-eks" || p.Files.Target != "gcs:moimroom-photos-gke" || (*p.Files.Flags)[0] != "--checksum" {
		t.Fatalf("files = %+v", p.Files)
	}
	if p.Gateway.SourceBackend != nil || deref(p.Gateway.TargetBackend) != "gke-be" || deref(p.Gateway.Token) != "${GATEWAY_TOKEN}" {
		t.Fatalf("gateway = %+v", p.Gateway)
	}
	if deref(p.Verify.WrittenRecords) != "stream:moimroom-demo" {
		t.Fatalf("verify = %+v", p.Verify)
	}

	// The reverse plan starts like any approved plan, without an attachment.
	res, err := s.Reserve(ctx, rec.Ref)
	if err != nil {
		t.Fatal(err)
	}
	if pl := res.Plan; pl.Gateway.SourceBackend != "eks" || pl.Gateway.TargetBackend != "gke-be" || pl.Target.Manifest != "" || res.RollbackOf != "r1" {
		t.Fatalf("parsed reverse plan: %+v %+v %q", pl.Gateway, pl.Target, res.RollbackOf)
	}
	if res.Summary.Source.Env != "eks" || deref(res.Summary.Source.DbHost) != "db-eks.shop.ts.net:5432" {
		t.Fatalf("reserved summary: %+v", res.Summary.Source)
	}
	if _, err := s.Rollback(ctx, "r9", "pln_missing", "op", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rollback of a missing plan: %v", err)
	}
}

func TestRollbackID(t *testing.T) {
	long := strings.Repeat("a", 50) + "-to-" + strings.Repeat("b", 9) // 63
	for in, want := range map[string]string{
		"moimroom-gke-to-eks": "moimroom-gke-to-eks-rollback",
		long:                  strings.Repeat("a", 50) + "-to-rollback",
	} {
		got := rollbackID(in)
		if got != want || len(got) > 63 || !planID.MatchString(got) {
			t.Errorf("rollbackID(%q) = %q, want %q", in, got, want)
		}
	}
}

// The store's conflict error reaches callers as ErrWrongStatus with the status.
func TestAttachmentConflictNamesStatus(t *testing.T) {
	st := storetest.NewMemory()
	if err := st.CreatePlan(ctx, store.Plan{Ref: "pln_1", Status: store.PlanStarted}); err != nil {
		t.Fatal(err)
	}
	_, err := New(DefaultPolicy(), st).PutAttachment(ctx, "pln_1", "x", []byte("x"), "a")
	if !errors.Is(err, ErrWrongStatus) || !strings.Contains(err.Error(), "plan is started") {
		t.Fatalf("err = %v", err)
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
