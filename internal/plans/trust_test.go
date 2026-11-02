package plans

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store/storetest"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

func ptr[T any](v T) *T { return &v }

// secretEnv sets server variables a plan must never be able to read, and
// returns their values.
func secretEnv(t *testing.T) []string {
	t.Helper()
	vals := map[string]string{
		"PLATFORM_DATABASE_URL": "postgres://platform:TOPSECRET-db@platform-db.internal:5432/platform",
		"AWS_SECRET_ACCESS_KEY": "TOPSECRET-aws",
		"GATEWAY_TOKEN":         "TOPSECRET-gw",
		"SRC_PGPASSWORD":        "TOPSECRET-src",
		"DST_PGPASSWORD":        "TOPSECRET-dst",
		"GKE_SHOP_PGPASSWORD":   "TOPSECRET-gke",
	}
	var out []string
	for k, v := range vals {
		t.Setenv(k, v)
		out = append(out, v)
	}
	return out
}

func noSecret(t *testing.T, what, s string, secrets []string) {
	t.Helper()
	for _, sec := range secrets {
		if strings.Contains(s, sec) || strings.Contains(s, "TOPSECRET") {
			t.Errorf("%s leaks a server variable: %s", what, s)
			return
		}
	}
}

// esc writes JSON unicode escapes the way an attacker would: <$> becomes the
// escape for "$" (backslash, u0024), <{> and <}> those for the braces. The raw
// bytes then hold no "$" at all.
func esc(s string) string {
	u := string(rune(92)) + "u00"
	return strings.NewReplacer("<$>", u+"24", "<{>", u+"7b", "<}>", u+"7d").Replace(s)
}

func TestEscIsAJSONEscape(t *testing.T) {
	raw := `"` + esc(`<$><{>X<}>`) + `"`
	var v string
	if err := json.Unmarshal([]byte(raw), &v); err != nil || v != "${X}" || strings.ContainsAny(raw, "${}") {
		t.Fatalf("esc: raw %s decodes to %q (%v)", raw, v, err)
	}
}

// withDSN replaces the source DSN of the base plan; dsn is spliced into the
// JSON as is, so it may hold JSON escapes (esc).
func withDSN(dsn string) []byte {
	return []byte(strings.Replace(string(planJSON("")), "postgres://app:${SRC_PGPASSWORD}@db-gke.shop.ts.net/app", dsn, 1))
}

func trustPolicy() Policy {
	p := DefaultPolicy()
	p.GatewayURL = "http://gateway.ts.net:9901"
	p.HostPasswordEnvs = map[string][]string{
		"GKE_SHOP_PGPASSWORD":  {"db-gke.shop.ts.net:5432"},
		"EKS_SHOP_PGPASSWORD":  {"db-eks.shop.ts.net:5432"},
		"GKE_OTHER_PGPASSWORD": {"db-other.gke.ts.net:5432"},
	}
	return p
}

func TestEnvReferencesAreCheckedAfterJSONDecoding(t *testing.T) {
	secrets := secretEnv(t)
	gw := func(url, token string) []byte {
		return planJSON(`"gateway": {"mode":"http","url":"` + url + `","token":"` + token + `"}`)
	}
	for name, raw := range map[string][]byte{
		// JSON escapes decode to "$" only after a check on the raw bytes.
		"escaped ${VAR} as the dsn":      withDSN(esc(`<$>{PLATFORM_DATABASE_URL}`)),
		"escaped $VAR as the dsn":        withDSN(esc(`<$>PLATFORM_DATABASE_URL`)),
		"escaped ${VAR} as the host":     withDSN(esc(`postgres://app:${SRC_PGPASSWORD}@<$>{PLATFORM_DATABASE_URL}/app`)),
		"escaped $VAR as the host":       withDSN(esc(`postgres://app:${SRC_PGPASSWORD}@<$>AWS_SECRET_ACCESS_KEY/app`)),
		"escaped ${VAR} as the password": withDSN(esc(`postgres://app:<$>{PLATFORM_DATABASE_URL}@db-gke.shop.ts.net/app`)),
		"escaped braces too":             withDSN(esc(`postgres://app:<$><{>AWS_SECRET_ACCESS_KEY<}>@db-gke.shop.ts.net/app`)),
		// Allowed variables in any other position.
		"reference as the host":       withDSN(`postgres://app:${SRC_PGPASSWORD}@${DST_PGPASSWORD}/app`),
		"reference in the query":      withDSN(`postgres://app:${SRC_PGPASSWORD}@db-gke.shop.ts.net/app?application_name=${DST_PGPASSWORD}`),
		"bare reference in the query": withDSN(`postgres://app@db-gke.shop.ts.net/app?sslmode=$SRC_PGPASSWORD`),
		"reference as the user":       withDSN(`postgres://${SRC_PGPASSWORD}@db-gke.shop.ts.net/app`),
		"reference as the port":       withDSN(`postgres://app:${SRC_PGPASSWORD}@db-gke.shop.ts.net:${DST_PGPASSWORD}/app`),
		"reference in the path":       withDSN(`postgres://app:${SRC_PGPASSWORD}@db-gke.shop.ts.net/${DST_PGPASSWORD}`),
		"part of the password":        withDSN(`postgres://app:x${SRC_PGPASSWORD}@db-gke.shop.ts.net/app`),
		"two references":              withDSN(`postgres://app:${SRC_PGPASSWORD}${DST_PGPASSWORD}@db-gke.shop.ts.net/app`),
		"userinfo with a second @":    withDSN(`postgres://app:${SRC_PGPASSWORD}@evil.example.com@db-gke.shop.ts.net/app`),
		"key=value dsn":               withDSN(`host=db-gke.shop.ts.net password=${SRC_PGPASSWORD}`),
		// encoding/json matches keys case-insensitively and keeps the last one.
		"second dsn key in another case": withDSN(esc(`postgres://app:${SRC_PGPASSWORD}@db-gke.shop.ts.net/app","DSN":"<$>{PLATFORM_DATABASE_URL}`)),
		// Gateway fields.
		"escaped ${VAR} as the gateway url": gw(esc(`<$>{PLATFORM_DATABASE_URL}`), "${GATEWAY_TOKEN}"),
		"reference in the gateway url":      gw("http://gateway.ts.net:9901/${GATEWAY_TOKEN}", "${GATEWAY_TOKEN}"),
		"escaped ${VAR} as the token":       gw("http://gateway.ts.net:9901", esc(`<$>{PLATFORM_DATABASE_URL}`)),
		"escaped $VAR as the token":         gw("http://gateway.ts.net:9901", esc(`<$>PLATFORM_DATABASE_URL`)),
		"password variable as the token":    gw("http://gateway.ts.net:9901", "${SRC_PGPASSWORD}"),
		"literal token":                     gw("http://gateway.ts.net:9901", "abc"),
		"token with more text":              gw("http://gateway.ts.net:9901", "Bearer ${GATEWAY_TOKEN}"),
		"bare token reference":              gw("http://gateway.ts.net:9901", "$GATEWAY_TOKEN"),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := New(trustPolicy(), storetest.NewMemory()).Submit(ctx, raw, "planner")
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("accepted: %v", err)
			}
			noSecret(t, "error", err.Error(), secrets)
		})
	}
}

func TestSummaryIsBuiltFromThePlanAsSubmitted(t *testing.T) {
	secrets := secretEnv(t)
	// A password that is not URL-safe moves the host of the expanded DSN:
	// postgres://app:7777/x@db-gke... would show "app:7777".
	t.Setenv("SRC_PGPASSWORD", "7777/TOPSECRET")
	t.Setenv("DST_PGPASSWORD", "TOPSECRET@other")
	s := New(trustPolicy(), storetest.NewMemory())
	rec, err := s.Submit(ctx, planJSON(`"gateway": {"mode":"http","url":"http://gateway.ts.net:9901","token":"${GATEWAY_TOKEN}"}`), "planner")
	if err != nil {
		t.Fatal(err)
	}
	if deref(rec.Summary.Source.DbHost) != "db-gke.shop.ts.net:5432" || deref(rec.Summary.Target.DbHost) != "db-eks.shop.ts.net:5432" {
		t.Fatalf("db hosts: %q %q", deref(rec.Summary.Source.DbHost), deref(rec.Summary.Target.DbHost))
	}
	b, _ := json.Marshal(rec)
	noSecret(t, "plan record", string(b), append(secrets, "7777"))

	if _, err := s.Approve(ctx, rec.Ref, "op", "", nil); err != nil {
		t.Fatal(err)
	}
	res, err := s.Reserve(ctx, rec.Ref)
	if err != nil {
		t.Fatal(err)
	}
	b, _ = json.Marshal(res.Summary)
	noSecret(t, "reserved summary", string(b), append(secrets, "7777"))
	if res.Plan.Source.DSN != "postgres://app:7777/TOPSECRET@db-gke.shop.ts.net/app" || res.Plan.Gateway.Token != "TOPSECRET-gw" {
		t.Fatalf("the engine plan is not expanded: %q %q", res.Plan.Source.DSN, res.Plan.Gateway.Token)
	}
}

func TestPolicyErrorsComeFromTheTemplate(t *testing.T) {
	secrets := secretEnv(t)
	p := trustPolicy()
	p.DSNHostSuffixes = []string{".ts.net"}
	_, err := New(p, storetest.NewMemory()).Submit(ctx, withDSN("postgres://app:${SRC_PGPASSWORD}@attacker.example.com/app"), "x")
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), `host "attacker.example.com" is not an allowed database host`) {
		t.Fatalf("err = %v", err)
	}
	noSecret(t, "error", err.Error(), secrets)
}

func TestAppPasswordOnlyGoesToItsHost(t *testing.T) {
	secrets := secretEnv(t)
	s := New(trustPolicy(), storetest.NewMemory())
	plan := func(src, dst string) []byte {
		raw := strings.Replace(string(planJSON("")), "postgres://app:${SRC_PGPASSWORD}@db-gke.shop.ts.net/app", src, 1)
		return []byte(strings.Replace(raw, "postgres://app:${DST_PGPASSWORD}@db-eks.shop.ts.net/app", dst, 1))
	}
	good := plan("postgres://app:${GKE_SHOP_PGPASSWORD}@db-gke.shop.ts.net:5432/app?sslmode=require",
		"postgres://app:${EKS_SHOP_PGPASSWORD}@DB-EKS.shop.ts.net/app")
	rec, err := s.Submit(ctx, good, "planner")
	if err != nil {
		t.Fatal(err)
	}
	// Rollback swaps the DSNs; each variable still goes to its own host.
	if _, err := s.Rollback(ctx, "run_1", rec.Ref, "op", ""); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	// The default password variables go to either DSN.
	if _, err := s.Submit(ctx, plan("postgres://app:${DST_PGPASSWORD}@db-gke.shop.ts.net/app", "postgres://app:${SRC_PGPASSWORD}@db-eks.shop.ts.net/app"), "x"); err != nil {
		t.Fatal(err)
	}

	eks := "postgres://app:${EKS_SHOP_PGPASSWORD}@db-eks.shop.ts.net/app"
	for name, raw := range map[string][]byte{
		"another environment's variable": plan("postgres://app:${EKS_SHOP_PGPASSWORD}@db-gke.shop.ts.net/app", eks),
		"another app's variable":         plan("postgres://app:${GKE_OTHER_PGPASSWORD}@db-gke.shop.ts.net/app", eks),
		"another host":                   plan("postgres://app:${GKE_SHOP_PGPASSWORD}@attacker.example.com/app", eks),
		"another port":                   plan("postgres://app:${GKE_SHOP_PGPASSWORD}@db-gke.shop.ts.net:6543/app", eks),
		"host in the query":              plan("postgres://app:${GKE_SHOP_PGPASSWORD}@db-gke.shop.ts.net/app?host=attacker.example.com", eks),
		"a second host":                  plan("postgres://app:${GKE_SHOP_PGPASSWORD}@db-gke.shop.ts.net,attacker.example.com/app", eks),
		"unknown variable":               plan("postgres://app:${ONPREM_SHOP_PGPASSWORD}@db-gke.shop.ts.net/app", eks),
	} {
		_, err := s.Submit(ctx, raw, "x")
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: accepted (%v)", name, err)
			continue
		}
		noSecret(t, name, err.Error(), secrets)
	}
}

func TestCheckPasswordEnv(t *testing.T) {
	for _, ok := range []string{"SRC_PGPASSWORD", "DST_PGPASSWORD", "GKE_MOIMROOM_PGPASSWORD", "A1_PGPASSWORD"} {
		if err := CheckPasswordEnv(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "GATEWAY_TOKEN", "PLATFORM_DATABASE_URL", "PLATFORM_DB_PGPASSWORD", "AWS_SECRET_ACCESS_KEY",
		"AWS_RDS_PGPASSWORD", "PGPASSWORD", "_PGPASSWORD", "gke_pgpassword", "GKE_PGPASSWORD_X", "HOME"} {
		if err := CheckPasswordEnv(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestApproveNeedsTheReviewedRevision(t *testing.T) {
	s := New(DefaultPolicy(), storetest.NewMemory())
	s.SetStateDir(t.TempDir())
	rec, err := s.Submit(ctx, withManifest("attachment:target.yaml"), "planner")
	if err != nil || rec.Revision != 1 {
		t.Fatalf("submit: revision %d, %v", rec.Revision, err)
	}
	if _, err := s.PutAttachment(ctx, rec.Ref, "target.yaml", []byte("kind: Cluster\n"), "planner"); err != nil {
		t.Fatal(err)
	}
	reviewed, _ := s.Get(ctx, rec.Ref)
	// The content changes after the operator looked at revision 2.
	if _, err := s.PutAttachment(ctx, rec.Ref, "target.yaml", []byte("kind: Cluster\nspec: {evil: true}\n"), "planner"); err != nil {
		t.Fatal(err)
	}
	if reviewed.Revision != 2 {
		t.Fatalf("reviewed revision = %d", reviewed.Revision)
	}

	if _, err := s.Approve(ctx, rec.Ref, "op", "", nil); !errors.Is(err, ErrWrongStatus) || !strings.Contains(err.Error(), "needs the revision you reviewed") {
		t.Fatalf("approve without a revision: %v", err)
	}
	_, err = s.Approve(ctx, rec.Ref, "op", "", ptr(reviewed.Revision))
	if !errors.Is(err, ErrWrongStatus) || !strings.Contains(err.Error(), "changed since you reviewed it (revision 2, now 3)") {
		t.Fatalf("approve a stale revision: %v", err)
	}
	if got, _ := s.Get(ctx, rec.Ref); got.Status != apiv1.PlanStatusPendingApproval {
		t.Fatalf("a refused approval changed the plan: %s", got.Status)
	}
	got, err := s.Approve(ctx, rec.Ref, "op", "ok", ptr[int64](3))
	if err != nil || got.Status != apiv1.PlanStatusApproved || got.Revision != 3 {
		t.Fatalf("approve the current revision: %+v %v", got, err)
	}
	if _, err := s.Approve(ctx, rec.Ref, "op", "", ptr[int64](3)); err != nil {
		t.Fatalf("approving again is a no-op: %v", err)
	}
	if _, err := s.Approve(ctx, rec.Ref, "op", "", ptr[int64](2)); !errors.Is(err, ErrWrongStatus) || !strings.Contains(err.Error(), "revision 2, now 3") {
		t.Fatalf("approving again with another revision: %v", err)
	}

	// Without attachments the revision is optional, and checked when given.
	plain, _ := s.Submit(ctx, planJSON(""), "planner")
	if _, err := s.Approve(ctx, plain.Ref, "op", "", ptr[int64](5)); !errors.Is(err, ErrWrongStatus) || !strings.Contains(err.Error(), "revision 5, now 1") {
		t.Fatalf("wrong revision of a plan without attachments: %v", err)
	}
	if got, err := s.Approve(ctx, plain.Ref, "op", "", nil); err != nil || got.Status != apiv1.PlanStatusApproved {
		t.Fatalf("approve without a revision: %+v %v", got, err)
	}
}

func TestOneRollbackPlanPerRun(t *testing.T) {
	s := New(DefaultPolicy(), storetest.NewMemory())
	orig, err := s.Submit(ctx, planJSON(""), "planner")
	if err != nil {
		t.Fatal(err)
	}
	if _, found, err := s.FindRollback(ctx, "run_1"); found || err != nil {
		t.Fatalf("rollback of a run nobody rolled back: %v %v", found, err)
	}
	rec, err := s.Rollback(ctx, "run_1", orig.Ref, "op", "")
	if err != nil {
		t.Fatal(err)
	}
	got, found, err := s.FindRollback(ctx, "run_1")
	if err != nil || !found || got.Ref != rec.Ref || deref(got.RollbackOf) != "run_1" {
		t.Fatalf("find rollback = %+v %v %v", got, found, err)
	}
	if _, err := s.Rollback(ctx, "run_1", orig.Ref, "op", ""); !errors.Is(err, ErrWrongStatus) {
		t.Fatalf("second rollback plan for the same run: %v", err)
	}
	if _, err := s.Rollback(ctx, "run_2", orig.Ref, "op", ""); err != nil {
		t.Fatalf("rollback of another run: %v", err)
	}
	if _, found, _ := s.FindRollback(ctx, "run_3"); found {
		t.Fatal("found a rollback of run_3")
	}
}
