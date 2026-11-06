// Command driver is the user load and the operator of the E2E test (run.sh).
//
// It sends writes and reads through the gateway for the whole migration and
// records every write that succeeded in the platform's written-records stream.
// Meanwhile it drives the migration through the platform API only: submit the
// plan and upload the target manifest (agent token), approve the reviewed
// revision, start with a canary and approve the switch (operator token). After
// the run it counts the recorded writes on the new primary itself.
//
// It exits 1 unless the run succeeded, verify found no missing write, and
// every recorded write is on the new primary.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/client"
)

const table = "items" // test/e2e/run.sh creates it

func main() {
	apiURL := flag.String("api", "", "platform API URL")
	gwURL := flag.String("gateway", "", "gateway public URL")
	planPath := flag.String("plan", "test/e2e/plan.json", "engine plan to submit")
	manifestPath := flag.String("manifest", "test/e2e/target.yaml", "target CNPG Cluster, uploaded as attachment target.yaml")
	stream := flag.String("stream", "e2e", "written-records stream the plan verifies")
	writers := flag.Int("writers", 4, "concurrent writers")
	readers := flag.Int("readers", 4, "concurrent readers")
	canaryFlag := flag.String("canary", "1,10,25,50,75,90,100", "canary read weights to the target")
	canaryStep := flag.Duration("canary-step", 5*time.Second, "duration of each canary step")
	scenarioFlag := flag.String("scenario", scenarioMigrate, "migrate | canary-fault | observe-rollback | lost-response (see scenarios.go)")
	targetApp := flag.String("target-app", "", "the target test app's own URL, for fault injection")
	sourceApp := flag.String("source-app", "", "the source test app's own URL, for fault injection")
	gwAdmin := flag.String("gateway-admin", "", "gateway admin URL, to check where traffic ended")
	timeout := flag.Duration("timeout", 20*time.Minute, "give up after this long")
	flag.Parse()
	log.SetFlags(log.Ltime)
	agentTok, opTok, targetDSN := os.Getenv("E2E_AGENT_TOKEN"), os.Getenv("E2E_OPERATOR_TOKEN"), os.Getenv("E2E_TARGET_DSN")
	if *apiURL == "" || *gwURL == "" || agentTok == "" || opTok == "" || targetDSN == "" {
		log.Fatal("need -api, -gateway, E2E_AGENT_TOKEN, E2E_OPERATOR_TOKEN and E2E_TARGET_DSN")
	}
	sc, err := newScenario(*scenarioFlag, *sourceApp, *targetApp, *gwAdmin, os.Getenv("E2E_GATEWAY_TOKEN"), os.Getenv("E2E_SOURCE_DSN"))
	must(err, "scenario")
	if *scenarioFlag != scenarioMigrate {
		*stream = "e2e-" + *scenarioFlag
	}
	var weights []int
	for _, s := range strings.Split(*canaryFlag, ",") {
		w, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil {
			log.Fatalf("-canary: %v", err)
		}
		weights = append(weights, w)
	}
	raw, err := os.ReadFile(*planPath)
	must(err, "read plan")
	// One plan id and written-records stream per scenario, so scenarios can
	// run one after another against the same platform.
	raw = []byte(strings.NewReplacer(`"stream:e2e"`, `"stream:`+*stream+`"`,
		`"id": "e2e-src-to-dst"`, `"id": "e2e-`+*scenarioFlag+`"`).Replace(string(raw)))
	manifest, err := os.ReadFile(*manifestPath)
	must(err, "read manifest")

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	agent, op := client.New(*apiURL, agentTok), client.New(*apiURL, opTok)
	began := time.Now()

	// Users first: the platform refuses to start a plan whose stream is empty.
	l := newLoad(*gwURL)
	stopLoad := l.start(*writers, *readers, agent, *stream)
	for {
		st, err := agent.GetWrittenRecordStats(ctx, *stream)
		if err == nil && st.Count > 0 {
			break
		}
		if ctx.Err() != nil {
			log.Fatalf("no written records reached stream %s (last error: %v)", *stream, err)
		}
		time.Sleep(time.Second)
	}
	log.Printf("load running through %s: %d writers, %d readers", *gwURL, *writers, *readers)
	if *scenarioFlag == scenarioLostResponse { // no migration
		ok := sc.lostResponse(ctx, l, stopLoad)
		fmt.Printf("driver wall    %s (load start to end)\n", time.Since(began).Round(time.Second))
		if !ok {
			fmt.Println("== E2E FAILED")
			os.Exit(1)
		}
		fmt.Println("== E2E passed")
		return
	}

	// The operator writes the plan: an agent's (AI) plan needs a sandbox
	// rehearsal report before approval (platform #24, internal/plans/rehearsal.go),
	// which this test of the migration itself does not produce. The API tests
	// cover that gate.
	pr, err := op.SubmitPlan(ctx, raw)
	must(err, "submit plan")
	att, err := op.PutPlanAttachment(ctx, pr.Ref, "target.yaml", manifest)
	must(err, "upload target.yaml")
	pr, err = op.GetPlan(ctx, pr.Ref)
	must(err, "get plan")
	log.Printf("plan %s submitted (operator), target.yaml %d bytes sha256 %.12s, revision %d", pr.Ref, att.Size, att.Sha256, pr.Revision)
	_, err = op.ApprovePlan(ctx, pr.Ref, "e2e: reviewed target.yaml", pr.Revision)
	must(err, "approve plan")
	step := canaryStep.String()
	req := apiv1.StartMigrationRequest{
		PlanRef: pr.Ref,
		Canary:  &apiv1.CanaryOptions{Weights: weights, StepDuration: &step},
	}
	sc.request(&req)
	started, err := op.StartMigration(ctx, req, "")
	must(err, "start migration")
	log.Printf("plan approved (operator); run %s started (scenario %s) with canary %v every %s",
		started.RunId, *scenarioFlag, req.Canary.Weights, *req.Canary.StepDuration)
	defer sc.clearFault()

	progress := time.NewTicker(15 * time.Second)
	defer progress.Stop()
	go func() {
		for range progress.C {
			log.Printf("   load: %s", l.counts())
		}
	}()

	switched := false
	err = op.Watch(ctx, started.RunId, func(m client.Message) error {
		status := ""
		if m.Snapshot != nil {
			status = string(m.Snapshot.Status)
		}
		if e := m.Event; e != nil {
			fmt.Printf("%s  %-14s %-10s %s\n", e.Time.Local().Format("15:04:05"), e.Kind, deref(e.Step), deref(e.Message))
			if e.Kind == "status_changed" && e.Data != nil {
				status, _ = (*e.Data)["status"].(string)
			}
			if err := sc.onEvent(e); err != nil {
				return err
			}
		}
		if status == string(apiv1.RunStatusAwaitingSwitch) && !switched {
			switched = true
			if _, err := op.SwitchMigration(ctx, started.RunId); err != nil {
				return fmt.Errorf("approve switch: %w", err)
			}
			log.Printf("switch approved (operator)")
		}
		return nil
	})
	must(err, "watch run")
	if *scenarioFlag != scenarioMigrate {
		ok := sc.finish(ctx, op, started.RunId, l, stopLoad)
		fmt.Printf("driver wall    %s (load start to end)\n", time.Since(began).Round(time.Second))
		if !ok {
			fmt.Println("== E2E FAILED")
			os.Exit(1)
		}
		fmt.Println("== E2E passed")
		return
	}

	// Writes keep landing on the new primary for a moment; then stop the users.
	time.Sleep(5 * time.Second)
	stopLoad()
	run, err := op.GetMigration(ctx, started.RunId)
	must(err, "get run")
	st, err := agent.GetWrittenRecordStats(ctx, *stream)
	must(err, "written-record stats")

	ok := report(ctx, run, l, st.Count, targetDSN)
	fmt.Printf("driver wall    %s (load start to end)\n", time.Since(began).Round(time.Second))
	if !ok {
		fmt.Println("== E2E FAILED")
		os.Exit(1)
	}
	fmt.Println("== E2E passed")
}

// report prints the result and says whether the test passed.
func report(ctx context.Context, run *apiv1.Run, l *load, streamCount int64, targetDSN string) bool {
	res := run.Result
	ok := true
	fail := func(format string, a ...any) {
		ok = false
		fmt.Printf("FAIL: "+format+"\n", a...)
	}
	fmt.Println("== result")
	fmt.Printf("run            %s status=%s %s\n", run.RunId, run.Status, deref(run.StatusReason))
	if run.Canary != nil {
		for _, s := range run.Canary.Steps {
			if s.Gate != nil {
				v := "-"
				if s.Verdict != nil {
					v = string(*s.Verdict)
				}
				fmt.Printf("canary %3d%%    %-12s %s\n", s.Weight, v, s.Gate.Reason)
			}
		}
	}
	fmt.Printf("downtime       %s (writes held at the gateway, engine)\n", millis(res.DowntimeMillis))
	fmt.Printf("run duration   %s\n", millis(res.DurationMillis))
	fmt.Printf("gateway        requests=%s failed=%s (during the run)\n", num(res.Requests), num(res.FailedRequests))
	fmt.Printf("verify         verified=%s missingRows=%s missingWrites=%s checkedWrites=%s\n",
		boolStr(res.Verified), num(res.MissingRows), num(res.MissingWrites), checkedWrites(run))
	fmt.Printf("client writes  ok=%d failed=%d; longest gap without a successful write %s, slowest write %s\n",
		l.writeOK.Load(), l.writeFail.Load(), l.maxGap.Round(time.Millisecond), l.slowest.Round(time.Millisecond))
	fmt.Printf("write retries  first attempt failed=%d, completed by a retry=%d, retry attempts=%d\n",
		l.writeFirstFail.Load(), l.writeRecovered.Load(), l.writeRetried.Load())
	fmt.Printf("client reads   ok=%d notFound=%d failed=%d; by backend %v\n",
		l.readOK.Load(), l.readMissing.Load(), l.readFail.Load(), l.byBackend)
	fmt.Printf("records        sent=%d unsent=%d stream=%d\n", l.sent, len(l.unsent), streamCount)

	if run.Status != apiv1.RunStatusSucceeded {
		fail("run status is %s, want succeeded", run.Status)
	}
	if res.MissingWrites == nil || *res.MissingWrites != 0 {
		fail("run result missingWrites is %s, want 0", num(res.MissingWrites))
	}
	if len(l.unsent) > 0 || streamCount != l.sent {
		fail("written records did not all reach the platform (sent %d, unsent %d, stream %d)", l.sent, len(l.unsent), streamCount)
	}
	if run.Status == apiv1.RunStatusSucceeded {
		found, err := countOnTarget(ctx, targetDSN, l.keys)
		if err != nil {
			fail("count recorded writes on the new primary: %v", err)
		} else {
			fmt.Printf("recheck        %d of %d recorded writes are on the new primary\n", found, len(l.keys))
			if found != int64(len(l.keys)) {
				fail("%d recorded writes are missing on the new primary", int64(len(l.keys))-found)
			}
		}
	}
	return ok
}

// countOnTarget counts the recorded writes present on the new primary,
// including the ones made after the engine's verify step.
func countOnTarget(ctx context.Context, dsn string, keys []string) (int64, error) {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return 0, err
	}
	defer conn.Close(ctx)
	var n int64
	err = conn.QueryRow(ctx, "SELECT count(*) FROM "+table+" WHERE id = ANY($1)", keys).Scan(&n)
	return n, err
}

// countWithPrefix counts every row whose id starts with prefix: this run's rows,
// acknowledged or not.
func countWithPrefix(ctx context.Context, dsn, prefix string) (int64, error) {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return 0, err
	}
	defer conn.Close(ctx)
	var n int64
	err = conn.QueryRow(ctx, "SELECT count(*) FROM "+table+" WHERE starts_with(id, $1)", prefix).Scan(&n)
	return n, err
}

// checkedWrites is how many written records the engine's verify step checked.
func checkedWrites(run *apiv1.Run) string {
	rec, ok := run.Engine.Steps["verify"]
	if !ok || rec.Evidence == nil {
		return "-"
	}
	var ev struct {
		Report struct {
			Written *struct {
				Checked int64 `json:"checked"`
			} `json:"written"`
		} `json:"report"`
	}
	b, _ := json.Marshal(*rec.Evidence)
	if json.Unmarshal(b, &ev) != nil || ev.Report.Written == nil {
		return "-"
	}
	return strconv.FormatInt(ev.Report.Written.Checked, 10)
}

// load is the users: writers insert rows with unique keys through the
// gateway, readers read written keys back.
type load struct {
	gw    string
	hc    *http.Client
	nonce string

	mu        sync.Mutex
	keys      []string              // writes that succeeded
	unsent    []apiv1.WrittenRecord // not yet in the written-records stream
	sent      int64
	lastOK    time.Time
	maxGap    time.Duration // longest time with no successful write
	slowest   time.Duration
	byBackend map[string]int64 // reads by X-Charlie-Backend
	failLogs  int

	writeOK, writeFail, readOK, readMissing, readFail atomic.Int64
	// writeFirstFail: writes whose first attempt failed (what a client without
	// retries would have seen); writeRecovered: of those, the ones a retry
	// completed; writeRetried: retry attempts sent.
	writeFirstFail, writeRecovered, writeRetried atomic.Int64
}

func newLoad(gw string) *load {
	return &load{
		gw: strings.TrimRight(gw, "/"),
		// Held writes wait up to the gateway's -max-hold (55s by default).
		hc:        &http.Client{Timeout: 2 * time.Minute, Transport: &http.Transport{MaxIdleConnsPerHost: 64}},
		nonce:     strconv.FormatInt(time.Now().UnixNano(), 36),
		byBackend: map[string]int64{},
	}
}

// start runs the writers, readers and the recorder until the returned
// function is called; that function waits for in-flight requests and sends
// the last records.
func (l *load) start(writers, readers int, agent *client.Client, stream string) func() {
	stop := make(chan struct{})
	var users, rec sync.WaitGroup
	for i := range writers {
		users.Go(func() { l.writer(i, stop) })
	}
	for range readers {
		users.Go(func() { l.reader(stop) })
	}
	done := make(chan struct{})
	rec.Go(func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				l.flush(agent, stream)
			case <-done:
				l.flush(agent, stream)
				return
			}
		}
	})
	return func() {
		close(stop)
		users.Wait()
		close(done)
		rec.Wait()
	}
}

const pace = 50 * time.Millisecond // between one user's requests

func (l *load) writer(i int, stop <-chan struct{}) {
	for seq := 1; ; seq++ {
		select {
		case <-stop:
			return
		default:
		}
		id := fmt.Sprintf("%s-w%d-%d", l.nonce, i, seq)
		t0 := time.Now()
		// A well-behaved client: a write that failed or whose answer was lost
		// is sent again with the same id. The app's insert is idempotent by id,
		// so a retry never adds a second row (platform ADR 0028).
		for attempt := 1; ; attempt++ {
			code, _, err := l.do(http.MethodPost, "/items", `{"id":"`+id+`"}`)
			if err == nil && code == http.StatusCreated {
				if attempt > 1 {
					l.writeRecovered.Add(1)
				}
				l.written(id, t0, time.Now())
				break
			}
			if attempt == 1 {
				l.writeFirstFail.Add(1)
			}
			if attempt >= maxWriteAttempts || !retryable(code, err) {
				l.writeFail.Add(1)
				l.logFail("write", code, err)
				break
			}
			l.writeRetried.Add(1)
			wait := 300 * time.Millisecond
			if code == http.StatusServiceUnavailable {
				wait = time.Second // the gateway's Retry-After
			}
			time.Sleep(wait)
		}
		time.Sleep(pace)
	}
}

// maxWriteAttempts bounds one write's attempts (about 6s of retries).
const maxWriteAttempts = 6

// retryable: a lost answer (transport error) or a 5xx. The gateway's 503s for
// held writes (hold_timeout, too_many_held_writes, shutting_down) were never
// forwarded; other 5xx and lost answers may have been committed, which the
// idempotent insert makes harmless. 4xx are the client's own mistakes.
func retryable(code int, err error) bool {
	return err != nil || code >= 500
}

func (l *load) reader(stop <-chan struct{}) {
	for {
		select {
		case <-stop:
			return
		default:
		}
		l.mu.Lock()
		id := ""
		if n := len(l.keys); n > 0 {
			id = l.keys[rand.IntN(n)]
		}
		l.mu.Unlock()
		if id != "" {
			code, backend, err := l.do(http.MethodGet, "/items/"+id, "")
			switch {
			case err == nil && code == http.StatusOK:
				l.readOK.Add(1)
			case err == nil && code == http.StatusNotFound:
				l.readMissing.Add(1) // e.g. a replica a moment behind during the canary
			default:
				l.readFail.Add(1)
				l.logFail("read", code, err)
			}
			if backend != "" {
				l.mu.Lock()
				l.byBackend[backend]++
				l.mu.Unlock()
			}
		}
		time.Sleep(pace)
	}
}

func (l *load) do(method, path, body string) (int, string, error) {
	req, err := http.NewRequest(method, l.gw+path, strings.NewReader(body))
	if err != nil {
		return 0, "", err
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := l.hc.Do(req)
	if err != nil {
		return 0, "", err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode, resp.Header.Get("X-Charlie-Backend"), nil
}

func (l *load) written(id string, t0, now time.Time) {
	l.writeOK.Add(1)
	l.mu.Lock()
	defer l.mu.Unlock()
	l.keys = append(l.keys, id)
	l.unsent = append(l.unsent, apiv1.WrittenRecord{Table: table, Pk: id, At: &now})
	if !l.lastOK.IsZero() && now.Sub(l.lastOK) > l.maxGap {
		l.maxGap = now.Sub(l.lastOK)
	}
	if now.After(l.lastOK) {
		l.lastOK = now
	}
	l.slowest = max(l.slowest, now.Sub(t0))
}

// flush sends the unsent records; on failure they are kept for the next try.
func (l *load) flush(agent *client.Client, stream string) {
	l.mu.Lock()
	batch := l.unsent
	l.unsent = nil
	l.mu.Unlock()
	for len(batch) > 0 {
		n := min(len(batch), 5000)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_, err := agent.AppendWrittenRecords(ctx, stream, batch[:n])
		cancel()
		if err != nil {
			log.Printf("send written records: %v", err)
			l.mu.Lock()
			l.unsent = append(batch, l.unsent...)
			l.mu.Unlock()
			return
		}
		l.mu.Lock()
		l.sent += int64(n)
		l.mu.Unlock()
		batch = batch[n:]
	}
}

func (l *load) logFail(kind string, code int, err error) {
	l.mu.Lock()
	l.failLogs++
	n := l.failLogs
	l.mu.Unlock()
	if n <= 20 {
		log.Printf("   %s failed: status=%d err=%v", kind, code, err)
	}
}

func (l *load) counts() string {
	return fmt.Sprintf("writes ok=%d failed=%d, reads ok=%d notFound=%d failed=%d",
		l.writeOK.Load(), l.writeFail.Load(), l.readOK.Load(), l.readMissing.Load(), l.readFail.Load())
}

func must(err error, what string) {
	if err != nil {
		log.Fatalf("%s: %v", what, err)
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func num(v *int64) string {
	if v == nil {
		return "-"
	}
	return strconv.FormatInt(*v, 10)
}

func millis(v *int64) string {
	if v == nil {
		return "-"
	}
	return (time.Duration(*v) * time.Millisecond).String()
}

func boolStr(v *bool) string {
	if v == nil {
		return "-"
	}
	return strconv.FormatBool(*v)
}
