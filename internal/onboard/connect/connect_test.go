package connect

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/onboard/vm"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/secrets"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/onboard/facts"
)

type fakeEC2 struct{ created, deleted []string }

func (f *fakeEC2) CreateTags(_ context.Context, in *ec2.CreateTagsInput, _ ...func(*ec2.Options)) (*ec2.CreateTagsOutput, error) {
	f.created = append(f.created, in.Resources[0]+":"+aws.ToString(in.Tags[0].Key)+"="+aws.ToString(in.Tags[0].Value))
	return &ec2.CreateTagsOutput{}, nil
}
func (f *fakeEC2) DeleteTags(_ context.Context, in *ec2.DeleteTagsInput, _ ...func(*ec2.Options)) (*ec2.DeleteTagsOutput, error) {
	f.deleted = append(f.deleted, in.Resources[0])
	return &ec2.DeleteTagsOutput{}, nil
}

type fakeVM struct {
	got  vm.ConnectParams
	fail bool
}

func (f *fakeVM) Connect(_ context.Context, _ string, p vm.ConnectParams) (vm.Connected, error) {
	f.got = p
	out := vm.Connected{Changes: []vm.Change{{OnboardingLegacyChange: apiv1.OnboardingLegacyChange{Kind: apiv1.OnboardingLegacyChangeKindReplicationRoleCreated, Target: "hopcharlie_replicator"}, New: true}}}
	if f.fail {
		return out, errors.New("HopCharlie-Connect exited 6")
	}
	out.ReplicationPassword = "pw-replicator"
	out.PasswordRotated = true
	// A rerun's report of the same change is kept once.
	out.Changes = append(out.Changes, out.Changes[0])
	return out, nil
}
func (f *fakeVM) Disconnect(context.Context, string, string, string) ([]apiv1.OnboardingLegacyChange, error) {
	now := time.Now()
	return []apiv1.OnboardingLegacyChange{{Kind: apiv1.OnboardingLegacyChangeKindHbaLineAdded, Target: "x", UndoneAt: &now}}, nil
}

type fakeTailnet struct{ deleted string }

func (f *fakeTailnet) AuthKey(context.Context, string) (string, error) { return "tskey-auth-abc", nil }
func (f *fakeTailnet) DeleteDevice(_ context.Context, h string) (bool, error) {
	f.deleted = h
	return true, nil
}

func testFacts() facts.Facts {
	return facts.Facts{App: "moimroom-legacy",
		Instance: &facts.Instance{Cloud: facts.CloudAWS, ID: "i-0abc", Region: "ap-northeast-2"},
		VM: &facts.VM{Project: "moimroom", Networks: []facts.Network{{Name: "moimroom_back", Subnet: "172.19.0.0/16"}, {Name: "moimroom_default", Subnet: "172.18.0.0/16"}},
			Containers: []facts.Container{{Name: "moimroom-db-1", Networks: []string{"moimroom_back", "moimroom_default"}}}},
		DB: &facts.DB{Container: "moimroom-db-1", Service: "db"}}
}

func TestConnect(t *testing.T) {
	c, _ := secrets.NewCipher(make([]byte, secrets.KeySize))
	e, v, ts := &fakeEC2{}, &fakeVM{}, &fakeTailnet{}
	k := &Connector{EC2: func(string) Tagger { return e }, VM: func(string) VM { return v }, Tailscale: ts, Cipher: c}
	res, err := k.Connect(context.Background(), Input{ID: "onb-0123456789ab", Facts: testFacts()})
	if err != nil {
		t.Fatal(err)
	}
	if e.created[0] != "i-0abc:hopcharlie-onboarding=onb-0123456789ab" {
		t.Fatalf("tags %v", e.created)
	}
	want := vm.ConnectParams{DBContainer: "moimroom-db-1", Network: "moimroom_default", Subnet: "172.18.0.0/16",
		TailscaleAuthKey: "tskey-auth-abc", Hostname: "hop-db-legacy-moimroom-legacy", App: "moimroom-legacy"}
	if v.got != want {
		t.Fatalf("params %+v", v.got)
	}
	if len(res.Changes) != 2 || res.Changes[0].Kind != apiv1.OnboardingLegacyChangeKindInstanceTagged || !res.Changes[1].New || !res.PasswordRotated {
		t.Fatalf("changes %+v", res.Changes)
	}
	o := store.Onboarding{ID: "onb-0123456789ab", KeyVersion: res.KeyVersion, KeyCheck: res.KeyCheck}
	pw, err := c.OpenOnboarding(o, secrets.OnboardingReplicationPassword, res.ReplicationPassword.Nonce, res.ReplicationPassword.Ciphertext)
	if err != nil || pw != "pw-replicator" {
		t.Fatalf("password: %v", err)
	}

	// A failed run keeps what it did, for DELETE.
	v.fail = true
	res, err = k.Connect(context.Background(), Input{ID: "onb-0123456789ab", Facts: testFacts()})
	if err == nil || len(res.Changes) != 2 || res.ReplicationPassword != nil {
		t.Fatalf("failed run: %v %+v", err, res)
	}

	out, err := k.Disconnect(context.Background(), Input{ID: "onb-0123456789ab", Facts: testFacts()})
	if err != nil || len(out) != 3 || ts.deleted != "hop-db-legacy-moimroom-legacy" || e.deleted[0] != "i-0abc" {
		t.Fatalf("disconnect %v %+v", err, out)
	}
	for _, ch := range out {
		if ch.UndoneAt == nil {
			t.Fatalf("not undone: %+v", ch)
		}
	}
}

func TestTailscaleAPI(t *testing.T) {
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v2/tailnet/-/keys":
			b := make([]byte, 1024)
			n, _ := r.Body.Read(b)
			body = string(b[:n])
			_, _ = w.Write([]byte(`{"key":"tskey-auth-k1"}`))
		case r.URL.Path == "/api/v2/tailnet/-/devices":
			_, _ = w.Write([]byte(`{"devices":[{"id":"1","hostname":"hop-db-legacy-a","tags":["tag:hop-legacy"]},{"id":"2","hostname":"hop-db-legacy-a","tags":[]}]}`))
		case r.Method == http.MethodDelete && r.URL.Path == "/api/v2/device/1":
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	api := &TailscaleAPI{HTTP: srv.Client(), BaseURL: srv.URL}
	k, err := api.AuthKey(context.Background(), "d")
	if err != nil || k != "tskey-auth-k1" {
		t.Fatal(k, err)
	}
	for _, w := range []string{`"reusable":false`, `"ephemeral":false`, `"preauthorized":true`, `"tags":["tag:hop-legacy"]`, `"expirySeconds":3600`} {
		if !strings.Contains(body, w) {
			t.Errorf("key request lacks %s: %s", w, body)
		}
	}
	if ok, err := api.DeleteDevice(context.Background(), "hop-db-legacy-a"); err != nil || !ok {
		t.Fatal(ok, err)
	}
}
