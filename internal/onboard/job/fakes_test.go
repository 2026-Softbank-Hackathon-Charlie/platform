package job

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/airun"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/apps"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/gateway"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/github"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/plans"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/runs"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/secrets"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store/storetest"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/onboard/facts"
)

const headSHA = "9fceb02d0ae598e95dc970b74767f19372d61af8"

// world is every port faked, recording what was done in order.
type world struct {
	t      *testing.T
	mu     sync.Mutex
	log    []string
	st     store.Store
	cipher *secrets.Cipher
	now    time.Time

	discoverErr error
	blockers    []facts.Finding
	human       bool

	hopaiStatus string // running, succeeded, failed
	hopaiExit   int
	jobs        int

	deployStatus map[string]string // deployment id -> status
	deps         int
	testFails    int // test deployments that fail before one passes
	startSeconds float64

	connectErr    error
	disconnectErr error

	streaming bool
	lag       time.Duration
	dns       bool

	noPostgres bool // the registered platform.yaml has no postgres resource
	rotated    bool // connect says the replication password changed
	lostRun    bool // no run of the plan was found after a restart
	foreignApp bool // an app of the onboarding's name is not its own

	firstDeployFails int
	sourceFenced     bool // the source still refuses writes after a run ended

	runStatus  apiv1.RunStatus
	reserveErr error
}

func newWorld(t *testing.T) *world {
	c, err := secrets.NewCipher(bytes.Repeat([]byte{7}, secrets.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	return &world{t: t, st: storetest.NewMemory(), cipher: c, now: time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC),
		hopaiStatus: "running", deployStatus: map[string]string{}, streaming: true, dns: true, runStatus: apiv1.RunStatusRunning}
}

func (w *world) did(s string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.log = append(w.log, s)
}

func (w *world) done() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.log...)
}

func (w *world) runner() *Runner {
	return New(Config{Store: w.st, Secrets: w.cipher, Discover: w, Connect: w, GitHub: w, AI: w, Apps: w, Kube: w,
		Files: w, DNS: w, Gateway: w, Routes: w, Cutover: w, SourceDB: w, AppDomain: "hopcharlie.dev",
		Envs: []string{"eks", "gke"}, Now: func() time.Time { return w.now }})
}

func (w *world) facts() facts.Facts {
	return facts.Facts{Version: 1, Repo: "org/moim", ServiceURL: "https://moim.hopcharlie.dev", App: "moim",
		GitHub: &facts.GitHub{Installed: true, DefaultBranch: "main", Head: headSHA,
			Workflows: []facts.Workflow{{ID: 5, Path: ".github/workflows/deploy.yml", DeploysToVM: true}, {ID: 6, Path: ".github/workflows/ci.yml"}}},
		HTTPS:    &facts.HTTPS{Host: "moim.hopcharlie.dev", TTLSeconds: 60},
		Instance: &facts.Instance{Cloud: facts.CloudAWS, ID: "i-1", PublicIP: "203.0.113.10", SSMManaged: true},
		VM: &facts.VM{Project: "moim", Networks: []facts.Network{{Name: "moim_default", Subnet: "172.18.0.0/16"}},
			Containers: []facts.Container{
				{Name: "moim-web-1", Service: "web", Env: []facts.EnvVar{{Name: "JWT_SECRET", Secret: true}, {Name: "DB_URL", Secret: true}, {Name: "PGHOST", Secret: true, Excluded: facts.ExcludePlatform}, {Name: "AWS_ACCESS", Secret: true}}},
				{Name: "moim-db-1", Service: "db", Env: []facts.EnvVar{{Name: "POSTGRES_PASSWORD", Secret: true, Excluded: facts.ExcludeDBContainer}}}}},
		DB: &facts.DB{Container: "moim-db-1", Service: "db", ServerVersionNum: 170011, Database: "moim", Owner: "moim", EnvUser: "moim", FenceUser: "moim",
			Databases: []facts.Database{{Name: "moim", SizeBytes: 1 << 20}}},
		Storage: &facts.Storage{Provider: facts.StorageAWS, Bucket: "moim-files", Container: "moim-web-1", AccessKeyEnv: "AWS_ACCESS"},
		Target: &facts.Target{Env: "eks", Image: "ghcr.io/cloudnative-pg/postgresql:17.11-standard-bookworm@sha256:aa", StorageSize: "2Gi",
			PublicService: "web", Port: 8080, HealthCheck: "/healthz", Reindex: []string{"public.users_email_idx"}},
		Report: facts.Report{Blockers: w.blockers, Hold: &facts.HoldEstimate{ReplaySeconds: 1, PromoteSeconds: 2, FenceGateSeconds: 3}}}
}

// Discoverer
func (w *world) Discover(_ context.Context, req DiscoverRequest) (Discovered, error) {
	w.did("discover")
	if w.discoverErr != nil {
		return Discovered{}, w.discoverErr
	}
	vals, _ := json.Marshal(map[string]map[string]string{
		"moim-web-1": {"JWT_SECRET": "jwt", "DB_URL": "postgres://moim:x@db:5432/moim", "AWS_ACCESS": "AKIA"},
		"moim-db-1":  {"POSTGRES_PASSWORD": "pw"}})
	sn, sc, _ := w.cipher.SealOnboarding(req.ID, secrets.OnboardingSecrets, string(vals))
	pn, pc, _ := w.cipher.SealOnboarding(req.ID, secrets.OnboardingSuperuserPassword, "pw")
	d := Discovered{Facts: w.facts(), Secrets: Sealed{sn, sc}, SuperuserPassword: Sealed{pn, pc}}
	// delete_dns_record is not done until syncing sees the name move.
	d.HumanActions = []apiv1.OnboardingHumanAction{{Kind: apiv1.OnboardingHumanActionKindDeleteDnsRecord, Done: false}}
	if w.human {
		d.HumanActions = append(d.HumanActions, apiv1.OnboardingHumanAction{Kind: apiv1.OnboardingHumanActionKindInstallGithubApp})
	}
	return d, nil
}

// Connector
func (w *world) Connect(_ context.Context, req ConnectRequest) (Connected, error) {
	w.did("connect")
	ch := []apiv1.OnboardingLegacyChange{
		{At: w.now, Kind: apiv1.OnboardingLegacyChangeKindInstanceTagged, Target: "i-1"},
		{At: w.now, Kind: apiv1.OnboardingLegacyChangeKindReplicationRoleCreated, Target: "hopcharlie_replicator"},
	}
	if w.connectErr != nil {
		return Connected{Changes: ch[:1]}, w.connectErr
	}
	ch = append(ch, apiv1.OnboardingLegacyChange{At: w.now, Kind: apiv1.OnboardingLegacyChangeKindTailscaleDeviceJoined, Target: "hop-db-legacy-moim"})
	n, c, _ := w.cipher.SealOnboarding(req.ID, secrets.OnboardingReplicationPassword, "repl")
	return Connected{Changes: ch, ReplicationPassword: Sealed{n, c}, Rotated: w.rotated}, nil
}

func (w *world) Disconnect(_ context.Context, _ ConnectRequest, ch []apiv1.OnboardingLegacyChange) ([]apiv1.OnboardingLegacyChange, error) {
	if w.disconnectErr != nil {
		return nil, w.disconnectErr
	}
	out := make([]apiv1.OnboardingLegacyChange, 0, len(ch))
	for i := len(ch) - 1; i >= 0; i-- {
		c := ch[i]
		w.did("undo " + string(c.Kind))
		c.UndoneAt = &w.now
		out = append(out, c)
	}
	return out, nil
}

func (w *world) StopInstance(context.Context, ConnectRequest) ([]apiv1.OnboardingLegacyChange, error) {
	w.did("stop instance")
	return []apiv1.OnboardingLegacyChange{{Kind: apiv1.OnboardingLegacyChangeKindInstanceTagged, Target: "i-1", UndoneAt: &w.now}}, nil
}
func (w *world) RemoveDevice(context.Context, ConnectRequest) ([]apiv1.OnboardingLegacyChange, error) {
	w.did("remove device")
	return []apiv1.OnboardingLegacyChange{{Kind: apiv1.OnboardingLegacyChangeKindTailscaleDeviceJoined, Target: "hop-db-legacy-moim", UndoneAt: &w.now}}, nil
}

// GitHub
func (w *world) ResolveRef(context.Context, string, string) (string, error) { return headSHA, nil }
func (w *world) OpenPR(_ context.Context, _, head, base, _, _ string) (github.PullRequest, error) {
	w.did("open pr " + head + " into " + base)
	return github.PullRequest{Number: 12, URL: "u", State: "open", HeadSHA: headSHA, BaseRef: base}, nil
}
func (w *world) GetPR(context.Context, string, int) (github.PullRequest, error) {
	return github.PullRequest{Number: 12, State: "open", HeadSHA: headSHA, BaseRef: "main"}, nil
}
func (w *world) MergePR(_ context.Context, _ string, _ int, sha string) error {
	w.did("merge " + sha[:7])
	return nil
}
func (w *world) ClosePR(context.Context, string, int) error                    { w.did("close pr"); return nil }
func (w *world) BehindBy(context.Context, string, string, string) (int, error) { return 0, nil }
func (w *world) DisableWorkflow(_ context.Context, _ string, id int64) error {
	w.did("disable workflow")
	return nil
}
func (w *world) EnableWorkflow(context.Context, string, int64) error {
	w.did("enable workflow")
	return nil
}
func (w *world) ActiveRuns(context.Context, string, int64) ([]github.WorkflowRun, error) {
	return nil, nil
}
func (w *world) CancelRun(context.Context, string, int64) error { return nil }

// AI
func (w *world) StartOnboard(_ context.Context, req airun.OnboardRequest, _ string) (airun.Run, error) {
	w.jobs++
	w.did("hopai")
	if req.CheckLog != nil {
		w.did("hopai with log")
	}
	return airun.Run{Name: "job" + string(rune('0'+w.jobs)), Status: "running"}, nil
}
func (w *world) ListOnboard(context.Context, string) ([]airun.Run, error) {
	return []airun.Run{{Name: "job" + string(rune('0'+w.jobs)), Status: w.hopaiStatus}}, nil
}
func (w *world) StopOnboard(context.Context, string) error { w.did("stop hopai"); return nil }
func (w *world) Outcome(context.Context, string) (airun.OnboardOutcome, error) {
	res := `{"kind":"onboard","head":"` + headSHA + `","summary":"요약","mapping":[{"composeService":"db","resource":"db"}]}`
	return airun.OnboardOutcome{ExitCode: w.hopaiExit, Result: []byte(res)}, nil
}

// Apps
func (w *world) newDeployment(status string) string {
	w.deps++
	id := "dep" + string(rune('0'+w.deps))
	w.deployStatus[id] = status
	return id
}
func (w *world) DeployTest(_ context.Context, t TestApp) (string, error) {
	w.did("deploy test " + t.Name + " " + t.Branch)
	status := store.DeployHealthy
	if w.testFails > 0 {
		w.testFails--
		status = store.DeployFailed
	}
	return w.newDeployment(status), nil
}
func (w *world) Owned(_ context.Context, app string) (bool, error) { return !w.foreignApp, nil }
func (w *world) Deploy(context.Context, string, string) (string, error) {
	w.did("deploy")
	status := store.DeployHealthy
	if w.firstDeployFails > 0 {
		w.firstDeployFails--
		status = store.DeployFailed
	}
	return w.newDeployment(status), nil
}
func (w *world) Register(ctx context.Context, r Registration) error {
	w.did("register " + r.App)
	if r.AppPassword != "pw" || r.ReplicationPassword != "repl" || r.Secrets["JWT_SECRET"] != "jwt" || len(r.Secrets) != 1 ||
		r.Parameters["max_connections"] != "100" || r.Parameters["max_worker_processes"] != "32" {
		w.t.Errorf("registration %+v", r)
	}
	spec := `{"name":"moim","services":[{"name":"web","port":8080,"public":true,"healthCheck":"/healthz","replicas":2}],"resources":[{"name":"db","type":"postgres"}]}`
	if w.noPostgres {
		spec = `{"name":"moim","services":[{"name":"web","port":8080,"public":true,"healthCheck":"/healthz","replicas":2}],"resources":[]}`
	}
	if err := w.st.CreateApp(ctx, store.App{Name: r.App, Repo: r.Repo, Branch: "main", Environments: []string{r.Env}, Spec: []byte(spec), CreatedAt: w.now}); err != nil && !errors.Is(err, store.ErrConflict) {
		return err
	}
	return nil
}
func (w *world) Redeploy(context.Context, string, string) (string, error) {
	w.did("redeploy")
	return w.newDeployment(store.DeployHealthy), nil
}
func (w *world) Deployment(_ context.Context, id string) (string, string, error) {
	return w.deployStatus[id], "msg", nil
}
func (w *world) DeploymentLog(context.Context, string) ([]byte, error) { return []byte("boom"), nil }
func (w *world) Remove(_ context.Context, app string) error {
	w.did("remove app " + app)
	return nil
}
func (w *world) FinishSource(context.Context, string) error { w.did("source peer"); return nil }
func (w *world) SetReplicationPassword(_ context.Context, _, pw string) error {
	w.did("seal replication password " + pw)
	return nil
}
func (w *world) BackendURL(_ context.Context, app, env string) (string, error) {
	return "https://" + app + "." + env + ".internal", nil
}
func (w *world) Target(_ context.Context, app, env string) (apps.OnboardTarget, error) {
	return apps.OnboardTarget{Endpoint: apiv1.EnginePlanEndpoint{Env: env, Namespace: app, Cluster: app + "-db", Dsn: "postgres://moim:${X_PGPASSWORD}@h:5432/moim"},
		Bucket: env + ":moim", GatewayBackend: env, GatewayURL: "http://gw"}, nil
}

// Kube
func (w *world) StartSeconds(context.Context, string, string, string) (float64, error) {
	return w.startSeconds, nil
}
func (w *world) Replication(context.Context, string, string, string, string) (bool, time.Duration, error) {
	return w.streaming, w.lag, nil
}
func (w *world) SetReplicaSource(_ context.Context, _, _, cluster, source string) error {
	w.did("replica source " + source)
	return nil
}

// Files, DNS
func (w *world) Copy(_ context.Context, env []string, src, dst string) (int, int, error) {
	return 3, 0, nil
}
func (w *world) PointsAtGateway(_ context.Context, _, vmIP string) (bool, error) {
	if vmIP != "203.0.113.10" {
		w.t.Errorf("DNS check without the VM's address: %q", vmIP)
	}
	return w.dns, nil
}
func (w *world) ReadOnly(context.Context, string, string, string, string) (bool, error) {
	return w.sourceFenced, nil
}

// Gateway
func (w *world) Apps(context.Context) (gateway.AppList, error) {
	return gateway.AppList{Apps: []gateway.AppInfo{{Name: "moim", WriteBackend: "eks",
		Backends: map[string]string{"legacy": "https://moim.hopcharlie.dev", "eks": "x"}, ReadWeights: map[string]int{"eks": 100, "legacy": 0},
		Dial: map[string]string{"legacy": "203.0.113.10:443"}}}}, nil
}
func (w *world) PutApp(_ context.Context, name string, spec gateway.AppSpec) error {
	w.did("gateway put " + spec.WriteBackend + " dial=" + spec.Dial["legacy"])
	return nil
}
func (w *world) DeleteApp(context.Context, string) error { w.did("gateway delete"); return nil }

// Routes
func (w *world) EnsureLegacy(context.Context, string) (string, error) {
	w.did("route")
	return "db-legacy-moim.ns.svc.cluster.local:5432", nil
}
func (w *world) RemoveLegacy(context.Context, string) error { w.did("remove route"); return nil }

// Cutover
func (w *world) ReserveOnboarding(_ context.Context, raw []byte, _ string, scope plans.OnboardingScope) (plans.Reserved, string, error) {
	w.did("reserve")
	if w.reserveErr != nil {
		return plans.Reserved{}, "pln_1", w.reserveErr
	}
	var p map[string]any
	if err := json.Unmarshal(raw, &p); err != nil {
		w.t.Fatal(err)
	}
	if scope.SourceHost != "db-legacy-moim.ns.svc.cluster.local:5432" || scope.SourceBucket != "legacy:moim-files" {
		w.t.Errorf("scope %+v", scope)
	}
	return plans.Reserved{Plan: &plan.Plan{ID: p["id"].(string)}}, "pln_1", nil
}
func (w *world) Started(context.Context, string, string) error { return nil }
func (w *world) Release(context.Context, string)               { w.did("release") }
func (w *world) Start(context.Context, runs.StartOptions) (apiv1.Run, error) {
	w.did("run")
	return apiv1.Run{RunId: "run1"}, nil
}
func (w *world) FindRun(_ context.Context, planID string) (apiv1.Run, bool, error) {
	if w.lostRun {
		return apiv1.Run{}, false, nil
	}
	return apiv1.Run{RunId: "run1", PlanId: planID}, true, nil
}
func (w *world) Get(context.Context, string) (apiv1.Run, error) {
	return apiv1.Run{RunId: "run1", Status: w.runStatus}, nil
}

// SourceDB
func (w *world) Check(_ context.Context, host, db, user, pw, repl string) error {
	if pw != "pw" || repl != "repl" {
		w.t.Errorf("check with %q %q", pw, repl)
	}
	w.did("check source")
	return nil
}
func (w *world) DropRole(_ context.Context, dsn, role string) error {
	if dsn != "postgres://moim:pw@h:5432/moim" {
		w.t.Errorf("drop with %s", dsn)
	}
	w.did("drop " + role)
	return nil
}
