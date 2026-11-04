package pgversion

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"
)

func v(major int, full, from string) *Version { return &Version{Major: major, Full: full, From: from} }

func TestCompare(t *testing.T) {
	src := v(17, "17.11", "server")
	for name, tc := range map[string]struct {
		src, dst   *Version
		targetCode string
		status     Status
		code       string
		message    []string
	}{
		"same major, different minor": {src, v(17, "17.6", "manifest image ghcr.io/cloudnative-pg/postgresql:17.6"), "", Pass, CodeMatch,
			[]string{"17 (17.11, from server)", "17 (17.6, from manifest image ghcr.io/cloudnative-pg/postgresql:17.6)"}},
		"17 to 18": {src, v(18, "18.6", "manifest image ghcr.io/cloudnative-pg/postgresql:18.6-minimal-trixie"), "", Fail, CodeMismatch,
			[]string{"source PostgreSQL 17 (17.11, from server)", "target 18 (18.6", "set the target's spec.imageName to a 17.x image"}},
		"newer source": {v(18, "18.6", "server"), v(17, "17.11", "server"), "", Fail, CodeMismatch, []string{"18.x image"}},
		"no source":    {nil, v(17, "17.11", "server"), "", Unknown, CodeSourceUnknown, []string{"not compared"}},
		"no image":     {src, nil, CodeImageDefault, Unknown, CodeImageDefault, []string{"17 (17.11, from server)", "operator's default image"}},
		"latest tag":   {src, nil, CodeImageUnparsed, Unknown, CodeImageUnparsed, []string{"names no version"}},
		"no target":    {src, nil, "", Unknown, CodeTargetUnknown, []string{"could not be read"}},
	} {
		t.Run(name, func(t *testing.T) {
			r := Compare(tc.src, tc.dst, tc.targetCode)
			if r.Status != tc.status || r.Code != tc.code {
				t.Fatalf("%s %s, want %s %s: %s", r.Status, r.Code, tc.status, tc.code, r.Message)
			}
			for _, s := range tc.message {
				if !strings.Contains(r.Message, s) {
					t.Fatalf("message %q lacks %q", r.Message, s)
				}
			}
		})
	}
}

func manifest(t *testing.T, spec string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "target.yaml")
	doc := "apiVersion: postgresql.cnpg.io/v1\nkind: Cluster\nmetadata:\n  name: shop-db-eks\nspec:\n  instances: 1\n" + spec
	if err := os.WriteFile(p, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestManifestVersion(t *testing.T) {
	for name, tc := range map[string]struct {
		spec  string
		major int
		full  string
		code  string
	}{
		"plain tag":             {"  imageName: ghcr.io/cloudnative-pg/postgresql:17.11\n", 17, "17.11", ""},
		"flavoured tag":         {"  imageName: ghcr.io/cloudnative-pg/postgresql:18.6-minimal-trixie\n", 18, "18.6", ""},
		"digest":                {"  imageName: ghcr.io/cloudnative-pg/postgresql:18.6-minimal-trixie@sha256:64abc98a4387361afd6821a3a99f975763bbd87bbdde9449e75ad6002528a318\n", 18, "18.6", ""},
		"major only":            {"  imageName: ghcr.io/cloudnative-pg/postgresql:16\n", 16, "16", ""},
		"registry with port":    {"  imageName: registry.local:5000/pg/postgresql:17.2\n", 17, "17.2", ""},
		"image catalog":         {"  imageCatalogRef:\n    apiGroup: postgresql.cnpg.io\n    kind: ClusterImageCatalog\n    name: pg\n    major: 17\n", 17, "17", ""},
		"catalog without major": {"  imageCatalogRef:\n    kind: ImageCatalog\n    name: pg\n", 0, "", CodeImageUnparsed},
		"no image":              {"  storage:\n    size: 1Gi\n", 0, "", CodeImageDefault},
		"latest":                {"  imageName: ghcr.io/cloudnative-pg/postgresql:latest\n", 0, "", CodeImageUnparsed},
		"no tag":                {"  imageName: registry.local:5000/postgresql\n", 0, "", CodeImageUnparsed},
		"digest only":           {"  imageName: ghcr.io/cloudnative-pg/postgresql@sha256:64abc98a4387361afd6821a3a99f975763bbd87bbdde9449e75ad6002528a318\n", 0, "", CodeImageUnparsed},
	} {
		t.Run(name, func(t *testing.T) {
			got, code := ManifestVersion(manifest(t, tc.spec))
			if code != tc.code {
				t.Fatalf("code %q, want %q", code, tc.code)
			}
			if tc.major == 0 {
				if got != nil {
					t.Fatalf("version %+v, want none", got)
				}
				return
			}
			if got == nil || got.Major != tc.major || got.Full != tc.full || !strings.HasPrefix(got.From, "manifest image") {
				t.Fatalf("version %+v", got)
			}
		})
	}
	if got, code := ManifestVersion(""); got != nil || code != CodeTargetUnknown {
		t.Fatalf("no manifest: %+v %q", got, code)
	}
	if got, code := ManifestVersion(filepath.Join(t.TempDir(), "missing.yaml")); got != nil || code != CodeTargetUnknown {
		t.Fatalf("missing file: %+v %q", got, code)
	}
	bad := filepath.Join(t.TempDir(), "bad.yaml")
	_ = os.WriteFile(bad, []byte("spec: [\n"), 0o600)
	if got, code := ManifestVersion(bad); got != nil || code != CodeTargetUnknown {
		t.Fatalf("bad yaml: %+v %q", got, code)
	}
}

// Unreachable databases make both sides unknown quickly, and nothing of the
// DSN reaches the result.
func TestCheckWithoutDatabases(t *testing.T) {
	p := &plan.Plan{
		Source: plan.Endpoint{DSN: "postgres://app:TOPSECRET@127.0.0.1:1/app?sslmode=disable&connect_timeout=1"},
		Target: plan.Endpoint{DSN: "postgres://app:TOPSECRET@127.0.0.1:1/app?sslmode=disable&connect_timeout=1",
			Manifest: manifest(t, "  imageName: ghcr.io/cloudnative-pg/postgresql:18.6\n")},
	}
	start := time.Now()
	r, err := Checker{}.Check(context.Background(), p)
	if err != nil || r.Status != Unknown || r.Code != CodeSourceUnknown || r.Target == nil || r.Target.Major != 18 {
		t.Fatalf("%+v %v", r, err)
	}
	if strings.Contains(r.Message, "TOPSECRET") || strings.Contains(r.Message, "127.0.0.1") {
		t.Fatalf("message leaks the DSN: %q", r.Message)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("took %s", time.Since(start))
	}
}

// With PLATFORM_TEST_DATABASE_URL the real server version is read and
// compared with a manifest.
func TestCheckReadsServerVersion(t *testing.T) {
	dsn := os.Getenv("PLATFORM_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("PLATFORM_TEST_DATABASE_URL not set")
	}
	got := serverVersion(context.Background(), dsn)
	if got == nil || got.Major < 10 || got.From != "server" || !strings.HasPrefix(got.Full, itoa(got.Major)+".") {
		t.Fatalf("server version %+v", got)
	}
	same := manifest(t, "  imageName: ghcr.io/cloudnative-pg/postgresql:"+itoa(got.Major)+".1\n")
	other := manifest(t, "  imageName: ghcr.io/cloudnative-pg/postgresql:"+itoa(got.Major+1)+".1\n")
	if r, _ := (Checker{}).Check(context.Background(), &plan.Plan{Source: plan.Endpoint{DSN: dsn}, Target: plan.Endpoint{Manifest: same}}); r.Status != Pass {
		t.Fatalf("same major: %+v", r)
	}
	if r, _ := (Checker{}).Check(context.Background(), &plan.Plan{Source: plan.Endpoint{DSN: dsn}, Target: plan.Endpoint{Manifest: other}}); r.Status != Fail {
		t.Fatalf("other major: %+v", r)
	}
	// A reachable target is read from its server, not from the manifest.
	if r, _ := (Checker{}).Check(context.Background(), &plan.Plan{Source: plan.Endpoint{DSN: dsn}, Target: plan.Endpoint{DSN: dsn, Manifest: other}}); r.Status != Pass || r.Target.From != "server" {
		t.Fatalf("reachable target: %+v", r)
	}
}

func itoa(n int) string { return strconv.Itoa(n) }
