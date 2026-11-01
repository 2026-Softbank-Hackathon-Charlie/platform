package plans

import (
	"errors"
	"strings"
	"testing"
)

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
	s := New(DefaultPolicy())
	rec, err := s.Submit(planJSON(`"files": {"source":"gcs:b","target":"s3:b","flags":["--transfers","16","--checksum"]}`), "alice")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Status != StatusPending || !strings.HasPrefix(rec.Ref, "pln_") || rec.SubmittedBy != "alice" {
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
			`"HOME"`,
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
			_, err := New(policy).Submit(tc.raw, "x")
			if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want ErrInvalid containing %q", err, tc.want)
			}
		})
	}
}

func TestGatewayHTTPDisabledWithoutConfiguredURL(t *testing.T) {
	_, err := New(DefaultPolicy()).Submit(planJSON(`"gateway": {"mode":"http","url":"http://gw:9901"}`), "x")
	if err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("err = %v", err)
	}
}

func TestDSNErrorsNeverEchoSecrets(t *testing.T) {
	t.Setenv("SRC_PGPASSWORD", "TOPSECRET")
	p := DefaultPolicy()
	p.DSNHostSuffixes = []string{".ts.net"}
	raw := []byte(strings.Replace(string(planJSON("")), "postgres://app:${SRC_PGPASSWORD}@db-gke.shop.ts.net/app", "postgres://app:${SRC_PGPASSWORD}@[::1", 1))
	_, err := New(p).Submit(raw, "x")
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "TOPSECRET") {
		t.Fatalf("error leaks the password: %v", err)
	}
}

func TestLifecycle(t *testing.T) {
	s := New(DefaultPolicy())
	rec, err := s.Submit(planJSON(""), "ai-agent")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reserve(rec.Ref); !errors.Is(err, ErrWrongStatus) {
		t.Fatalf("reserving an unapproved plan: %v", err)
	}
	if rec, err = s.Approve(rec.Ref, "operator"); err != nil || rec.Status != StatusApproved || rec.ApprovedBy != "operator" {
		t.Fatalf("approve: %+v %v", rec, err)
	}
	if _, err := s.Approve(rec.Ref, "operator"); err != nil {
		t.Fatalf("second approve should be a no-op: %v", err)
	}
	p, err := s.Reserve(rec.Ref)
	if err != nil || p.ID != "shop-gke-to-eks" {
		t.Fatalf("reserve: %v", err)
	}
	if _, err := s.Reserve(rec.Ref); !errors.Is(err, ErrWrongStatus) {
		t.Fatalf("double reserve: %v", err)
	}
	s.Release(rec.Ref)
	if got, _ := s.Get(rec.Ref); got.Status != StatusApproved {
		t.Fatalf("after release: %s", got.Status)
	}
	if _, err := s.Reserve(rec.Ref); err != nil {
		t.Fatal(err)
	}
	s.Started(rec.Ref, "run-1")
	got, _ := s.Get(rec.Ref)
	if got.Status != StatusStarted || got.RunID != "run-1" {
		t.Fatalf("after start: %+v", got)
	}
	if _, err := s.Approve(rec.Ref, "x"); !errors.Is(err, ErrWrongStatus) {
		t.Fatalf("approving a started plan: %v", err)
	}
	if _, err := s.Get("pln_missing"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if len(s.List()) != 1 {
		t.Fatal("list")
	}
}
