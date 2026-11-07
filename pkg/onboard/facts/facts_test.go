package facts

import (
	"encoding/json"
	"reflect"
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
