package job

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/onboard/facts"
)

var ctx = context.Background()

func start(t *testing.T, w *world, r *Runner) string {
	t.Helper()
	o, err := r.Start(ctx, apiv1.OnboardingRequest{Repo: "org/moim", ServiceUrl: "https://moim.hopcharlie.dev"}, "op")
	if err != nil {
		t.Fatal(err)
	}
	r.Wait()
	return o.Id
}

func state(t *testing.T, r *Runner, id string) apiv1.Onboarding {
	t.Helper()
	o, err := r.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func want(t *testing.T, r *Runner, id string, s apiv1.OnboardingState) apiv1.Onboarding {
	t.Helper()
	o := state(t, r, id)
	if o.State != s {
		t.Fatalf("state %s (failure %q), want %s", o.State, deref(o.Failure), s)
	}
	return o
}

// toApproval runs an onboarding to awaiting_approval.
func toApproval(t *testing.T, w *world, r *Runner) string {
	t.Helper()
	id := start(t, w, r)
	want(t, r, id, apiv1.OnboardingStatePatching) // hopai runs
	w.hopaiStatus = "succeeded"
	r.Advance(ctx, id)
	o := want(t, r, id, apiv1.OnboardingStateAwaitingApproval)
	if o.Pr == nil || o.Pr.HeadSha != headSHA || o.Check == nil || o.Check.Result != apiv1.OnboardingCheckResultPassed || o.PlannedLegacyChanges == nil {
		t.Fatalf("approval screen %+v", o)
	}
	return id
}

// toReady runs an onboarding to ready.
func toReady(t *testing.T, w *world, r *Runner) string {
	t.Helper()
	id := toApproval(t, w, r)
	if _, err := r.Approve(ctx, id, headSHA, "op"); err != nil {
		t.Fatal(err)
	}
	r.Wait()
	o := want(t, r, id, apiv1.OnboardingStateSyncing) // waits the TTL after the DNS changed
	if !o.LegacyDeployStopped {
		t.Fatal("the source's deploys are not shown stopped")
	}
	w.now = w.now.Add(time.Minute)
	r.Advance(ctx, id)
	want(t, r, id, apiv1.OnboardingStateReady)
	return id
}

func TestHappyPath(t *testing.T) {
	w := newWorld(t)
	r := w.runner()
	id := toReady(t, w, r)
	if _, err := r.Cutover(ctx, id, "op"); err != nil {
		t.Fatal(err)
	}
	r.Wait()
	o := want(t, r, id, apiv1.OnboardingStateCuttingOver)
	if o.RunId == nil || *o.RunId != "run1" {
		t.Fatalf("run %v", o.RunId)
	}
	w.runStatus = apiv1.RunStatusSucceeded
	r.Advance(ctx, id)
	o = want(t, r, id, apiv1.OnboardingStateDecommissioning)
	if _, err := r.Decommission(ctx, id, "op"); err != nil {
		t.Fatal(err)
	}
	want(t, r, id, apiv1.OnboardingStateDone)
	got := strings.Join(w.done(), "\n")
	order := []string{"discover", "hopai", "open pr hopcharlie/onboard into main", "deploy test " + id + " hopcharlie/onboard", "remove app " + id,
		"disable workflow", "connect", "route", "check source", "merge 9fceb02", "register moim", "gateway put legacy dial=203.0.113.10:443",
		"reserve", "run", "replica source moim-db-gke", "source peer", "redeploy", "drop hopcharlie_replicator",
		"gateway put eks dial=", "undo tailscale_device_joined", "remove route", "stop instance"}
	at := 0
	for _, s := range order {
		i := strings.Index(got[at:], s)
		if i < 0 {
			t.Fatalf("%q not after position %d in\n%s", s, at, got)
		}
		at += i + len(s)
	}
	// Events were kept, with the syncing data the console reads.
	evs, _ := w.st.ListOnboardingEvents(ctx, id, 0, 0)
	found := false
	for _, e := range evs {
		if e.Kind == "step" && strings.Contains(string(e.Data), `"filesPending"`) && strings.Contains(string(e.Data), `"lagSeconds"`) && strings.Contains(string(e.Data), `"dnsToGateway"`) {
			found = true
		}
	}
	if !found {
		t.Fatal("no syncing step event with lagSeconds, filesCopied, filesPending, dnsToGateway")
	}
}

// TestPostRunNeedsPostgresResource: the Cluster's name comes from the
// registered platform.yaml; without a postgres resource the onboarding
// fails and says so.
func TestPostRunNeedsPostgresResource(t *testing.T) {
	w := newWorld(t)
	w.noPostgres = true
	r := w.runner()
	id := toReady(t, w, r)
	if _, err := r.Cutover(ctx, id, "op"); err != nil {
		t.Fatal(err)
	}
	r.Wait()
	w.runStatus = apiv1.RunStatusSucceeded
	r.Advance(ctx, id)
	o := want(t, r, id, apiv1.OnboardingStateFailed)
	if !strings.Contains(deref(o.Failure), "no postgres resource") {
		t.Fatalf("failure %q", deref(o.Failure))
	}
}

func TestStartRefuses(t *testing.T) {
	w := newWorld(t)
	r := w.runner()
	for _, u := range []string{"http://moim.hopcharlie.dev", "https://moim.example.com", "https://a.b.hopcharlie.dev", "https://onb-1.hopcharlie.dev", "https://moim.hopcharlie.dev/x"} {
		if _, err := r.Start(ctx, apiv1.OnboardingRequest{Repo: "org/moim", ServiceUrl: u}, "op"); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", u, err)
		}
	}
	start(t, w, r)
	if _, err := r.Start(ctx, apiv1.OnboardingRequest{Repo: "org/moim", ServiceUrl: "https://moim.hopcharlie.dev"}, "op"); !errors.Is(err, ErrExists) {
		t.Fatalf("second onboarding of the app: %v", err)
	}
	if active, _, _ := r.Active(ctx, "moim"); !active {
		t.Fatal("not active")
	}
}

func TestDiscoveryFailsAndResumes(t *testing.T) {
	w := newWorld(t)
	w.discoverErr = errors.New("ssm down")
	r := w.runner()
	id := start(t, w, r)
	o := want(t, r, id, apiv1.OnboardingStateFailed)
	if o.FailedState == nil || *o.FailedState != apiv1.OnboardingStateDiscovering || !strings.Contains(deref(o.Failure), "ssm down") {
		t.Fatalf("%+v", o)
	}
	w.discoverErr = nil
	if _, err := r.Resume(ctx, id, "op"); err != nil {
		t.Fatal(err)
	}
	r.Wait()
	want(t, r, id, apiv1.OnboardingStatePatching)
}

func TestBlockerFails(t *testing.T) {
	w := newWorld(t)
	w.blockers = []facts.Finding{{Code: facts.BlockManagedDatabase, Message: "RDS"}}
	r := w.runner()
	id := start(t, w, r)
	o := want(t, r, id, apiv1.OnboardingStateFailed)
	if len(o.Report.Blockers) != 1 || !strings.Contains(deref(o.Failure), "managed_database") {
		t.Fatalf("%+v", o.Report)
	}
}

func TestHumanActionWaitsForResume(t *testing.T) {
	w := newWorld(t)
	w.human = true
	r := w.runner()
	id := start(t, w, r)
	want(t, r, id, apiv1.OnboardingStateDiscovering)
	r.Advance(ctx, id)
	if n := strings.Count(strings.Join(w.done(), ","), "discover"); n != 1 {
		t.Fatalf("discovered %d times while waiting", n)
	}
	w.human = false
	if _, err := r.Resume(ctx, id, "op"); err != nil {
		t.Fatal(err)
	}
	r.Wait()
	want(t, r, id, apiv1.OnboardingStatePatching)
}

func TestHopaiFailure(t *testing.T) {
	w := newWorld(t)
	r := w.runner()
	id := start(t, w, r)
	w.hopaiStatus, w.hopaiExit = "failed", 1
	r.Advance(ctx, id)
	want(t, r, id, apiv1.OnboardingStateFailed)
}

func TestCheckRetriesThenFails(t *testing.T) {
	w := newWorld(t)
	w.testFails = 1
	r := w.runner()
	id := start(t, w, r)
	w.hopaiStatus = "succeeded"
	r.Advance(ctx, id)
	// One failure: hopai again with the log, then the test passes.
	if !slices.Contains(w.done(), "hopai with log") {
		t.Fatalf("hopai did not get the log: %v", w.done())
	}
	o := want(t, r, id, apiv1.OnboardingStateAwaitingApproval)
	if o.Check.Attempts != 2 {
		t.Fatalf("attempts %d", o.Check.Attempts)
	}

	w2 := newWorld(t)
	w2.testFails = 2
	r2 := w2.runner()
	id2 := start(t, w2, r2)
	w2.hopaiStatus = "succeeded"
	r2.Advance(ctx, id2)
	r2.Advance(ctx, id2)
	o = want(t, r2, id2, apiv1.OnboardingStateFailed)
	if !slices.ContainsFunc(o.Report.Blockers, func(f apiv1.OnboardingFinding) bool { return f.Code == facts.BlockCheckFailed }) {
		t.Fatalf("blockers %+v", o.Report.Blockers)
	}
}

func TestHoldBudget(t *testing.T) {
	w := newWorld(t)
	w.startSeconds = 39
	r := w.runner()
	id := start(t, w, r)
	w.hopaiStatus = "succeeded"
	r.Advance(ctx, id)
	o := want(t, r, id, apiv1.OnboardingStateFailed)
	if o.Report.HoldSeconds == nil || *o.Report.HoldSeconds != 45 {
		t.Fatalf("hold %v", o.Report.HoldSeconds)
	}
}

func TestApproveChecksHead(t *testing.T) {
	w := newWorld(t)
	r := w.runner()
	id := toApproval(t, w, r)
	if _, err := r.Approve(ctx, id, "0000000000000000000000000000000000000000", "op"); !errors.Is(err, ErrHeadMismatch) {
		t.Fatalf("approve another head: %v", err)
	}
	if _, err := r.Cutover(ctx, id, "op"); !errors.Is(err, ErrState) {
		t.Fatalf("cutover before ready: %v", err)
	}
}

func TestConnectFailureKeepsWhatWasDone(t *testing.T) {
	w := newWorld(t)
	w.connectErr = errors.New("ssm failed")
	r := w.runner()
	id := toApproval(t, w, r)
	if _, err := r.Approve(ctx, id, headSHA, "op"); err != nil {
		t.Fatal(err)
	}
	r.Wait()
	o := want(t, r, id, apiv1.OnboardingStateFailed)
	kinds := []string{}
	for _, c := range o.LegacyChanges {
		kinds = append(kinds, string(c.Kind))
	}
	if strings.Join(kinds, ",") != "workflow_disabled,instance_tagged" || !o.LegacyDeployStopped {
		t.Fatalf("changes %v", kinds)
	}
	// Resume connects again.
	w.connectErr = nil
	if _, err := r.Resume(ctx, id, "op"); err != nil {
		t.Fatal(err)
	}
	r.Wait()
	want(t, r, id, apiv1.OnboardingStateSyncing)
}

func TestRunOutcomes(t *testing.T) {
	w := newWorld(t)
	r := w.runner()
	id := toReady(t, w, r)
	if _, err := r.Cutover(ctx, id, "op"); err != nil {
		t.Fatal(err)
	}
	r.Wait()
	w.runStatus = apiv1.RunStatusHalted
	r.Advance(ctx, id)
	want(t, r, id, apiv1.OnboardingStateCuttingOver)
	w.runStatus = apiv1.RunStatusRolledBack
	r.Advance(ctx, id)
	want(t, r, id, apiv1.OnboardingStateReady)
	// The next attempt has the next plan id.
	w.runStatus = apiv1.RunStatusRunning
	if _, err := r.Cutover(ctx, id, "op"); err != nil {
		t.Fatal(err)
	}
	r.Wait()
	x, _ := r.load(ctx, id)
	if x.doc.Internal.Attempt != 2 {
		t.Fatalf("attempt %d", x.doc.Internal.Attempt)
	}
	if _, err := r.Delete(ctx, id, "op"); !errors.Is(err, ErrState) {
		t.Fatalf("delete while cutting over: %v", err)
	}

	// A plan the gates refuse fails with failedState cutting_over; resume
	// goes back to ready.
	w2 := newWorld(t)
	w2.reserveErr = errors.New("residency denied")
	r2 := w2.runner()
	id2 := toReady(t, w2, r2)
	if _, err := r2.Cutover(ctx, id2, "op"); err == nil {
		t.Fatal("cutover with a refused plan")
	}
	o := want(t, r2, id2, apiv1.OnboardingStateFailed)
	if *o.FailedState != apiv1.OnboardingStateCuttingOver {
		t.Fatalf("failed in %s", *o.FailedState)
	}
	if _, err := r2.Resume(ctx, id2, "op"); err != nil {
		t.Fatal(err)
	}
	r2.Wait()
	want(t, r2, id2, apiv1.OnboardingStateReady)
}

// TestResumeAfterRestart: a new runner on the same store goes on from the
// row, without doing again what is done.
func TestResumeAfterRestart(t *testing.T) {
	w := newWorld(t)
	r := w.runner()
	id := start(t, w, r)
	want(t, r, id, apiv1.OnboardingStatePatching)
	jobs := w.jobs
	r2 := w.runner() // the server restarted while hopai ran
	r2.Tick(ctx)
	r2.Wait()
	if w.jobs != jobs {
		t.Fatal("a restart started hopai again")
	}
	w.hopaiStatus = "succeeded"
	r2.Tick(ctx)
	r2.Wait()
	want(t, r2, id, apiv1.OnboardingStateAwaitingApproval)
}

func TestDeleteBeforeApproval(t *testing.T) {
	w := newWorld(t)
	r := w.runner()
	id := toApproval(t, w, r)
	if _, err := r.Delete(ctx, id, "op"); err != nil {
		t.Fatal(err)
	}
	r.Wait()
	if _, err := r.Get(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("row after delete: %v", err)
	}
	got := w.done()
	if !slices.Contains(got, "close pr") || !slices.Contains(got, "stop hopai") || slices.Contains(got, "enable workflow") {
		t.Fatalf("undo %v", got)
	}
}

func TestDeleteWhilePatching(t *testing.T) {
	w := newWorld(t)
	r := w.runner()
	id := start(t, w, r)
	if _, err := r.Delete(ctx, id, "op"); err != nil {
		t.Fatal(err)
	}
	r.Wait()
	if _, err := r.Get(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("row after delete: %v", err)
	}
	if !slices.Contains(w.done(), "stop hopai") {
		t.Fatal("hopai not stopped")
	}
}

func TestDeleteAfterConnectUndoesInReverse(t *testing.T) {
	w := newWorld(t)
	w.disconnectErr = errors.New("ssm down")
	r := w.runner()
	id := toReady(t, w, r)
	before := len(w.done())
	if _, err := r.Delete(ctx, id, "op"); err != nil {
		t.Fatal(err)
	}
	r.Wait()
	o := want(t, r, id, apiv1.OnboardingStateReady)
	if !o.Deleting || !strings.Contains(deref(o.Failure), "ssm down") {
		t.Fatalf("failed deletion %+v", o)
	}
	if _, err := r.Cutover(ctx, id, "op"); !errors.Is(err, ErrDeleting) {
		t.Fatalf("cutover while deleting: %v", err)
	}
	if _, err := r.Resume(ctx, id, "op"); !errors.Is(err, ErrDeleting) {
		t.Fatalf("resume while deleting: %v", err)
	}
	// Only another DELETE tries again.
	r.Advance(ctx, id)
	w.disconnectErr = nil
	if _, err := r.Delete(ctx, id, "op"); err != nil {
		t.Fatal(err)
	}
	r.Wait()
	if _, err := r.Get(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("row after delete: %v", err)
	}
	got := strings.Join(w.done()[before:], ",")
	// The first attempt removed the route before the source failed; the
	// second does only what is left, in reverse order.
	wantOrder := "remove app moim,undo tailscale_device_joined,undo replication_role_created,undo instance_tagged,enable workflow"
	if !strings.HasSuffix(got, wantOrder) || strings.Count(got, "remove route") != 1 {
		t.Fatalf("undo order\n%s\nwant suffix\n%s", got, wantOrder)
	}
}

func TestAppSecrets(t *testing.T) {
	w := newWorld(t)
	f := w.facts()
	got := AppSecrets(&f, map[string]map[string]string{
		"moim-web-1": {"JWT_SECRET": "j", "DB_URL": "postgres://u:p@db:5432/x", "PGHOST": "db", "AWS_ACCESS": "k"},
		"moim-db-1":  {"POSTGRES_PASSWORD": "pw"}})
	if len(got) != 1 || got["JWT_SECRET"] != "j" {
		t.Fatalf("secrets %v", got)
	}
}

func TestCuttingOverAndPush(t *testing.T) {
	w := newWorld(t)
	r := w.runner()
	id := start(t, w, r)
	if ok, _ := PushAllowed(ctx, w.st, "ORG/moim"); !ok {
		t.Fatal("no push token while patching")
	}
	if busy, _ := CuttingOver(w.st)(ctx, "moim"); busy {
		t.Fatal("cutting over while patching")
	}
	_, _ = r.Delete(ctx, id, "op")
	r.Wait()
	if ok, _ := PushAllowed(ctx, w.st, "org/moim"); ok {
		t.Fatal("push token after delete")
	}
	_ = store.OnboardingDone
}

func TestStandbyParameters(t *testing.T) {
	got := StandbyParameters(&facts.DB{MaxConnections: 300, MaxWALSenders: 4, MaxWorkerProcesses: 8, MaxPreparedTransactions: 10, MaxLocksPerTransaction: 128})
	want := map[string]string{"max_connections": "300", "max_wal_senders": "10", "max_worker_processes": "32",
		"max_prepared_transactions": "10", "max_locks_per_transaction": "128"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}

func TestConnectBlocker(t *testing.T) {
	w := newWorld(t)
	w.connectErr = &Blocker{Code: facts.BlockPendingConfig, Message: "restart it"}
	r := w.runner()
	id := toApproval(t, w, r)
	if _, err := r.Approve(ctx, id, headSHA, "op"); err != nil {
		t.Fatal(err)
	}
	r.Wait()
	o := want(t, r, id, apiv1.OnboardingStateFailed)
	if !slices.ContainsFunc(o.Report.Blockers, func(f apiv1.OnboardingFinding) bool { return f.Code == facts.BlockPendingConfig }) {
		t.Fatalf("blockers %+v", o.Report.Blockers)
	}
}

// TestRotatedPasswordRedeploys: a connect run that changed the replication
// password of an app registered already seals it again and redeploys.
func TestRotatedPasswordRedeploys(t *testing.T) {
	w := newWorld(t)
	r := w.runner()
	id := toReady(t, w, r)
	if _, err := r.update(ctx, id, nil, func(x *rec) (string, error) {
		x.doc.Internal.Connected = false
		return store.OnboardingConnecting, nil
	}); err != nil {
		t.Fatal(err)
	}
	w.rotated = true
	r.Advance(ctx, id)
	got := strings.Join(w.done(), ",")
	if !strings.Contains(got, "seal replication password repl,redeploy") {
		t.Fatalf("no reseal and redeploy in %s", got)
	}
}
