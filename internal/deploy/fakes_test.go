package deploy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/gateway"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store/storetest"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/appspec"
)

const testToken = "ghs_test-installation-token"

// --- GitHub ---

type fakeGitHub struct {
	mu    sync.Mutex
	refs  map[string]string            // branch -> commit
	files map[string]map[string]string // commit -> path -> content
}

func newFakeGitHub() *fakeGitHub {
	return &fakeGitHub{refs: map[string]string{}, files: map[string]map[string]string{}}
}

// push adds a commit with files and moves branch to it.
func (f *fakeGitHub) push(branch, commit string, files map[string]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[commit] = files
	f.refs[branch] = commit
}

func (f *fakeGitHub) Token(context.Context) (string, error) { return testToken, nil }

func (f *fakeGitHub) ResolveRef(_ context.Context, _, ref string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c, ok := f.refs[ref]; ok {
		return c, nil
	}
	if _, ok := f.files[ref]; ok {
		return ref, nil
	}
	return "", fmt.Errorf("ref %s: %w", ref, fs.ErrNotExist)
}

func (f *fakeGitHub) ReadFile(_ context.Context, _, ref, path string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.files[ref][path]
	if !ok {
		return nil, fmt.Errorf("%s at %s: %w", path, ref, fs.ErrNotExist)
	}
	return []byte(c), nil
}

// --- builder ---

type fakeBuilder struct {
	mu      sync.Mutex
	calls   []BuildRequest
	err     error
	block   map[string]chan struct{} // by deployment: Build waits for it, ignoring ctx
	started map[string]chan struct{} // by deployment: closed once Build was called
}

func newFakeBuilder() *fakeBuilder {
	return &fakeBuilder{block: map[string]chan struct{}{}, started: map[string]chan struct{}{}}
}

// hold makes the build of deployment id wait until release is called.
func (f *fakeBuilder) hold(id string) (started <-chan struct{}, release func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, s := make(chan struct{}), make(chan struct{})
	f.block[id], f.started[id] = b, s
	return s, func() { close(b) }
}

func (f *fakeBuilder) Build(_ context.Context, req BuildRequest, hooks BuildHooks) (BuildResult, error) {
	f.mu.Lock()
	f.calls = append(f.calls, req)
	n := len(f.calls)
	block, started, err := f.block[req.DeploymentID], f.started[req.DeploymentID], f.err
	f.mu.Unlock()
	id := fmt.Sprintf("fake-build-%d", n)
	if err := hooks.started(id); err != nil {
		return BuildResult{}, err
	}
	hooks.log("Step 1/2 : FROM scratch")
	hooks.log("Step 2/2 : COPY . .")
	if started != nil {
		close(started)
	}
	if block != nil {
		<-block
	}
	if err != nil {
		return BuildResult{}, err
	}
	res := BuildResult{BuildID: id}
	for _, sv := range req.Services {
		sum := sha256.Sum256([]byte(req.Commit + "/" + sv.Name))
		res.Images = append(res.Images, store.DeploymentImage{Service: sv.Name, Image: ImageRepository("registry.test", req.App, sv.Name) + "@sha256:" + hex.EncodeToString(sum[:])})
	}
	return res, nil
}

func (f *fakeBuilder) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// --- gitops ---

// fakeGitOps is a repository with a linear history.
type fakeGitOps struct {
	mu       sync.Mutex
	files    map[string][]byte
	history  []string // commit shas, oldest first
	messages []string
	err      error
	// others is how many commits of other apps land right after each write.
	others int
}

func newFakeGitOps() *fakeGitOps {
	f := &fakeGitOps{files: map[string][]byte{}}
	f.commitLocked("initial commit")
	return f
}

func (f *fakeGitOps) commitLocked(msg string) string {
	sha := fmt.Sprintf("%040x", len(f.history)+1)
	f.history = append(f.history, sha)
	f.messages = append(f.messages, msg)
	return sha
}

func (f *fakeGitOps) head() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.history[len(f.history)-1]
}

func (f *fakeGitOps) Write(_ context.Context, files map[string][]byte, message string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return "", false, f.err
	}
	changed := false
	for p, b := range files {
		if !bytes.Equal(f.files[p], b) {
			f.files[p], changed = bytes.Clone(b), true
		}
	}
	if !changed {
		return f.history[len(f.history)-1], false, nil
	}
	sha := f.commitLocked(message)
	for i := range f.others {
		f.commitLocked(fmt.Sprintf("another app's commit %d", i))
	}
	return sha, true, nil
}

func (f *fakeGitOps) Delete(_ context.Context, dirs []string, message string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return "", false, f.err
	}
	if err := checkDelete(dirs); err != nil {
		return "", false, err
	}
	changed := false
	for p := range f.files {
		if underAny(p, dirs) {
			delete(f.files, p)
			changed = true
		}
	}
	if !changed {
		return f.history[len(f.history)-1], false, nil
	}
	return f.commitLocked(message), true, nil
}

func (f *fakeGitOps) Contains(_ context.Context, revision, commit string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, c := slices.Index(f.history, revision), slices.Index(f.history, commit)
	return r >= 0 && c >= 0 && r >= c, nil
}

func (f *fakeGitOps) snapshot() (map[string][]byte, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return maps.Clone(f.files), slices.Clone(f.messages)
}

// --- Argo CD ---

// fakeArgo reports every Application synced to the gitops head and healthy,
// unless told otherwise.
type fakeArgo struct {
	mu        sync.Mutex
	git       *fakeGitOps
	health    map[string]string // by Application; default Healthy
	missing   map[string]bool
	revision  string // when set, every Application reports this revision
	failed    map[string]string
	refreshes []string
	// multiSource reports the chart's commit next to the gitops revision.
	multiSource     bool
	appSetErr       error
	appSetRefreshes int
}

func newFakeArgo(git *fakeGitOps) *fakeArgo {
	return &fakeArgo{git: git, health: map[string]string{}, missing: map[string]bool{}, failed: map[string]string{}}
}

func (f *fakeArgo) RefreshApplicationSet(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.appSetRefreshes++
	return f.appSetErr
}

func (f *fakeArgo) Refresh(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refreshes = append(f.refreshes, name)
	if f.missing[name] {
		return fs.ErrNotExist
	}
	return nil
}

func (f *fakeArgo) Status(_ context.Context, name string) (AppStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.missing[name] {
		return AppStatus{}, nil
	}
	rev := f.git.head()
	if f.revision != "" {
		rev = f.revision
	}
	revs := []string{rev}
	if f.multiSource {
		revs = []string{strings.Repeat("c", 40), rev} // infra's chart commit first
	}
	st := AppStatus{Exists: true, Revisions: revs, Sync: SyncSynced, Health: HealthHealthy}
	if h := f.health[name]; h != "" {
		st.Health, st.HealthMessage = h, "Deployment web exceeded its progress deadline"
	}
	if msg := f.failed[name]; msg != "" {
		st.Sync, st.Health = "OutOfSync", "Missing"
		st.OperationPhase, st.OperationRevisions, st.OperationMessage = "Failed", revs, msg
	}
	return st, nil
}

func (f *fakeArgo) refreshed() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.refreshes)
}

// --- gateway ---

type fakeGateway struct {
	mu      sync.Mutex
	apps    map[string]gateway.AppInfo
	puts    []gateway.AppSpec
	deletes []string
}

func newFakeGateway() *fakeGateway { return &fakeGateway{apps: map[string]gateway.AppInfo{}} }

func (f *fakeGateway) Apps(context.Context) (gateway.AppList, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	list := gateway.AppList{Domain: "hopcharlie.test", Apps: []gateway.AppInfo{{Name: "app", Default: true}}}
	for _, name := range slices.Sorted(maps.Keys(f.apps)) {
		list.Apps = append(list.Apps, f.apps[name])
	}
	return list, nil
}

func (f *fakeGateway) PutApp(_ context.Context, name string, spec gateway.AppSpec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.apps[name].WritesHeld {
		return &gateway.APIError{Status: 409, Code: "writes_held", Message: "writes of " + name + " are held"}
	}
	f.puts = append(f.puts, spec)
	weights := map[string]int{}
	for b := range spec.Backends {
		weights[b] = 0
	}
	if len(spec.ReadWeights) == 0 {
		weights[spec.WriteBackend] = 100
	} else {
		maps.Copy(weights, spec.ReadWeights)
	}
	f.apps[name] = gateway.AppInfo{Name: name, Backends: maps.Clone(spec.Backends), WriteBackend: spec.WriteBackend, ReadWeights: weights}
	return nil
}

func (f *fakeGateway) DeleteApp(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	info, ok := f.apps[name]
	switch {
	case !ok:
		return &gateway.APIError{Status: 404, Code: "unknown_app"}
	case info.WritesHeld:
		return &gateway.APIError{Status: 409, Code: "writes_held", Message: "writes of " + name + " are held"}
	}
	delete(f.apps, name)
	f.deletes = append(f.deletes, name)
	return nil
}

func (f *fakeGateway) app(name string) (gateway.AppInfo, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.apps[name]
	return a, ok
}

// --- harness ---

const shopSpec = `name: shop
services:
  - name: web
    port: 8080
    healthCheck: /healthz
    public: true
    env:
      LOG_LEVEL: info
  - name: worker
    path: worker
    port: 9000
resources:
  - {name: db, type: postgres}
`

var shopFiles = map[string]string{"platform.yaml": shopSpec, "Dockerfile": "FROM scratch\n", "worker/Dockerfile": "FROM scratch\n"}

type testEnv struct {
	t       *testing.T
	store   store.Store
	github  *fakeGitHub
	builder *fakeBuilder
	git     *fakeGitOps
	argo    *fakeArgo
	gw      *fakeGateway
	cfg     Config
	ex      *Executor
	n       int
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	te := &testEnv{t: t, store: storetest.NewMemory(), github: newFakeGitHub(), builder: newFakeBuilder(), git: newFakeGitOps(), gw: newFakeGateway()}
	te.argo = newFakeArgo(te.git)
	te.cfg = Config{
		Store: te.store, GitHub: te.github, Builder: te.builder, GitOps: te.git, Argo: te.argo,
		Gateway: te.gw, Backends: TemplateBackends{Template: "http://{app}-{service}.{instance}.{env}.test:{port}"},
		AppDomain: "hopcharlie.test", Workers: 2,
		SyncTimeout: 3 * time.Second, PollInterval: 5 * time.Millisecond, RefreshInterval: 20 * time.Millisecond,
	}
	te.github.push("main", sha("c1"), shopFiles)
	return te
}

// start starts an executor on the store; it stops when the test ends.
func (te *testEnv) start() *Executor {
	te.t.Helper()
	ex, err := New(te.cfg)
	if err != nil {
		te.t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := ex.Start(ctx); err != nil {
		te.t.Fatal(err)
	}
	te.t.Cleanup(func() {
		cancel()
		wctx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		if err := ex.Wait(wctx); err != nil {
			te.t.Error("workers did not stop")
		}
	})
	te.ex = ex
	return ex
}

func sha(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:20])
}

func (te *testEnv) register(name string, envs ...string) {
	te.t.Helper()
	spec, errs := appspec.Check([]byte(shopSpec))
	if len(errs) > 0 {
		te.t.Fatal(errs)
	}
	spec.Name = name
	err := te.store.CreateApp(context.Background(), store.App{
		Name: name, Repo: "org/" + name, Branch: "main", Environments: envs, AutoDeploy: true,
		Spec: specJSON(*spec), SpecCommit: "", CreatedBy: "op", CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		te.t.Fatal(err)
	}
}

// addBranch adds branch to app as an instance (ADR 0015) and returns the
// instance name.
func (te *testEnv) addBranch(app, branch string) string {
	te.t.Helper()
	instance, err := appspec.InstanceName(app, branch)
	if err != nil {
		te.t.Fatal(err)
	}
	err = te.store.CreateAppBranch(context.Background(), store.AppBranch{
		App: app, Branch: branch, Instance: instance, AutoDeploy: true, Spec: []byte(`{}`), CreatedBy: "op", CreatedAt: time.Now().UTC(),
	}, appspec.MaxInstances-1)
	if err != nil {
		te.t.Fatal(err)
	}
	return instance
}

// deployment stores a queued deployment of commit to envs of the app's
// default instance.
func (te *testEnv) deployment(app, commit string, envs ...string) store.Deployment {
	te.t.Helper()
	return te.deploymentTo(app, "main", app, commit, envs...)
}

// deploymentTo stores a queued deployment of commit to envs of an instance.
func (te *testEnv) deploymentTo(app, branch, instance, commit string, envs ...string) store.Deployment {
	te.t.Helper()
	te.n++
	d := store.Deployment{
		ID: fmt.Sprintf("dep_%012d", te.n), App: app, Branch: branch, Instance: instance, Ref: branch, Commit: commit, Trigger: "manual", TriggeredBy: "op",
		Status: store.DeployQueued, Images: []store.DeploymentImage{},
		CreatedAt: time.Date(2026, 11, 3, 0, 0, te.n, 0, time.UTC),
	}
	for _, e := range envs {
		d.Environments = append(d.Environments, store.DeploymentEnvironment{Env: e, Status: EnvPending})
	}
	if err := te.store.CreateDeployment(context.Background(), d); err != nil {
		te.t.Fatal(err)
	}
	return d
}

func (te *testEnv) enqueue(id string) {
	te.t.Helper()
	if err := te.ex.Enqueue(context.Background(), id); err != nil {
		te.t.Fatal(err)
	}
}

func (te *testEnv) get(id string) store.Deployment {
	te.t.Helper()
	d, err := te.store.GetDeployment(context.Background(), id)
	if err != nil {
		te.t.Fatal(err)
	}
	return d
}

// wait polls until the deployment is finished and its done event stored.
func (te *testEnv) wait(id string) store.Deployment {
	te.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		d := te.get(id)
		if store.DeploymentFinished(d.Status) {
			evs := te.events(id)
			if len(evs) > 0 && evs[len(evs)-1].Type == EventDone {
				return d
			}
		}
		if time.Now().After(deadline) {
			te.t.Fatalf("deployment %s did not finish; status %s, events:\n%s", id, d.Status, te.dump(id))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (te *testEnv) events(id string) []store.DeploymentEvent {
	te.t.Helper()
	evs, err := te.store.ListDeploymentEvents(context.Background(), id, 0, 0)
	if err != nil {
		te.t.Fatal(err)
	}
	return evs
}

func (te *testEnv) dump(id string) string {
	var b strings.Builder
	for _, e := range te.events(id) {
		fmt.Fprintf(&b, "  %d %s %s %s %s\n", e.Seq, e.Type, e.Phase, e.Env, e.Message)
	}
	return b.String()
}

// phases lists the phase and done events as "type:phase".
func phases(evs []store.DeploymentEvent) []string {
	var out []string
	for _, e := range evs {
		if e.Type == EventPhase || e.Type == EventDone {
			out = append(out, e.Type+":"+e.Phase)
		}
	}
	return out
}

func hasEvent(evs []store.DeploymentEvent, typ, substr string) bool {
	return slices.ContainsFunc(evs, func(e store.DeploymentEvent) bool { return e.Type == typ && strings.Contains(e.Message, substr) })
}
