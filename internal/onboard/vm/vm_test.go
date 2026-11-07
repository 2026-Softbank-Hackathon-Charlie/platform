package vm

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
)

func readFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The fixtures are inspect.sh's output against infra's test compose
// project (terraform/platform/aws/onboarding/test), for testdata/key.der.b64.
func chunkFixtures(t *testing.T) []string {
	var out []string
	for i := 0; ; i++ {
		b, err := os.ReadFile(filepath.Join("testdata", fmt.Sprintf("inspect-chunk%d.out", i)))
		if err != nil {
			break
		}
		out = append(out, string(b))
	}
	if len(out) < 2 {
		t.Fatal("no chunk fixtures")
	}
	return out
}

func TestAssembleRecordedOutput(t *testing.T) {
	k := goldenKey(t)
	one, err := Assemble([]string{readFixture(t, "inspect.out")})
	if err != nil {
		t.Fatal(err)
	}
	many, err := Assemble(chunkFixtures(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, doc := range []Document{one, many} {
		if len(doc.Containers) != 3 || len(doc.DB) != 1 || doc.DB[0].Facts == nil {
			t.Fatalf("document: %d containers, %d db", len(doc.Containers), len(doc.DB))
		}
		db := doc.DB[0].Facts
		if !strings.Contains(db.Version, "musl") || db.User != "app" || len(db.Indexes) == 0 {
			t.Fatalf("db facts: %+v", db)
		}
		v, err := OpenValues(k, doc.Secrets)
		if err != nil {
			t.Fatal(err)
		}
		if v["hcinspecttest-app-1"]["APP_SECRET"] != "s3cr3t-value-for-test" || v["hcinspecttest-db-1"]["POSTGRES_PASSWORD"] == "" {
			t.Fatal("values not decrypted")
		}
	}
	// Chunks out of order or missing fail.
	c := chunkFixtures(t)
	c[1], c[2] = c[2], c[1]
	if _, err := Assemble(c); err == nil {
		t.Fatal("swapped chunks assembled")
	}
	if _, err := Assemble(chunkFixtures(t)[:2]); err == nil {
		t.Fatal("missing chunks assembled")
	}
}

// fakeSSM answers each step with a canned output.
type fakeSSM struct {
	steps  map[string]string
	params map[string][]string
	reads  int
}

func (f *fakeSSM) SendCommand(_ context.Context, in *ssm.SendCommandInput, _ ...func(*ssm.Options)) (*ssm.SendCommandOutput, error) {
	f.params = in.Parameters
	return &ssm.SendCommandOutput{Command: &ssmtypes.Command{CommandId: aws.String("cmd-1")}}, nil
}

func (f *fakeSSM) GetCommandInvocation(_ context.Context, in *ssm.GetCommandInvocationInput, _ ...func(*ssm.Options)) (*ssm.GetCommandInvocationOutput, error) {
	f.reads++
	if f.reads == 1 {
		return nil, &ssmtypes.InvocationDoesNotExist{}
	}
	if f.reads == 2 {
		return &ssm.GetCommandInvocationOutput{Status: ssmtypes.CommandInvocationStatusInProgress}, nil
	}
	return &ssm.GetCommandInvocationOutput{Status: ssmtypes.CommandInvocationStatusSuccess,
		StandardOutputContent: aws.String(f.steps[aws.ToString(in.PluginName)])}, nil
}

func TestRunnerReadsChunkSteps(t *testing.T) {
	chunks := chunkFixtures(t)
	f := &fakeSSM{steps: map[string]string{InspectStep: chunks[0]}}
	for i := 1; i < len(chunks); i++ {
		f.steps[ChunkStep(i)] = chunks[i]
	}
	r := &Runner{SSM: f, Poll: time.Millisecond}
	// The recorded output is for the golden key, not Inspect's own: the
	// document assembles and the envelope then refuses this run's key.
	_, err := r.Inspect(context.Background(), "i-1")
	if err == nil || !strings.Contains(err.Error(), "does not decrypt") {
		t.Fatalf("err %v", err)
	}
	if f.params["maxChunk"][0] != "20000" || len(f.params["publicKey"][0]) < 300 {
		t.Fatalf("params %v", f.params)
	}
}

func TestConnectLines(t *testing.T) {
	out := `{"kind":"postgres_role_created","target":"postgres","at":"2026-11-07T10:00:00Z","new":true}
{"kind":"hba_line_added","target":"host replication hopcharlie_replicator 172.18.0.0/16 scram-sha-256","at":"2026-11-07T10:00:01Z","new":false}
{"result":"ok","replicationPassword":"hcenc1.x.y"}
`
	changes, result, err := lines(out)
	if err != nil || len(changes) != 2 || result["result"] != "ok" {
		t.Fatalf("%v %v %v", changes, result, err)
	}
	if changes[0].Kind != "postgres_role_created" || !changes[0].New || changes[1].New || changes[1].At.IsZero() {
		t.Fatalf("changes %+v", changes)
	}
	if _, _, err := lines(`{"kind":"rm_rf","target":"/"}`); err == nil {
		t.Fatal("unknown kind accepted")
	}
}
