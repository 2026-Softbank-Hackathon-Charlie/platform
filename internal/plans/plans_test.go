package plans

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store/storetest"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

var ctx = context.Background()

func newService(p Policy) *Service { return New(p, storetest.NewMemory()) }

// Base plan; tests splice extra JSON in before the closing brace.
const base = `{
  "id": "shop-gke-to-eks",
  "source": {"env":"gke","context":"gke-ctx","namespace":"shop","cluster":"db-gke","dsn":"postgres://app:${SRC_PGPASSWORD}@db-gke.shop.ts.net/app"},
  "target": {"env":"eks","context":"eks-ctx","namespace":"shop","cluster":"db-eks","dsn":"postgres://app:${DST_PGPASSWORD}@db-eks.shop.ts.net/app"}%s
}`

func planJSON(extra string) []byte {
	if extra != "" {
		extra = ",\n" + extra
	}
	return []byte(strings.Replace(base, "%s", extra, 1))
}

func TestSubmitAcceptsValidPlan(t *testing.T) {
	t.Setenv("SRC_PGPASSWORD", "hunter2")
	policy := DefaultPolicy()
	policy.Buckets = []string{"gcs:b", "s3:b"}
	s := newService(policy)
	rec, err := s.Submit(ctx, planJSON(`"files": {"source":"gcs:b","target":"s3:b","flags":["--transfers","16","--checksum"]}`), "alice")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Status != apiv1.PlanStatusPendingApproval || !strings.HasPrefix(rec.Ref, "pln_") || rec.SubmittedBy != "alice" {
		t.Fatalf("record = %+v", rec)
	}
	if rec.Summary.Source.Cluster != "db-gke" || !rec.Summary.Files || rec.Summary.GatewayMode != "none" || rec.Summary.MaxDowntime != "3m0s" {
		t.Fatalf("summary = %+v", rec.Summary)
	}
}

func TestPolicyRejects(t *testing.T) {
	policy := DefaultPolicy()
	policy.GatewayURL = "http://gateway.ts.net:9901"
	policy.DSNHostSuffixes = []string{".ts.net"}
	cases := map[string]struct {
		raw  []byte
		want string
	}{
		"foreign env var in dsn": {
			[]byte(strings.Replace(string(planJSON("")), "${SRC_PGPASSWORD}", "${AWS_SECRET_ACCESS_KEY}", 1)),
			`"AWS_SECRET_ACCESS_KEY"`,
		},
		"bare $VAR form": {
			[]byte(strings.Replace(string(planJSON("")), "${SRC_PGPASSWORD}", "$HOME", 1)),
			"only as its whole password",
		},
		"dsn to outside host": {
			[]byte(strings.Replace(string(planJSON("")), "db-gke.shop.ts.net", "attacker.example.com", 1)),
			"not an allowed database host",
		},
		"custom kubeconfig": {
			[]byte(strings.Replace(string(planJSON("")), `"context":"gke-ctx"`, `"context":"gke-ctx","kubeconfig":"/root/.kube/config"`, 1)),
			"kubeconfig is not allowed",
		},
		"rclone binary": {
			planJSON(`"files": {"source":"a:b","target":"c:d","rclone":"/bin/sh"}`),
			"files.rclone is not allowed",
		},
		"rclone flag": {
			planJSON(`"files": {"source":"a:b","target":"c:d","flags":["--config=/etc/shadow"]}`),
			`"--config"`,
		},
		"files without registered buckets": {
			planJSON(`"files": {"source":"a:b","target":"c:d"}`),
			"files is disabled on this server",
		},
		"absolute manifest": {
			[]byte(strings.Replace(string(planJSON("")), `"cluster":"db-eks"`, `"cluster":"db-eks","manifest":"/etc/passwd"`, 1)),
			"relative path",
		},
		"manifest escapes": {
			[]byte(strings.Replace(string(planJSON("")), `"cluster":"db-eks"`, `"cluster":"db-eks","manifest":"cnpg/../../secret.yaml"`, 1)),
			"must not leave",
		},
		"written records escapes": {
			planJSON(`"verify": {"writtenRecords":"..\\..\\k6.jsonl"}`),
			"must not leave",
		},
		"gateway elsewhere": {
			planJSON(`"gateway": {"mode":"http","url":"http://evil.example.com","token":"${GATEWAY_TOKEN}"}`),
			"must be the platform gateway",
		},
		"engine validation": {
			[]byte(strings.Replace(string(planJSON("")), `"env":"eks"`, `"env":"gke"`, 1)),
			"source and target env are both",
		},
		"not json": {[]byte("{"), "decode plan"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := newService(policy).Submit(ctx, tc.raw, "x")
			if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want ErrInvalid containing %q", err, tc.want)
			}
		})
	}
}

// gateway.url is the gateway root only for databases no registered app owns,
// and otherwise exactly the scope of the app that owns both (ADR 0012).
func TestGatewayScopeFollowsThePlansApp(t *testing.T) {
	policy := DefaultPolicy()
	policy.GatewayURL = "http://gateway.ts.net:9901/"
	policy.AppDBHosts = map[string][]string{
		"shop":  {"db-eks.shop.ts.net:5432", "DB-GKE.shop.ts.net:5432"},
		"other": {"db-other.ts.net:5432"},
		"half":  {"db-gke.half.ts.net:5432"},
	}
	gw := func(url string) []byte {
		return planJSON(`"gateway": {"mode":"http","url":"` + url + `","token":"${GATEWAY_TOKEN}"}`)
	}
	moveHosts := func(raw []byte, src, dst string) []byte {
		s := strings.Replace(string(raw), "db-gke.shop.ts.net", src, 1)
		return []byte(strings.Replace(s, "db-eks.shop.ts.net", dst, 1))
	}
	ok := map[string][]byte{
		"own app's scope":             gw("http://gateway.ts.net:9901/apps/shop"),
		"own app's scope, slash":      gw("http://gateway.ts.net:9901/apps/shop/"),
		"root for unregistered hosts": moveHosts(gw("http://gateway.ts.net:9901"), "db-a.ts.net", "db-b.ts.net"),
	}
	for name, raw := range ok {
		if _, err := newService(policy).Submit(ctx, raw, "x"); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	bad := map[string]struct {
		raw  []byte
		want string
	}{
		"root for an app's plan":    {gw("http://gateway.ts.net:9901"), "not the gateway root"},
		"another app's scope":       {gw("http://gateway.ts.net:9901/apps/other"), "scope of app shop (http://gateway.ts.net:9901/apps/shop)"},
		"unknown app's scope":       {gw("http://gateway.ts.net:9901/apps/nope"), "scope of app shop"},
		"extra path":                {gw("http://gateway.ts.net:9901/apps/shop/v1"), "scope of app shop"},
		"query":                     {gw("http://gateway.ts.net:9901/apps/shop?x=1"), "scope of app shop"},
		"scope on another host":     {gw("http://evil.example.com/apps/shop"), "scope of app shop"},
		"scope for unowned hosts":   {moveHosts(gw("http://gateway.ts.net:9901/apps/shop"), "db-a.ts.net", "db-b.ts.net"), "owns both"},
		"root, one app's database":  {moveHosts(gw("http://gateway.ts.net:9901"), "db-gke.half.ts.net", "db-b.ts.net"), "(half), not the gateway root"},
		"half app's scope, one db":  {moveHosts(gw("http://gateway.ts.net:9901/apps/half"), "db-gke.half.ts.net", "db-b.ts.net"), "owns both"},
		"two apps' databases mixed": {moveHosts(gw("http://gateway.ts.net:9901/apps/shop"), "db-gke.shop.ts.net", "db-other.ts.net"), "owns both"},
	}
	for name, tc := range bad {
		_, err := newService(policy).Submit(ctx, tc.raw, "x")
		if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
}

func TestTargetAppIsRefused(t *testing.T) {
	raw := []byte(strings.Replace(string(planJSON(`"gateway": {"mode":"http","url":"http://gw:9901","token":"${GATEWAY_TOKEN}"}`)), `"cluster":"db-eks"`,
		`"cluster":"db-eks","app":{"mode":"cold","kind":"Deployment","name":"web","replicas":1,"smokeUrl":"http://web:8080","smoke":[{"method":"GET","path":"/"}]}`, 1))
	policy := DefaultPolicy()
	policy.GatewayURL = "http://gw:9901"
	_, err := newService(policy).Submit(ctx, raw, "x")
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "target.app is not accepted") {
		t.Fatalf("err = %v", err)
	}
}

func TestGatewayHTTPDisabledWithoutConfiguredURL(t *testing.T) {
	_, err := newService(DefaultPolicy()).Submit(ctx, planJSON(`"gateway": {"mode":"http","url":"http://gw:9901"}`), "x")
	if err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("err = %v", err)
	}
}

func TestDSNErrorsNeverEchoSecrets(t *testing.T) {
	t.Setenv("SRC_PGPASSWORD", "TOPSECRET")
	p := DefaultPolicy()
	p.DSNHostSuffixes = []string{".ts.net"}
	raw := []byte(strings.Replace(string(planJSON("")), "postgres://app:${SRC_PGPASSWORD}@db-gke.shop.ts.net/app", "postgres://app:${SRC_PGPASSWORD}@[::1", 1))
	_, err := newService(p).Submit(ctx, raw, "x")
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "TOPSECRET") {
		t.Fatalf("error leaks the password: %v", err)
	}
}

func TestLifecycle(t *testing.T) {
	s := newService(DefaultPolicy())
	rec, err := s.Submit(ctx, planJSON(""), "ai-agent")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reserve(ctx, rec.Ref); !errors.Is(err, ErrWrongStatus) {
		t.Fatalf("reserving an unapproved plan: %v", err)
	}
	if rec, err = s.Approve(ctx, rec.Ref, "operator", "ok", nil); err != nil || rec.Status != apiv1.PlanStatusApproved || *rec.ReviewedBy != "operator" || *rec.ReviewComment != "ok" {
		t.Fatalf("approve: %+v %v", rec, err)
	}
	if _, err := s.Approve(ctx, rec.Ref, "operator", "", nil); err != nil {
		t.Fatalf("second approve should be a no-op: %v", err)
	}
	res, err := s.Reserve(ctx, rec.Ref)
	if err != nil || res.Plan.ID != "shop-gke-to-eks" || res.Summary.PlanId != "shop-gke-to-eks" || res.RollbackOf != "" {
		t.Fatalf("reserve: %v", err)
	}
	if _, err := s.Reserve(ctx, rec.Ref); !errors.Is(err, ErrWrongStatus) {
		t.Fatalf("double reserve: %v", err)
	}
	s.Release(ctx, rec.Ref)
	if got, _ := s.Get(ctx, rec.Ref); got.Status != apiv1.PlanStatusApproved {
		t.Fatalf("after release: %s", got.Status)
	}
	if _, err := s.Reserve(ctx, rec.Ref); err != nil {
		t.Fatal(err)
	}
	if err := s.Started(ctx, rec.Ref, "run-1"); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get(ctx, rec.Ref)
	if got.Status != apiv1.PlanStatusStarted || *got.RunId != "run-1" {
		t.Fatalf("after start: %+v", got)
	}
	if _, err := s.Approve(ctx, rec.Ref, "x", "", nil); !errors.Is(err, ErrWrongStatus) {
		t.Fatalf("approving a started plan: %v", err)
	}
	if _, err := s.Get(ctx, "pln_missing"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if l, _ := s.List(ctx); len(l) != 1 {
		t.Fatal("list")
	}
}

func TestRecoverStartingLinksAnExistingRun(t *testing.T) {
	st := storetest.NewMemory()
	s := New(DefaultPolicy(), st)
	rec, _ := s.Submit(ctx, planJSON(""), "ai")
	_, _ = s.Approve(ctx, rec.Ref, "operator", "", nil)
	if _, err := s.Reserve(ctx, rec.Ref); err != nil {
		t.Fatal(err)
	}
	// The run was created, then the server died before Started.
	if err := st.CreateRun(ctx, store.Run{RunID: "run-1", PlanRef: rec.Ref, Status: "running", Doc: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if n, err := s.RecoverStarting(ctx); err != nil || n != 1 {
		t.Fatalf("recover = %d, %v", n, err)
	}
	got, _ := s.Get(ctx, rec.Ref)
	if got.Status != apiv1.PlanStatusStarted || *got.RunId != "run-1" {
		t.Fatalf("a plan with a run must not go back to approved: %+v", got)
	}
}

func TestPlanIDMustBeSafe(t *testing.T) {
	for _, id := range []string{"../../x", "Shop", "a/b", "-a", ""} {
		raw := []byte(strings.Replace(string(planJSON("")), `"id": "shop-gke-to-eks"`, `"id": "`+id+`"`, 1))
		if _, err := newService(DefaultPolicy()).Submit(ctx, raw, "x"); !errors.Is(err, ErrInvalid) {
			t.Errorf("id %q accepted: %v", id, err)
		}
	}
}

func TestRejectAndRecoverStarting(t *testing.T) {
	s := newService(DefaultPolicy())
	a, _ := s.Submit(ctx, planJSON(""), "ai")
	if r, err := s.Reject(ctx, a.Ref, "operator", "wrong target"); err != nil || r.Status != apiv1.PlanStatusRejected {
		t.Fatalf("reject: %+v %v", r, err)
	}
	if _, err := s.Approve(ctx, a.Ref, "operator", "", nil); !errors.Is(err, ErrWrongStatus) {
		t.Fatalf("approve after reject: %v", err)
	}
	b, _ := s.Submit(ctx, planJSON(""), "ai")
	_, _ = s.Approve(ctx, b.Ref, "operator", "", nil)
	if _, err := s.Reserve(ctx, b.Ref); err != nil {
		t.Fatal(err)
	}
	// The server crashed between Reserve and Started.
	if n, err := s.RecoverStarting(ctx); err != nil || n != 1 {
		t.Fatalf("recover = %d, %v", n, err)
	}
	if got, _ := s.Get(ctx, b.Ref); got.Status != apiv1.PlanStatusApproved {
		t.Fatalf("after recover: %s", got.Status)
	}
}

// rclone takes a backend definition from the remote part of a path, so only
// registered buckets may reach it.
func TestPolicyRejectsUnregisteredBuckets(t *testing.T) {
	t.Setenv("SRC_PGPASSWORD", "hunter2")
	policy := DefaultPolicy()
	policy.Buckets = []string{"gcs:photos-gke", "s3:photos-eks"}
	cases := map[string]struct{ source, target, want string }{
		"local backend":       {":local:/data/runs", "s3:photos-eks", `files.source ":local:/data/runs" is not a registered bucket`},
		"inline s3 backend":   {"gcs:photos-gke", ":s3,access_key_id=AKIA,secret_access_key=x:evil", `files.target ":s3,access_key_id=AKIA,secret_access_key=x:evil" is not a registered bucket`},
		"remote with options": {"gcs,service_account_file=/home/app/.kube/config:photos-gke", "s3:photos-eks", "is not a registered bucket"},
		"other bucket":        {"gcs:photos-gke", "s3:someone-else", `files.target "s3:someone-else" is not a registered bucket`},
		"sub path":            {"gcs:photos-gke/../x", "s3:photos-eks", "is not a registered bucket"},
		"same bucket":         {"gcs:photos-gke", "gcs:photos-gke", "must be different buckets"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			files, _ := json.Marshal(map[string]string{"source": c.source, "target": c.target})
			_, err := newService(policy).Submit(ctx, planJSON(`"files": `+string(files)), "x")
			if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want %q", err, c.want)
			}
		})
	}
	if _, err := newService(policy).Submit(ctx, planJSON(`"files": {"source":"gcs:photos-gke","target":"s3:photos-eks"}`), "x"); err != nil {
		t.Fatalf("registered buckets: %v", err)
	}
}

func TestCheckBucket(t *testing.T) {
	for _, ok := range []string{"gcs:moimroom-photos-gke", "s3:a.b_c-d", "minio:photos/2026/uploads", "my-remote:b"} {
		if err := CheckBucket(ok); err != nil {
			t.Errorf("CheckBucket(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"", "gcs", ":local:/etc", "gcs,x=y:b", "gcs:", "gcs:/abs", "gcs:b/../c", "gcs:b/./c", "gcs:b c", "gcs:b'", `gcs:b\c`} {
		if err := CheckBucket(bad); err == nil {
			t.Errorf("CheckBucket(%q) accepted", bad)
		}
	}
	if got := RcloneTypeEnv("my-remote:b/c"); got != "RCLONE_CONFIG_MY_REMOTE_TYPE" {
		t.Errorf("RcloneTypeEnv = %q", got)
	}
}
