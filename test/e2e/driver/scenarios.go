package main

// Failure scenarios. Each one breaks the target test app on purpose (PUT
// /_fault on its own port) at a set point of the run and checks that the
// platform moves traffic back and loses no acknowledged write.
//
//	migrate           no fault: the normal migration (report in main.go)
//	canary-fault      at the 10% canary step every read on the target answers
//	                  500. Want: run rolled_back in the canary, writes never
//	                  moved, all reads back on the source, no write failed, every
//	                  acknowledged write on the source.
//	observe-rollback  after cutover every request on the target takes 1.2s
//	                  (p95 over the 800ms gate). Want: observe violated, the
//	                  automatic rollback run (slo-guard) succeeded, writes and
//	                  reads back on the source, every acknowledged write on the
//	                  source.
//	lost-response     no migration: for 20s the source app commits every 5th
//	                  write and then drops the connection, so the client never
//	                  hears that it succeeded. Want: every such write retried
//	                  with the same id and answered, no write failed, and
//	                  exactly one row per acknowledged write (no duplicate, no
//	                  row the client does not know of).

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/client"
)

const (
	scenarioMigrate         = "migrate"
	scenarioCanaryFault     = "canary-fault"
	scenarioObserveRollback = "observe-rollback"
	scenarioLostResponse    = "lost-response"
)

type scenario struct {
	name                      string
	targetApp, gwAdmin, gwTok string
	sourceApp                 string
	sourceDSN                 string
	injected                  bool
	injectedAt                time.Time
}

func newScenario(name, sourceApp, targetApp, gwAdmin, gwTok, sourceDSN string) (*scenario, error) {
	s := &scenario{name: name, sourceApp: strings.TrimRight(sourceApp, "/"), targetApp: strings.TrimRight(targetApp, "/"),
		gwAdmin: strings.TrimRight(gwAdmin, "/"), gwTok: gwTok, sourceDSN: sourceDSN}
	switch name {
	case scenarioMigrate:
	case scenarioLostResponse:
		if s.sourceApp == "" || sourceDSN == "" {
			return nil, fmt.Errorf("%s needs -source-app and E2E_SOURCE_DSN", name)
		}
	case scenarioCanaryFault, scenarioObserveRollback:
		if s.targetApp == "" || s.gwAdmin == "" || gwTok == "" || sourceDSN == "" {
			return nil, fmt.Errorf("%s needs -target-app, -gateway-admin, E2E_GATEWAY_TOKEN and E2E_SOURCE_DSN", name)
		}
	default:
		return nil, fmt.Errorf("unknown scenario %q", name)
	}
	return s, nil
}

// request adjusts the start request for the scenario.
func (s *scenario) request(req *apiv1.StartMigrationRequest) {
	if s.name != scenarioObserveRollback {
		return
	}
	// A short canary, then cutover without an operator and watch the target.
	step := "5s"
	req.Canary = &apiv1.CanaryOptions{Weights: []int{50, 100}, StepDuration: &step}
	t, d, w := true, "60s", "10s"
	req.AutoSwitch = &t
	req.Observe = &apiv1.ObserveOptions{Duration: &d, StepDuration: &w, AutoRollback: &t}
}

// onEvent injects the fault at the scenario's point of the run.
func (s *scenario) onEvent(e *apiv1.RunEvent) error {
	if s.injected || e.Data == nil {
		return nil
	}
	data := *e.Data
	switch s.name {
	case scenarioCanaryFault:
		if w, _ := data["weight"].(float64); e.Kind == "canary_step" && w >= 10 {
			return s.inject("status=500&method=GET", "every read on the target answers 500")
		}
	case scenarioObserveRollback:
		if p, _ := data["phase"].(string); e.Kind == "status_changed" && p == string(apiv1.RunPhaseObserve) {
			// Every request, not only reads: writes are most of the target's
			// requests after cutover, so slow reads alone keep p95 low.
			return s.inject("delayMs=1200", "every request on the target takes 1.2s")
		}
	}
	return nil
}

func (s *scenario) inject(query, what string) error {
	if err := s.fault(http.MethodPut, "/_fault?"+query); err != nil {
		return fmt.Errorf("inject fault: %w", err)
	}
	s.injected, s.injectedAt = true, time.Now()
	log.Printf("FAULT injected on the target app: %s", what)
	return nil
}

func (s *scenario) clearFault() {
	if s.injected {
		if err := s.fault(http.MethodDelete, "/_fault"); err != nil {
			log.Printf("clear fault: %v", err)
		}
	}
}

func (s *scenario) fault(method, path string) error {
	app := s.targetApp
	if s.name == scenarioLostResponse {
		app = s.sourceApp
	}
	req, err := http.NewRequest(method, app+path, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("%s %s: %s", method, path, resp.Status)
	}
	return nil
}

// lostResponse runs the lost-response scenario: no migration, only the load
// against the current primary while the source app drops every 5th answer
// after committing. It reports and says whether the scenario passed.
func (s *scenario) lostResponse(ctx context.Context, l *load, stopLoad func()) bool {
	ok := true
	fail := func(format string, a ...any) {
		ok = false
		fmt.Printf("FAIL: "+format+"\n", a...)
	}
	if err := s.inject("dropEvery=5&method=POST", "every 5th write is committed, then its answer is dropped"); err != nil {
		fail("%v", err)
		stopLoad()
		return false
	}
	time.Sleep(20 * time.Second)
	s.clearFault()
	time.Sleep(2 * time.Second)
	stopLoad()

	fmt.Println("== result (" + s.name + ")")
	fmt.Printf("client writes  ok=%d failed=%d\n", l.writeOK.Load(), l.writeFail.Load())
	fmt.Printf("write retries  first attempt failed=%d, completed by a retry=%d, retry attempts=%d\n",
		l.writeFirstFail.Load(), l.writeRecovered.Load(), l.writeRetried.Load())
	if n := l.writeFail.Load(); n != 0 {
		fail("%d writes failed after retries", n)
	}
	if l.writeRecovered.Load() == 0 {
		fail("no answer was dropped and retried; the fault did not take effect")
	}
	found, err := countOnTarget(ctx, s.sourceDSN, l.keys)
	if err != nil {
		fail("count acknowledged writes: %v", err)
		return false
	}
	all, err := countWithPrefix(ctx, s.sourceDSN, l.nonce+"-")
	if err != nil {
		fail("count this run's rows: %v", err)
		return false
	}
	fmt.Printf("rows           %d acknowledged writes, %d of them in the database, %d rows from this run in all\n", len(l.keys), found, all)
	if found != int64(len(l.keys)) {
		fail("%d acknowledged writes are missing", int64(len(l.keys))-found)
	}
	if all != int64(len(l.keys)) {
		fail("%d rows exist that no acknowledged write accounts for (duplicates or unknown commits)", all-int64(len(l.keys)))
	}
	return ok
}

type gatewayState struct {
	WriteBackend string         `json:"writeBackend"`
	ReadWeights  map[string]int `json:"readWeights"`
	WritesHeld   bool           `json:"writesHeld"`
}

func (s *scenario) gateway(ctx context.Context) (gatewayState, error) {
	var st gatewayState
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.gwAdmin+"/v1/state", nil)
	if err != nil {
		return st, err
	}
	req.Header.Set("Authorization", "Bearer "+s.gwTok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return st, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return st, fmt.Errorf("gateway state: %s", resp.Status)
	}
	return st, json.NewDecoder(resp.Body).Decode(&st)
}

// finish waits for the scenario's end, stops the load and checks the result.
func (s *scenario) finish(ctx context.Context, op *client.Client, runID string, l *load, stopLoad func()) bool {
	ok := true
	fail := func(format string, a ...any) {
		ok = false
		fmt.Printf("FAIL: "+format+"\n", a...)
	}
	run, err := op.GetMigration(ctx, runID)
	if err != nil {
		fail("get run: %v", err)
		stopLoad()
		return false
	}
	var rollback *apiv1.Run
	if s.name == scenarioObserveRollback {
		rollback = s.followRollback(ctx, op, runID, fail)
		if rollback != nil {
			run, _ = op.GetMigration(ctx, runID)
		}
	}
	time.Sleep(5 * time.Second) // writes keep landing for a moment
	stopLoad()

	fmt.Println("== result (" + s.name + ")")
	fmt.Printf("run            %s status=%s %s\n", run.RunId, run.Status, deref(run.StatusReason))
	if !s.injected {
		fail("the fault was never injected")
	} else {
		fmt.Printf("fault          injected at %s\n", s.injectedAt.Format("15:04:05"))
	}
	if run.Canary != nil {
		for _, st := range run.Canary.Steps {
			v := "-"
			if st.Verdict != nil {
				v = string(*st.Verdict)
			}
			if st.Gate != nil {
				fmt.Printf("canary %3d%%    %-12s %s\n", st.Weight, v, st.Gate.Reason)
			}
		}
	}
	if o := run.Observe; o != nil {
		for _, st := range o.Steps {
			reason := ""
			if st.Gate != nil {
				reason = st.Gate.Reason
			}
			fmt.Printf("observe        %-12s %s\n", st.Verdict, reason)
		}
	}
	gw, gerr := s.gateway(ctx)
	if gerr != nil {
		fail("gateway state: %v", gerr)
	} else {
		fmt.Printf("gateway        writeBackend=%s readWeights=%v writesHeld=%t\n", gw.WriteBackend, gw.ReadWeights, gw.WritesHeld)
	}
	fmt.Printf("client writes  ok=%d failed=%d; longest gap without a successful write %s\n",
		l.writeOK.Load(), l.writeFail.Load(), l.maxGap.Round(time.Millisecond))
	fmt.Printf("write retries  first attempt failed=%d, completed by a retry=%d, retry attempts=%d\n",
		l.writeFirstFail.Load(), l.writeRecovered.Load(), l.writeRetried.Load())
	fmt.Printf("client reads   ok=%d notFound=%d failed=%d; by backend %v\n",
		l.readOK.Load(), l.readMissing.Load(), l.readFail.Load(), l.byBackend)

	switch s.name {
	case scenarioCanaryFault:
		if run.Status != apiv1.RunStatusRolledBack || run.Phase != apiv1.RunPhaseDone {
			fail("run status is %s, want rolled_back", run.Status)
		}
		if run.Canary == nil || run.Canary.Passed == nil || *run.Canary.Passed {
			fail("the canary did not fail")
		}
		if run.Engine.TrafficOnTarget || run.Engine.Steps["fence"].Status != apiv1.StepStatusPending {
			fail("writes moved although the canary failed (fence %s, trafficOnTarget %t)", run.Engine.Steps["fence"].Status, run.Engine.TrafficOnTarget)
		}
	case scenarioObserveRollback:
		if run.Observe == nil || run.Observe.Passed == nil || *run.Observe.Passed {
			fail("observe did not report a violation")
		}
		if rollback == nil || rollback.Status != apiv1.RunStatusSucceeded {
			st := "none"
			if rollback != nil {
				st = string(rollback.Status)
			}
			fail("the automatic rollback run is %s, want succeeded", st)
		} else {
			fmt.Printf("rollback run   %s status=%s actor=%s downtime %s\n", rollback.RunId, rollback.Status, rollbackActor(rollback), millis(rollback.Result.DowntimeMillis))
		}
	}
	if gerr == nil {
		if gw.WriteBackend != "src" || gw.ReadWeights["dst"] != 0 || gw.WritesHeld {
			fail("traffic is not back on the source: %+v", gw)
		}
	}
	if n := l.writeFail.Load(); n != 0 {
		fail("%d writes failed", n)
	}
	found, err := countOnTarget(ctx, s.sourceDSN, l.keys)
	if err != nil {
		fail("count acknowledged writes on the source: %v", err)
	} else {
		fmt.Printf("recheck        %d of %d acknowledged writes are on the source (primary again)\n", found, len(l.keys))
		if found != int64(len(l.keys)) {
			fail("%d acknowledged writes are missing on the source", int64(len(l.keys))-found)
		}
	}
	return ok
}

// followRollback waits for the automatic rollback run and watches it to its end.
func (s *scenario) followRollback(ctx context.Context, op *client.Client, runID string, fail func(string, ...any)) *apiv1.Run {
	deadline := time.Now().Add(90 * time.Second)
	var id string
	for time.Now().Before(deadline) {
		run, err := op.GetMigration(ctx, runID)
		if err == nil && run.Observe != nil {
			if run.Observe.RollbackRunId != nil {
				id = *run.Observe.RollbackRunId
				break
			}
			if run.Observe.RollbackError != nil {
				fail("automatic rollback could not start: %s", *run.Observe.RollbackError)
				return nil
			}
		}
		time.Sleep(time.Second)
	}
	if id == "" {
		fail("no automatic rollback run started within 90s")
		return nil
	}
	log.Printf("automatic rollback run %s started; watching it", id)
	err := op.Watch(ctx, id, func(m client.Message) error {
		if e := m.Event; e != nil {
			fmt.Printf("%s  rollback %-14s %-10s %s\n", e.Time.Local().Format("15:04:05"), e.Kind, deref(e.Step), deref(e.Message))
		}
		return nil
	})
	if err != nil {
		fail("watch rollback run: %v", err)
	}
	rb, err := op.GetMigration(ctx, id)
	if err != nil {
		fail("get rollback run: %v", err)
		return nil
	}
	return rb
}

func rollbackActor(r *apiv1.Run) string {
	if r.StartedBy != "" {
		return r.StartedBy
	}
	return "-"
}
