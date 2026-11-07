package discover

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/onboard/vm"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/onboard/facts"
)

func ptr[T any](v T) *T { return &v }

// goodDB is a database every check passes.
func goodDB() vm.DBFacts {
	return vm.DBFacts{ServerVersionNum: 170011, Version: "PostgreSQL 17.11 (Debian 17.11-1.pgdg120+1) on x86_64-pc-linux-gnu",
		User: "app", Database: "app", Owner: "app", WALLevel: "replica", MaxWALSenders: 10, PasswordEncryption: "scram-sha-256",
		WALKeepSize: ptr("0"), WALKeepSizeSource: ptr("default"), Superusers: []string{"app"}, DataDirectory: "/var/lib/postgresql/data",
		Databases: []vm.DBDatabase{{Name: "app", Owner: "app", LocProvider: "c", Collate: "en_US.utf8", SizeBytes: ptr(int64(1 << 20))}}}
}

// vmWith is a project of a database and an app around one DB result.
func vmWith(res vm.DBResult) (vm.Document, vm.Values) {
	doc := vm.Document{FreeDiskBytes: 10 << 30, Containers: []vm.Container{
		{ID: "1", Name: "/p-db-1", Project: "p", Service: "db", Image: "postgres:17.11", State: "running"},
		{ID: "2", Name: "/p-app-1", Project: "p", Service: "app", Image: "app", State: "running", Exposed: map[string]struct{}{"8080/tcp": {}}},
	}}
	res.Container = "p-db-1"
	doc.DB = []vm.DBResult{res}
	return doc, vm.Values{"p-db-1": {"POSTGRES_PASSWORD": "pw"}}
}

func blockersOf(t *testing.T, res vm.DBResult, values func(vm.Values)) []string {
	t.Helper()
	doc, vals := vmWith(res)
	if values != nil {
		values(vals)
	}
	f := &facts.Facts{}
	fromInspection(f, doc, vals, nil, report{&f.Report})
	if err := chooseTarget(context.Background(), imagesAll{}, f, "eks", report{&f.Report}); err != nil {
		t.Fatal(err)
	}
	return codes(f.Report.Blockers)
}

func TestDatabaseBlockers(t *testing.T) {
	cases := map[string]struct {
		edit   func(*vm.DBResult)
		values func(vm.Values)
		want   []string
	}{
		"good":               {nil, nil, nil},
		"psql error":         {func(r *vm.DBResult) { r.Facts, r.Error = nil, "psql exited 2: FATAL:\n role \"x\" does not exist" }, nil, []string{facts.BlockDBUnreadable}},
		"auto.conf":          {func(r *vm.DBResult) { r.Facts.AutoConf = []string{"work_mem"} }, nil, []string{facts.BlockAutoConf}},
		"role read only":     {func(r *vm.DBResult) { r.Facts.ReadOnlyRoleSettings = []string{"app@"} }, nil, []string{facts.BlockRoleReadOnly}},
		"command line":       {func(r *vm.DBResult) { r.Facts.WALKeepSizeSource = ptr("command line") }, nil, []string{facts.BlockWALKeepSizeCmdLine}},
		"alter system off":   {func(r *vm.DBResult) { r.Facts.AllowAlterSystem = ptr("off") }, nil, []string{facts.BlockAlterSystemOff}},
		"wal_level minimal":  {func(r *vm.DBResult) { r.Facts.WALLevel = "minimal" }, nil, []string{facts.BlockWALLevelMinimal}},
		"wal senders in use": {func(r *vm.DBResult) { r.Facts.MaxWALSenders, r.Facts.WALSendersUsed = 4, 2 }, nil, []string{facts.BlockNoWALSenders}},
		"md5":                {func(r *vm.DBResult) { r.Facts.PasswordEncryption = "md5" }, nil, []string{facts.BlockPasswordMD5}},
		"no password":        {nil, func(v vm.Values) { delete(v["p-db-1"], "POSTGRES_PASSWORD") }, []string{facts.BlockNoSuperuser}},
		"not superuser":      {func(r *vm.DBResult) { r.Facts.Superusers = []string{"admin"} }, nil, []string{facts.BlockNoSuperuser}},
		"pending restart":    {func(r *vm.DBResult) { r.Facts.PendingConfig = []string{"shared_buffers"} }, nil, []string{facts.BlockPendingConfig}},
		"read-only config":   {func(r *vm.DBResult) { r.ConfigWritable = false }, nil, []string{facts.BlockConfigNotWritable}},
		"postgres 12":        {func(r *vm.DBResult) { r.Facts.ServerVersionNum = 120020 }, nil, []string{facts.BlockNoImage}},
		"musl icu index": {func(r *vm.DBResult) {
			r.Facts.Version = "PostgreSQL 17.11 on x86_64-pc-linux-musl"
			r.Facts.Indexes = []vm.DBIndex{{Name: "public.i", Provider: "i", Collation: "und-x-icu"}}
		}, nil, []string{facts.BlockICUVersion}},
		"musl other database": {func(r *vm.DBResult) {
			r.Facts.Version = "PostgreSQL 17.11 on x86_64-pc-linux-musl"
			r.Others = []vm.DBFacts{{Database: "reports", Indexes: []vm.DBIndex{{Name: "public.r_idx", Provider: "c", Collation: "en_US.utf8"}}}}
			r.OthersError = []string{"audit: psql exited 2: x"}
		}, nil, []string{facts.BlockOtherDBCollation, facts.BlockOtherDBCollation}},
		"glibc other database is fine": {func(r *vm.DBResult) {
			r.Others = []vm.DBFacts{{Database: "reports", Indexes: []vm.DBIndex{{Name: "public.r_idx", Provider: "c", Collation: "en_US.utf8"}}}}
		}, nil, nil},
		"musl check constraint": {func(r *vm.DBResult) {
			r.Facts.Version = "PostgreSQL 17.11 on x86_64-pc-linux-musl"
			r.Facts.CollationDependent = []string{"public.t.c_check"}
		}, nil, []string{facts.BlockCollationConstraint}},
	}
	for name, c := range cases {
		res := vm.DBResult{Facts: ptr(goodDB()), ConfigWritable: true}
		if c.edit != nil {
			c.edit(&res)
		}
		got := blockersOf(t, res, c.values)
		if !slices.Equal(got, c.want) {
			t.Errorf("%s: blockers %v, want %v", name, got, c.want)
		}
	}
}

func TestDBUnreadableMessageIsClean(t *testing.T) {
	doc, vals := vmWith(vm.DBResult{Error: "psql exited 2: line1\nline2\x00"})
	f := &facts.Facts{}
	fromInspection(f, doc, vals, nil, report{&f.Report})
	m := f.Report.Blockers[0].Message
	if strings.ContainsAny(m, "\n\x00") || !strings.Contains(m, "line1 line2") {
		t.Fatalf("message %q", m)
	}
}

func TestInspectExitCodesAreFindings(t *testing.T) {
	for code, want := range map[int32]string{vm.InspectExitImagePull: facts.BlockVMImagePull, vm.InspectExitNoDocker: facts.BlockNoCompose} {
		d := newDiscoverer(t, awsInstance(), true)
		d.Inspector = failingInspector{code}
		res, err := d.Discover(context.Background(), Input{ID: "onb-0123456789ab", Repo: "org/x", ServiceURL: "https://x.hopcharlie.dev"})
		if err != nil {
			t.Fatalf("exit %d: %v", code, err)
		}
		if got := codes(res.Facts.Report.Blockers); !slices.Equal(got, []string{want}) {
			t.Errorf("exit %d: blockers %v", code, got)
		}
	}
	d := newDiscoverer(t, awsInstance(), true)
	d.Inspector = failingInspector{5}
	if _, err := d.Discover(context.Background(), Input{ID: "onb-0123456789ab", Repo: "org/x", ServiceURL: "https://x.hopcharlie.dev"}); err == nil {
		t.Fatal("exit 5 is not an error")
	}
}

type failingInspector struct{ code int32 }

func (f failingInspector) Inspect(context.Context, string, string) (*vm.Inspection, error) {
	return nil, &vm.StepError{Document: "HopCharlie-Inspect", ExitCode: f.code, Status: "Failed", Message: "m"}
}

func TestIndividualRecord(t *testing.T) {
	gw := DNSAnswer{Addrs: []string{"198.51.100.1", "198.51.100.2"}, CNAMEs: []string{"gw.elb.amazonaws.com"}}
	for name, c := range map[string]struct {
		host, wild DNSAnswer
		want       bool
	}{
		"A record at the VM":         {DNSAnswer{Addrs: []string{"203.0.113.10"}}, gw, true},
		"wildcard, rotated LB addrs": {DNSAnswer{Addrs: []string{"198.51.100.3"}, CNAMEs: []string{"gw.elb.amazonaws.com"}}, gw, false},
		"CNAME elsewhere":            {DNSAnswer{Addrs: []string{"192.0.2.1"}, CNAMEs: []string{"vm.example.net"}}, gw, true},
		"plain A wildcard, overlap":  {DNSAnswer{Addrs: []string{"198.51.100.1"}}, DNSAnswer{Addrs: []string{"198.51.100.1", "198.51.100.9"}}, false},
	} {
		if individual(c.host, c.wild) != c.want {
			t.Errorf("%s: want %v", name, c.want)
		}
	}
}

func TestK8sName(t *testing.T) {
	for in, want := range map[string]string{"db": "db", "Main_DB": "main-db", "pg.primary": "pg-primary", "_x_": "x"} {
		if got := K8sName(in); got != want {
			t.Errorf("%q: %q", in, got)
		}
	}
}

func TestDeployWorkflowsWider(t *testing.T) {
	for wf, want := range map[string]bool{
		"env: {DOCKER_HOST: 'ssh://deploy@vm'}\nsteps: [{run: docker compose up -d}]": true,
		"steps: [{run: docker context use prod}]":                                     true,
		"steps: [{run: aws ssm send-command --document-name AWS-RunShellScript}]":     true,
		"steps: [{run: gcloud compute ssh vm --command up}]":                          true,
		"steps: [{uses: easingthemes/ssh-deploy@v5}]":                                 true,
	} {
		if DeploysToVM(wf) != want {
			t.Errorf("%q: want %v", wf, want)
		}
	}
	wfs := []facts.Workflow{{Path: ".github/workflows/release.yml"}, {Path: ".github/workflows/deploy.yml", DeploysToVM: true}, {Path: ".github/workflows/ci.yml"}}
	markCallers(wfs, map[string]string{
		".github/workflows/release.yml": "jobs:\n  d:\n    uses: ./.github/workflows/deploy.yml\n",
		".github/workflows/ci.yml":      "jobs:\n  t:\n    runs-on: ubuntu-latest\n",
	})
	if !wfs[0].DeploysToVM || wfs[2].DeploysToVM {
		t.Fatalf("callers %+v", wfs)
	}
}

func TestStorageVariableChoice(t *testing.T) {
	doc := vm.Document{FreeDiskBytes: 10 << 30, Containers: []vm.Container{
		{ID: "2", Name: "/p-app-1", Project: "p", Service: "app", Image: "app", State: "running"}}}
	vals := vm.Values{"p-app-1": {"BACKUP_BUCKET_URL": "s3://not a name", "S3_BUCKET": "files-prod",
		"AWS_ENDPOINT_URL": "https://s3.ap-northeast-2.amazonaws.com", "LEGACY_ENDPOINT": "https://other.example.com",
		"AWS_ACCESS_KEY_ID": "k", "AWS_SECRET_ACCESS_KEY": "s", "AWS_REGION": "ap-northeast-2"}}
	f := &facts.Facts{}
	fromInspection(f, doc, vals, nil, report{&f.Report})
	s := f.Storage
	if s == nil || s.BucketEnv != "S3_BUCKET" || s.EndpointEnv != "AWS_ENDPOINT_URL" || s.Provider != facts.StorageAWS || s.Region != "ap-northeast-2" {
		t.Fatalf("storage %+v", s)
	}
}
