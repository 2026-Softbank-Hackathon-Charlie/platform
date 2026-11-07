package discover

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	gh "github.com/2026-Softbank-Hackathon-Charlie/platform/internal/github"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/onboard/vm"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/secrets"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/onboard/facts"
)

const head = "0123456789abcdef0123456789abcdef01234567"

type fakeGitHub struct {
	installed bool
	files     map[string]string
}

func (f fakeGitHub) RepoDefaultBranch(context.Context, string) (string, error) {
	if !f.installed {
		return "", gh.ErrNotFound
	}
	return "main", nil
}
func (f fakeGitHub) ResolveRef(context.Context, string, string) (string, error) { return head, nil }
func (f fakeGitHub) RepoTree(context.Context, string, string) ([]gh.TreeEntry, bool, error) {
	var out []gh.TreeEntry
	for p := range f.files {
		out = append(out, gh.TreeEntry{Path: p, Type: "blob", Size: int64(len(f.files[p]))})
	}
	return out, false, nil
}
func (f fakeGitHub) ReadFile(_ context.Context, _, _, p string) ([]byte, error) {
	return []byte(f.files[p]), nil
}
func (f fakeGitHub) RepoWorkflows(context.Context, string) ([]gh.ActionsWorkflow, error) {
	return nil, &gh.APIError{Status: 403}
}
func (f fakeGitHub) WorkflowLastSuccess(context.Context, string, int64) (*gh.WorkflowRunInfo, error) {
	return nil, nil
}

type fakeWeb struct{}

func (fakeWeb) Resolve(_ context.Context, name string) ([]string, int, error) {
	if strings.HasPrefix(name, "hc-probe-") {
		return []string{"198.51.100.1"}, 300, nil // the wildcard: the gateway
	}
	return []string{"203.0.113.10"}, 60, nil
}
func (fakeWeb) Probe(_ context.Context, host, ip string, paths []string) (facts.HTTPS, error) {
	return facts.HTTPS{Responds: true, Status: 200, CertNames: []string{host}, HealthCandidates: []string{"/healthz", "/"}}, nil
}

type fakeAWS struct{ inst *facts.Instance }

func (f fakeAWS) FindInstance(context.Context, string) (*facts.Instance, error) { return f.inst, nil }

type fakeImages struct{}

func (fakeImages) Digest(_ context.Context, tag string) (string, error) {
	if tag == "17.11-standard-bookworm" {
		return "sha256:" + strings.Repeat("ab", 32), nil
	}
	return "", ErrNoImage
}

// recorded is inspect.sh's recorded run (internal/onboard/vm/testdata),
// opened with the run's key.
type recorded struct{ t *testing.T }

func (r recorded) Inspect(context.Context, string, string) (*vm.Inspection, error) {
	dir := filepath.Join("..", "vm", "testdata")
	b, err := os.ReadFile(filepath.Join(dir, "key.der.b64"))
	if err != nil {
		r.t.Fatal(err)
	}
	der, _ := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
	k, err := vm.ParseKey(der)
	if err != nil {
		r.t.Fatal(err)
	}
	out, err := os.ReadFile(filepath.Join(dir, "inspect.out"))
	if err != nil {
		r.t.Fatal(err)
	}
	doc, err := vm.Assemble([]string{string(out)})
	if err != nil {
		return nil, err
	}
	values, err := vm.OpenValues(k, doc.Secrets)
	if err != nil {
		return nil, err
	}
	doc.Secrets = ""
	return &vm.Inspection{Document: doc, Values: values}, nil
}

func newDiscoverer(t *testing.T, inst *facts.Instance, installed bool) *Discoverer {
	c, err := secrets.NewCipher(make([]byte, secrets.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	return &Discoverer{
		GitHub: fakeGitHub{installed: installed, files: map[string]string{
			"compose.yaml": "services: {}", "Dockerfile": "FROM alpine",
			".github/workflows/deploy.yml": "jobs:\n  d:\n    steps:\n      - uses: appleboy/ssh-action@v1\n        with: {host: '${{ secrets.VM_HOST }}', script: docker compose up -d}\n",
			".github/workflows/test.yml":   "jobs:\n  t:\n    runs-on: ubuntu-latest\n    steps: [{run: go test ./...}]\n",
			"Caddyfile":                    "moimroom-legacy.hopcharlie.dev {\n  reverse_proxy app:8080\n}\n",
		}},
		Web: fakeWeb{}, AWS: fakeAWS{inst}, Inspector: recorded{t}, Images: fakeImages{}, Cipher: c,
		Now: func() time.Time { return time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC) },
	}
}

func awsInstance() *facts.Instance {
	return &facts.Instance{Cloud: facts.CloudAWS, ID: "i-0abc", Region: "ap-northeast-2", PublicIP: "203.0.113.10", SSMManaged: true, Tags: map[string]string{}}
}

func codes(fs []facts.Finding) []string {
	var out []string
	for _, f := range fs {
		out = append(out, f.Code)
	}
	return out
}

func TestDiscoverRecordedVM(t *testing.T) {
	d := newDiscoverer(t, awsInstance(), true)
	res, err := d.Discover(context.Background(), Input{ID: "onb-0123456789ab", Repo: "org/moimroom-legacy", ServiceURL: "https://moimroom-legacy.hopcharlie.dev"})
	if err != nil {
		t.Fatal(err)
	}
	f := res.Facts
	if f.App != "moimroom-legacy" || f.GitHub.Head != head || !f.GitHub.Installed {
		t.Fatalf("github %+v", f.GitHub)
	}
	var deploys []string
	for _, w := range f.GitHub.Workflows {
		if w.DeploysToVM {
			deploys = append(deploys, w.Path)
		}
	}
	if !slices.Equal(deploys, []string{".github/workflows/deploy.yml"}) {
		t.Fatalf("deploy workflows %v", deploys)
	}
	if f.HTTPS.Record == nil || f.HTTPS.Record.Value != "203.0.113.10" || len(res.HumanActions) != 1 {
		t.Fatalf("dns record %+v, actions %+v", f.HTTPS.Record, res.HumanActions)
	}
	if f.VM == nil || f.VM.Project != "hcinspecttest" || len(f.VM.Containers) != 3 || f.VM.Networks[0].Subnet == "" {
		t.Fatalf("vm %+v", f.VM)
	}
	db := f.DB
	if db == nil || db.Libc != facts.LibcMusl || db.Service != "db" || db.FenceUser != "app" || db.Database != "app" || db.PostgresRole {
		t.Fatalf("db %+v", db)
	}
	tg := f.Target
	if tg.Env != "eks" || !strings.HasPrefix(tg.Image, ImageRepo+":17.11-standard-bookworm@sha256:") || tg.StorageSize != "1Gi" ||
		tg.PublicService != "app" || tg.Port != 8080 || tg.HealthCheck != "/healthz" {
		t.Fatalf("target %+v", tg)
	}
	if !slices.Contains(tg.Reindex, "public.users_email_idx") || slices.Contains(tg.Reindex, "public.users_email_c_idx") {
		t.Fatalf("reindex %v", tg.Reindex)
	}
	// The recorded database has CHECK (name > 'a') on a libc-collated column.
	if !slices.Contains(codes(f.Report.Blockers), facts.BlockCollationConstraint) {
		t.Fatalf("blockers %v", codes(f.Report.Blockers))
	}
	for _, c := range codes(f.Report.Blockers) {
		if c != facts.BlockCollationConstraint {
			t.Errorf("unexpected blocker %s", c)
		}
	}
	if w := codes(f.Report.Warnings); !slices.Contains(w, facts.WarnReindex) || !slices.Contains(w, facts.WarnAppSuperuser) {
		t.Fatalf("warnings %v", w)
	}
	if f.Storage == nil || f.Storage.Provider != facts.StorageAWS || f.Storage.Bucket != "moimroom-files" || f.Storage.SecretKeyEnv != "AWS_SECRET_ACCESS_KEY" {
		t.Fatalf("storage %+v", f.Storage)
	}
	envOf := func(svc, name string) *facts.EnvVar {
		for _, c := range f.VM.Containers {
			if c.Service == svc {
				for i := range c.Env {
					if c.Env[i].Name == name {
						return &c.Env[i]
					}
				}
			}
		}
		return nil
	}
	if e := envOf("app", "APP_SECRET"); e == nil || !e.Secret || e.Excluded != "" {
		t.Fatalf("APP_SECRET %+v", e)
	}
	if e := envOf("app", "DATABASE_URL"); e == nil || e.Excluded != facts.ExcludePlatform {
		t.Fatalf("DATABASE_URL %+v", e)
	}
	if e := envOf("db", "POSTGRES_PASSWORD"); e == nil || e.Excluded != facts.ExcludeDBContainer {
		t.Fatalf("POSTGRES_PASSWORD %+v", e)
	}
	if envOf("app", "PATH") != nil {
		t.Fatal("an image default is listed")
	}

	// No value is in the facts; the sealed values open with the key.
	b, _ := json.Marshal(f)
	for _, v := range []string{"s3cr3t-value-for-test", "aws-secret-for-test", "pw-db-0123456789"} {
		if strings.Contains(string(b), v) {
			t.Fatalf("facts carry the value %s", v)
		}
	}
	var o store.Onboarding
	o.ID = "onb-0123456789ab"
	if err := res.Apply(&o); err != nil {
		t.Fatal(err)
	}
	pw, err := d.Cipher.OpenOnboarding(o, secrets.OnboardingSuperuserPassword, o.SuperuserPasswordNonce, o.SuperuserPasswordCiphertext)
	if err != nil || pw != "pw-db-0123456789" {
		t.Fatalf("superuser password: %v", err)
	}
	all, err := d.Cipher.OpenOnboarding(o, secrets.OnboardingSecrets, o.SecretsNonce, o.SecretsCiphertext)
	if err != nil || !strings.Contains(all, `"APP_SECRET":"s3cr3t-value-for-test"`) {
		t.Fatalf("secrets: %v", err)
	}
}

func TestDiscoverStopsWhereItCannotLook(t *testing.T) {
	ctx := context.Background()
	in := Input{ID: "onb-0123456789ab", Repo: "org/x", ServiceURL: "https://x.hopcharlie.dev"}
	cases := map[string]struct {
		inst      *facts.Instance
		installed bool
		url       string
		want      []string
	}{
		"not installed": {awsInstance(), false, "", nil},
		"no instance":   {nil, true, "", []string{facts.BlockInstanceNotFound}},
		"gcp":           {&facts.Instance{Cloud: facts.CloudGCP, ID: "vm-1", Zone: "asia-northeast3-a"}, true, "", []string{facts.BlockGCPInspection}},
		"asg and no ssm": {&facts.Instance{Cloud: facts.CloudAWS, ID: "i-1", AutoScalingGroup: "g", Tags: map[string]string{}}, true, "",
			[]string{facts.BlockAutoScalingGroup, facts.BlockNotSSMManaged}},
		"domain": {nil, true, "https://example.com", []string{facts.BlockDomainOutOfScope, facts.BlockInstanceNotFound}},
	}
	for name, c := range cases {
		d := newDiscoverer(t, c.inst, c.installed)
		d.Inspector = nil // must not be reached
		if c.inst != nil && c.inst.Cloud == facts.CloudAWS && c.inst.SSMManaged {
			d.Inspector = recorded{t}
		}
		i := in
		if c.url != "" {
			i.ServiceURL = c.url
		}
		res, err := d.Discover(ctx, i)
		if name == "not installed" {
			// The VM is still inspected; the onboarding waits for the App.
			if err != nil || !res.Waiting() || res.HumanActions[0].Kind != "install_github_app" {
				t.Fatalf("%s: %v %+v", name, err, res.HumanActions)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := codes(res.Facts.Report.Blockers); !slices.Equal(got, c.want) {
			t.Errorf("%s: blockers %v, want %v", name, got, c.want)
		}
		if res.Secrets != nil {
			t.Errorf("%s: sealed values without an inspection", name)
		}
	}
}

func TestDeploysToVM(t *testing.T) {
	for wf, want := range map[string]bool{
		"steps: [{run: ssh deploy@host 'docker compose up -d'}]":       true,
		"steps: [{uses: appleboy/scp-action@v0}]":                      true,
		"env: {H: ${{ secrets.PROD_HOST }}}":                           true,
		"runs-on: [self-hosted]\nsteps: [{run: docker compose up -d}]": true,
		"runs-on: ubuntu-latest\nsteps: [{run: docker compose build}]": false,
		"steps: [{uses: actions/checkout@v4}, {run: go test ./...}]":   false,
	} {
		if DeploysToVM(wf) != want {
			t.Errorf("%q: want %v", wf, want)
		}
	}
}

func TestImageRules(t *testing.T) {
	ctx := context.Background()
	mk := func(version string, num int, collate string) *facts.Facts {
		f := &facts.Facts{DB: &facts.DB{ServerVersionNum: num, Version: version, Database: "app",
			Databases:  []facts.Database{{Name: "app", LocProvider: "c", Collate: collate, SizeBytes: 3 << 30}},
			Indexes:    []facts.Index{{Name: "public.i", Collation: collate}},
			Superusers: []string{"app"}}}
		f.DB.Libc = facts.LibcGlibc
		if strings.Contains(version, "musl") {
			f.DB.Libc = facts.LibcMusl
		} else if m := pgdg.FindStringSubmatch(version); m != nil {
			f.DB.Debian = map[string]string{"11": "bullseye", "12": "bookworm", "13": "trixie"}[m[1]]
		}
		return f
	}
	all := imagesAll{}
	for name, c := range map[string]struct {
		f       *facts.Facts
		tag     string
		reindex bool
		warn    string
	}{
		"bookworm glibc": {mk("PostgreSQL 17.6 (Debian 17.6-1.pgdg120+1) on x86_64-pc-linux-gnu", 170006, "en_US.utf8"), "17.6-standard-bookworm", false, ""},
		"bullseye glibc": {mk("PostgreSQL 16.4 (Debian 16.4-1.pgdg110+1) on x86_64-pc-linux-gnu", 160004, "en_US.utf8"), "16.4", false, ""},
		"musl C":         {mk("PostgreSQL 17.11 on x86_64-pc-linux-musl", 170011, "C"), "17.11-standard-bookworm", false, ""},
		"musl en_US":     {mk("PostgreSQL 17.11 on x86_64-pc-linux-musl", 170011, "en_US.utf8"), "17.11-standard-bookworm", true, facts.WarnReindex},
		"unknown glibc":  {mk("PostgreSQL 17.2 on x86_64-pc-linux-gnu", 170002, "en_US.UTF-8"), "17.2-standard-bookworm", true, facts.WarnGlibcRelease},
	} {
		c.f.Report = facts.Report{}
		if err := chooseTarget(ctx, all, c.f, "eks", report{&c.f.Report}); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(c.f.Target.Image, ":"+c.tag+"@sha256:") {
			t.Errorf("%s: image %s, want tag %s", name, c.f.Target.Image, c.tag)
		}
		if (len(c.f.Target.Reindex) > 0) != c.reindex {
			t.Errorf("%s: reindex %v", name, c.f.Target.Reindex)
		}
		if c.warn != "" && !slices.Contains(codes(c.f.Report.Warnings), c.warn) {
			t.Errorf("%s: warnings %v", name, codes(c.f.Report.Warnings))
		}
		if c.f.Target.StorageSize != "6Gi" {
			t.Errorf("%s: storage %s", name, c.f.Target.StorageSize)
		}
	}
	// No image of the major: blocked.
	f := mk("PostgreSQL 17.11 on x86_64-pc-linux-musl", 170011, "C")
	if err := chooseTarget(ctx, fakeImages{}, f, "eks", report{&f.Report}); err != nil || f.Target.Image == "" {
		t.Fatalf("exact tag: %v", err)
	}
	f = mk("PostgreSQL 19.1 on x86_64-pc-linux-musl", 190001, "C")
	_ = chooseTarget(ctx, fakeImages{}, f, "eks", report{&f.Report})
	if !slices.Contains(codes(f.Report.Blockers), facts.BlockNoImage) {
		t.Fatalf("blockers %v", codes(f.Report.Blockers))
	}
}

type imagesAll struct{}

func (imagesAll) Digest(context.Context, string) (string, error) {
	return "sha256:" + strings.Repeat("cd", 32), nil
}

func TestReverseProxyVolumesAreNotData(t *testing.T) {
	doc := vm.Document{FreeDiskBytes: 10 << 30, Containers: []vm.Container{
		{ID: "1", Name: "/p-caddy-1", Project: "p", Service: "caddy", Image: "caddy:2", State: "running",
			Ports:  map[string][]vm.PortBinding{"443/tcp": {{HostPort: "443"}}, "80/tcp": {{HostPort: "80"}}},
			Mounts: []vm.Mount{{Type: "volume", Name: "p_caddy_data", Destination: "/data"}}},
		{ID: "2", Name: "/p-app-1", Project: "p", Service: "app", Image: "app:1", State: "running",
			Exposed: map[string]struct{}{"3000/tcp": {}},
			Mounts:  []vm.Mount{{Type: "volume", Name: "p_uploads", Destination: "/uploads"}}},
	}, Volumes: []vm.Volume{{Name: "p_caddy_data", SizeBytes: 1 << 20}, {Name: "p_uploads", SizeBytes: 5 << 20}}}
	f := &facts.Facts{}
	fromInspection(f, doc, vm.Values{}, nil, report{&f.Report})
	roles := map[string]string{}
	for _, v := range f.VM.Volumes {
		roles[v.Name] = v.Role
	}
	if roles["p_caddy_data"] != facts.VolumeProxy || roles["p_uploads"] != facts.VolumeData {
		t.Fatalf("roles %v", roles)
	}
	if got := codes(f.Report.Blockers); !slices.Equal(got, []string{facts.BlockDBNotContainer, facts.BlockDataVolume}) {
		t.Fatalf("blockers %v", got)
	}
	if f.Target.PublicService != "app" || f.Target.Port != 3000 {
		t.Fatalf("target %+v", f.Target)
	}
}

func TestInVMStorageBlocks(t *testing.T) {
	doc := vm.Document{FreeDiskBytes: 1 << 30, Containers: []vm.Container{
		{ID: "1", Name: "/m-app-1", Project: "m", Service: "app", Image: "app", State: "running", Env: []vm.EnvName{{Name: "S3_ENDPOINT"}, {Name: "S3_BUCKET"}}},
		{ID: "2", Name: "/m-minio-1", Project: "m", Service: "minio", Image: "minio/minio:latest", State: "running",
			Mounts: []vm.Mount{{Type: "volume", Name: "m_minio", Destination: "/data"}}},
	}, Volumes: []vm.Volume{{Name: "m_minio", SizeBytes: 9 << 20}}}
	vals := vm.Values{"m-app-1": {"S3_ENDPOINT": "http://minio:9000", "S3_BUCKET": "files"}}
	f := &facts.Facts{}
	fromInspection(f, doc, vals, nil, report{&f.Report})
	got := codes(f.Report.Blockers)
	for _, want := range []string{facts.BlockDiskTooSmall, facts.BlockStorageOutOfScope} {
		if !slices.Contains(got, want) {
			t.Errorf("blockers %v lack %s", got, want)
		}
	}
	if f.Storage.Provider != facts.StorageInVM || f.Storage.Endpoint != "http://minio:9000" {
		t.Fatalf("storage %+v", f.Storage)
	}
	for _, c := range f.VM.Containers {
		for _, e := range c.Env {
			if e.Name == "S3_ENDPOINT" && e.Excluded != facts.ExcludePlatform {
				t.Errorf("S3_ENDPOINT %+v", e)
			}
		}
	}
}
