package facts

import (
	"encoding/json"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestFactsRoundTrip(t *testing.T) {
	in := Facts{
		Version: Version, At: time.Date(2026, 11, 7, 9, 0, 0, 0, time.UTC),
		Repo: "2026-Softbank-Hackathon-Charlie/moimroom-legacy", ServiceURL: "https://moimroom-legacy.hopcharlie.dev", App: "moimroom-legacy",
		GitHub: &GitHub{Installed: true, DefaultBranch: "main", Head: "0a1b", ComposeFiles: []string{"compose.yaml"},
			Dockerfiles: []string{"Dockerfile"}, Workflows: []Workflow{{ID: 1, Path: ".github/workflows/deploy.yml", Name: "deploy", State: "active", DeploysToVM: true}}},
		VM: &VM{Project: "moimroom", Networks: []Network{{Name: "moimroom_default", Driver: "bridge", Subnet: "172.18.0.0/16"}},
			Containers: []Container{{Name: "moimroom-db-1", Service: "db", Image: "postgres:17.11-alpine",
				Env: []EnvVar{{Name: "POSTGRES_PASSWORD", Secret: true}, {Name: "DATABASE_URL", Excluded: ExcludePlatform}}}}},
		DB:     &DB{Container: "moimroom-db-1", Service: "db", Port: 5432, ServerVersionNum: 170011, Libc: LibcMusl, Database: "moimroom", Owner: "moimroom"},
		Target: &Target{Env: "eks", Image: "ghcr.io/cloudnative-pg/postgresql:17.11-standard-bookworm@sha256:0f", StorageSize: "2Gi", Reindex: []string{"public.users_email_idx"}},
		Report: Report{Blockers: []Finding{}, Warnings: []Finding{{Code: WarnReindex, Message: "1 index is rebuilt"}}},
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out Facts
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Fatalf("round trip:\n%+v\n%+v", in, out)
	}
	if strings.Contains(string(b), `"value"`) {
		t.Fatalf("an environment variable carries a value: %s", b)
	}
}

// valueLike are field names that could carry a value from the VM rather
// than a name: a value, password, token, key, DSN, URI or connection
// string, a command line or arguments.
var valueLike = regexp.MustCompile(`(?i)value|passw|secret|token|key|dsn|uri|url|conn|command|args|content|data`)

// reviewed are the string fields matching valueLike that hold no secret,
// each looked at by hand. A new string field that matches fails the test
// until someone decides it is safe and adds it here.
var reviewed = map[string]bool{
	"Facts.ServiceURL":      true, // the address the onboarding was started with
	"DNSRecord.Value":       true, // a public IP address
	"Storage.AccessKeyEnv":  true, // the variable's name
	"Storage.SecretKeyEnv":  true, // the variable's name
	"DB.EnvDatabase":        true, // POSTGRES_DB, a database name
	"DB.Database":           true, // a database name
	"DB.PasswordEncryption": true, // md5 or scram-sha-256
	"DB.DataDirectory":      true, // a path
}

// No field of Facts carries a secret value (ADR 0042: names, containers
// and secret flags only): every string field is checked by name.
func TestFactsHoldNoValues(t *testing.T) {
	seen := map[reflect.Type]bool{}
	var walk func(reflect.Type)
	walk = func(ty reflect.Type) {
		for ty.Kind() == reflect.Pointer || ty.Kind() == reflect.Slice || ty.Kind() == reflect.Map {
			ty = ty.Elem()
		}
		if ty.Kind() != reflect.Struct || seen[ty] || ty == reflect.TypeOf(time.Time{}) {
			return
		}
		seen[ty] = true
		for i := range ty.NumField() {
			f := ty.Field(i)
			k := f.Type
			for k.Kind() == reflect.Pointer || k.Kind() == reflect.Slice || k.Kind() == reflect.Map {
				k = k.Elem()
			}
			name := ty.Name() + "." + f.Name
			if k.Kind() == reflect.String && valueLike.MatchString(f.Name) && !reviewed[name] {
				t.Errorf("%s may carry a value from the VM; Facts hold names only (add it to reviewed if it is safe)", name)
			}
			if f.Type.Kind() == reflect.Map && f.Type.Key().Kind() == reflect.String && name != "Instance.Tags" {
				t.Errorf("%s is a map of strings; only Instance.Tags is reviewed", name)
			}
			walk(f.Type)
		}
	}
	walk(reflect.TypeOf(Facts{}))
}

func TestEndpointDropsCredentials(t *testing.T) {
	for raw, want := range map[string]string{
		"https://AKIA:secret@s3.ap-northeast-2.amazonaws.com/bucket?x=1": "https://s3.ap-northeast-2.amazonaws.com",
		"http://minio:9000":                     "http://minio:9000",
		"https://storage.googleapis.com/b#frag": "https://storage.googleapis.com",
	} {
		got, err := Endpoint(raw)
		if err != nil || got != want {
			t.Errorf("Endpoint(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
	for _, bad := range []string{"s3.amazonaws.com", "ftp://x", "https://user:pw@"} {
		if _, err := Endpoint(bad); err == nil || strings.Contains(err.Error(), "pw") {
			t.Errorf("Endpoint(%q) = %v, want an error without the URL", bad, err)
		}
	}
}
