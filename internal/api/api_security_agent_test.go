package api

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/agentlog"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/auth"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/runs"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/security"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

// clusterCollector reports fixed findings for one environment (tests only).
type clusterCollector struct {
	src      security.Source
	findings []apiv1.SecurityFinding
}

func (c clusterCollector) Sources() []security.Source { return []security.Source{c.src} }

func (c clusterCollector) Collect(context.Context) []security.Report {
	return []security.Report{{Source: c.src, Status: apiv1.SecuritySourceStatusOk, Message: "fake cluster", Findings: c.findings}}
}

// withSecurity serves /v1/security from the platform collector over the
// harness store, a fake cluster and a source that is not configured.
func withSecurity() option {
	return func(h *harness, _ *runs.Config, d *Deps) {
		gke := clusterCollector{src: security.Source{Name: "kubernetes", Provider: apiv1.SecurityProviderGcp, Environment: "gke"},
			findings: []apiv1.SecurityFinding{{Check: "k8s.privileged", Category: apiv1.SecurityCategoryWorkload, Severity: apiv1.SecuritySeverityHigh,
				Title: "Privileged container", Resource: "shop/Deployment/web/app", Detail: "d", Recommendation: "r"}}}
		d.Security = security.New([]security.Collector{
			gke,
			security.NotConfigured(security.Source{Name: "gcp-scc", Provider: apiv1.SecurityProviderGcp, Environment: "gcp-project"}, "needs a project id"),
			security.NewPlatformCollector(h.store, nil, true),
		}, security.Options{})
	}
}

func withAgents() option {
	return func(h *harness, _ *runs.Config, d *Deps) { d.Agents = agentlog.New(h.store) }
}

// fullLog answers every append as a full session would (tests only).
type fullLog struct{ AgentLog }

func (fullLog) Append(context.Context, auth.Principal, string, agentlog.EventInput) (apiv1.AgentEvent, error) {
	return apiv1.AgentEvent{}, agentlog.ErrSessionFull
}

func TestRootNamesTheService(t *testing.T) {
	h := newHarness(t)
	code, b := h.do("GET", "/", "", nil)
	info := decode[apiv1.ServiceInfo](t, b)
	if code != 200 || info.Name != "hopcharlie-platform" || info.Links.Openapi != "/openapi.yaml" || info.Links.Health != "/healthz" {
		t.Fatalf("GET /: %d %s", code, b)
	}
	resp, err := http.Get(h.srv.URL + "/nope")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("GET /nope: %d", resp.StatusCode)
	}
}

func TestSecurityView(t *testing.T) {
	h := newHarness(t, withSecurity())
	rec := h.submit(planJSON("p1", "a", "b", false))
	if code, _ := h.do("POST", "/v1/plans/"+rec.Ref+"/approve", agentToken, nil); code != 403 {
		t.Fatalf("agent approve: %d", code)
	}

	if code, _ := h.do("GET", "/v1/security", "", nil); code != 401 {
		t.Fatalf("no token: %d", code)
	}
	code, b := h.do("GET", "/v1/security", viewerToken, nil)
	if code != 200 {
		t.Fatalf("security: %d %s", code, b)
	}
	rep := decode[apiv1.SecurityReport](t, b)
	var titles []string
	for _, f := range rep.Findings {
		titles = append(titles, string(f.Severity)+" "+f.Environment+" "+f.Title)
	}
	want := []string{
		"high gke Privileged container",
		"medium platform ai (agent) tried POST /v1/plans/{ref}/approve once; blocked",
		"medium platform Plan database hosts are not restricted",
		// The audit entry is written before the response is sent.
		"low platform anonymous called GET /v1/security without a valid token once; blocked",
	}
	if strings.Join(titles, "\n") != strings.Join(want, "\n") {
		t.Fatalf("findings:\n%s\nwant:\n%s", strings.Join(titles, "\n"), strings.Join(want, "\n"))
	}
	status := map[string]apiv1.SecuritySourceStatus{}
	for _, s := range rep.Sources {
		status[s.Name] = s.Status
	}
	if status["gcp-scc"] != apiv1.SecuritySourceStatusNotConfigured || status["platform-audit"] != apiv1.SecuritySourceStatusOk || len(rep.Sources) != 4 {
		t.Fatalf("sources: %+v", rep.Sources)
	}
	if rep.Summary.BySeverity.Total != 4 || rep.Summary.ByEnvironment["platform"].Medium != 2 || rep.Summary.ByEnvironment["gcp-project"].Total != 0 {
		t.Fatalf("summary: %+v", rep.Summary)
	}

	_, b = h.do("GET", "/v1/security?severity=high", viewerToken, nil)
	if r := decode[apiv1.SecurityReport](t, b); len(r.Findings) != 1 || r.Summary.BySeverity.Total != 1 {
		t.Fatalf("severity=high: %s", b)
	}
	_, b = h.do("GET", "/v1/security?environment=platform&refresh=true", viewerToken, nil)
	if r := decode[apiv1.SecurityReport](t, b); len(r.Findings) != 3 || len(r.Sources) != 2 || len(r.Summary.ByEnvironment) != 1 {
		t.Fatalf("environment=platform: %s", b)
	}
	for _, q := range []string{"severity=urgent", "environment=mars", "refresh=maybe"} {
		if code, b := h.do("GET", "/v1/security?"+q, viewerToken, nil); code != 400 || errCode(t, b) != "invalid_request" {
			t.Errorf("%s: %d %s", q, code, b)
		}
	}

	h2 := newHarness(t)
	if code, b := h2.do("GET", "/v1/security", viewerToken, nil); code != 503 || errCode(t, b) != "security_not_configured" {
		t.Fatalf("not configured: %d %s", code, b)
	}
}

func (h *harness) createSession(token string, req apiv1.CreateAgentSessionRequest) apiv1.AgentSession {
	h.t.Helper()
	code, b := h.do("POST", "/v1/agent/sessions", token, req)
	if code != 201 {
		h.t.Fatalf("create session: %d %s", code, b)
	}
	return decode[apiv1.AgentSession](h.t, b)
}

func (h *harness) agentEvent(token, id string, body any) (int, []byte) {
	h.t.Helper()
	return h.do("POST", "/v1/agent/sessions/"+id+"/events", token, body)
}

func TestAgentSessionLifecycle(t *testing.T) {
	h := newHarness(t, withAgents())
	planRef := "pln_1"
	a := h.createSession(agentToken, apiv1.CreateAgentSessionRequest{Kind: apiv1.AgentSessionKindPlan, Title: "plan gke -> eks", PlanRef: &planRef, Model: ptr("claude-test")})
	if a.Actor != "ai" || a.Status != apiv1.AgentSessionStatusRunning || deref(a.PlanRef) != planRef {
		t.Fatalf("session: %+v", a)
	}
	if code, _ := h.do("POST", "/v1/agent/sessions", viewerToken, apiv1.CreateAgentSessionRequest{Kind: apiv1.AgentSessionKindPlan, Title: "x"}); code != 403 {
		t.Fatalf("viewer create: %d", code)
	}
	if code, b := h.do("POST", "/v1/agent/sessions", agentToken, `{"kind":"plan"}`); code != 400 {
		t.Fatalf("no title: %d %s", code, b)
	}

	code, b := h.agentEvent(agentToken, a.Id, map[string]any{
		"type": "tool_call", "tool": "submit_plan", "status": "ok", "durationMillis": 250,
		"input":  map[string]any{"plan": map[string]any{"id": "p1", "gateway": map[string]any{"token": "gw-secret-value"}}},
		"output": map[string]any{"ref": "pln_1"},
		"usage":  map[string]any{"inputTokens": 1200, "outputTokens": 90},
	})
	if code != 201 {
		t.Fatalf("event: %d %s", code, b)
	}
	if strings.Contains(string(b), "gw-secret-value") || !strings.Contains(string(b), `"token":"[redacted]"`) {
		t.Fatalf("event not redacted: %s", b)
	}
	if code, b := h.agentEvent(agentToken, a.Id, map[string]any{"type": "blocked", "tool": "approve_plan", "output": "403 forbidden: role agent may not do this"}); code != 201 {
		t.Fatalf("blocked event: %d %s", code, b)
	}
	if code, b := h.agentEvent(agentToken, a.Id, map[string]any{"type": "message", "extra": 1}); code != 400 {
		t.Fatalf("unknown field: %d %s", code, b)
	}
	if code, b := h.agentEvent(agentToken, a.Id, map[string]any{"type": "message", "output": strings.Repeat("x", agentlog.MaxPayloadBytes)}); code != 413 || errCode(t, b) != "too_large" {
		t.Fatalf("large output: %d %s", code, b)
	}
	if code, _ := h.agentEvent(viewerToken, a.Id, map[string]any{"type": "message"}); code != 403 {
		t.Fatalf("viewer event: %d", code)
	}
	if code, b := h.agentEvent(agentToken, "ags_missing", map[string]any{"type": "message"}); code != 404 || errCode(t, b) != "agent_session_not_found" {
		t.Fatalf("missing session: %d %s", code, b)
	}

	// The operator's own session: the agent may not write to it.
	own := h.createSession(opToken, apiv1.CreateAgentSessionRequest{Kind: apiv1.AgentSessionKindDiagnose, Title: "diagnose"})
	if code, b := h.agentEvent(agentToken, own.Id, map[string]any{"type": "message"}); code != 403 || errCode(t, b) != "forbidden" {
		t.Fatalf("agent writes to the operator's session: %d %s", code, b)
	}
	if code, _ := h.agentEvent(opToken, a.Id, map[string]any{"type": "decision", "output": "looks fine"}); code != 201 {
		t.Fatalf("operator writes to the agent's session: %d", code)
	}

	code, b = h.do("GET", "/v1/agent/sessions/"+a.Id, viewerToken, nil)
	got := decode[apiv1.AgentSession](t, b)
	if code != 200 || got.Totals != (apiv1.AgentSessionTotals{Events: 3, ToolCalls: 1, InputTokens: 1200, OutputTokens: 90, DurationMillis: 250}) {
		t.Fatalf("get session: %d %s", code, b)
	}
	_, b = h.do("GET", "/v1/agent/sessions?planRef="+planRef, viewerToken, nil)
	if l := decode[apiv1.AgentSessionList](t, b); len(l.Sessions) != 1 || l.Sessions[0].Id != a.Id {
		t.Fatalf("list by plan: %s", b)
	}
	_, b = h.do("GET", "/v1/agent/sessions", viewerToken, nil)
	if l := decode[apiv1.AgentSessionList](t, b); len(l.Sessions) != 2 || l.Sessions[0].Id != own.Id {
		t.Fatalf("list newest first: %s", b)
	}
	if code, _ := h.do("GET", "/v1/agent/sessions?limit=0", viewerToken, nil); code != 400 {
		t.Fatalf("limit=0: %d", code)
	}
	code, b = h.do("GET", "/v1/agent/sessions/"+a.Id+"/events?format=json&after=1", viewerToken, nil)
	if l := decode[apiv1.AgentEventList](t, b); code != 200 || len(l.Events) != 2 || l.Events[0].Type != apiv1.AgentEventTypeBlocked {
		t.Fatalf("events json: %d %s", code, b)
	}

	code, b = h.do("POST", "/v1/agent/sessions/"+a.Id+"/finish", agentToken, map[string]any{"status": "succeeded", "summary": "plan pln_1 submitted; waiting for approval"})
	if fin := decode[apiv1.AgentSession](t, b); code != 200 || fin.Status != apiv1.AgentSessionStatusSucceeded || fin.EndedAt == nil {
		t.Fatalf("finish: %d %s", code, b)
	}
	if code, b := h.agentEvent(agentToken, a.Id, map[string]any{"type": "message"}); code != 409 || errCode(t, b) != "agent_session_finished" {
		t.Fatalf("event after finish: %d %s", code, b)
	}
	if code, b := h.do("POST", "/v1/agent/sessions/"+a.Id+"/finish", agentToken, map[string]any{"status": "failed"}); code != 409 {
		t.Fatalf("second finish: %d %s", code, b)
	}
	if code, _ := h.do("POST", "/v1/agent/sessions/"+own.Id+"/finish", agentToken, map[string]any{"status": "running"}); code != 400 {
		t.Fatalf("finish as running: %d", code)
	}

	// Every write is in the audit log, without the event payloads.
	_, b = h.do("GET", "/v1/audit", viewerToken, nil)
	if strings.Contains(string(b), "gw-secret-value") || strings.Contains(string(b), "pln_1 submitted") {
		t.Fatalf("payload in the audit log: %s", b)
	}
	var lines []string
	for _, e := range decode[apiv1.AuditList](t, b).Entries {
		if strings.Contains(e.Action, "/v1/agent/") {
			lines = append(lines, fmt.Sprintf("%s %s %d", e.Actor, e.Action, e.Status))
		}
	}
	for _, w := range []string{
		"ai POST /v1/agent/sessions 201",
		"view POST /v1/agent/sessions 403",
		"ai POST /v1/agent/sessions/{id}/events 201",
		"ai POST /v1/agent/sessions/{id}/events 409",
		"ai POST /v1/agent/sessions/{id}/finish 200",
	} {
		if !strings.Contains(strings.Join(lines, "\n"), w) {
			t.Errorf("audit log lacks %q:\n%s", w, strings.Join(lines, "\n"))
		}
	}
}

func TestAgentEventStream(t *testing.T) {
	h := newHarness(t, withAgents())
	a := h.createSession(agentToken, apiv1.CreateAgentSessionRequest{Kind: apiv1.AgentSessionKindRehearsal, Title: "rehearse in the sandbox"})
	event := func(n int) {
		t.Helper()
		if code, b := h.agentEvent(agentToken, a.Id, map[string]any{"type": "tool_call", "tool": "kubectl_get", "input": map[string]any{"n": n}}); code != 201 {
			t.Fatalf("event %d: %d %s", n, code, b)
		}
	}
	event(1)
	event(2)

	msgs := make(chan []sseMsg, 1)
	go func() { msgs <- h.agentStream(a.Id, "") }()
	time.Sleep(50 * time.Millisecond)
	event(3)
	event(4)
	if code, b := h.do("POST", "/v1/agent/sessions/"+a.Id+"/finish", agentToken, map[string]any{"status": "succeeded"}); code != 200 {
		t.Fatalf("finish: %d %s", code, b)
	}
	var got []sseMsg
	select {
	case got = <-msgs:
	case <-time.After(5 * time.Second):
		t.Fatal("stream did not end after finish")
	}

	if names := names(got); len(got) != 6 || names[0] != "snapshot" || names[5] != "snapshot" {
		t.Fatalf("stream: %v", names)
	}
	h.validateSchema("AgentSession", []byte(got[0].data))
	final := decode[apiv1.AgentSession](t, []byte(got[5].data))
	if final.Status != apiv1.AgentSessionStatusSucceeded || final.Totals.Events != 4 {
		t.Fatalf("final snapshot: %+v", final)
	}
	var seqs []int64
	for _, m := range got[1:5] {
		h.validateSchema("AgentEvent", []byte(m.data))
		if m.event != "tool_call" || m.id == 0 || (len(seqs) > 0 && m.id <= seqs[len(seqs)-1]) {
			t.Fatalf("event message %+v after %v", m, seqs)
		}
		seqs = append(seqs, m.id)
	}

	// A finished session replays from Last-Event-ID and ends at once.
	replay := h.agentStream(a.Id, strconv.FormatInt(seqs[1], 10))
	var ids []int64
	for _, m := range replay {
		if m.id > 0 {
			ids = append(ids, m.id)
		}
	}
	if fmt.Sprint(ids) != fmt.Sprint(seqs[2:]) || replay[0].event != "snapshot" || replay[len(replay)-1].event != "snapshot" {
		t.Fatalf("replay after %d: %v (%v)", seqs[1], ids, names(replay))
	}
}

// agentStream reads a session's event stream until the server closes it.
func (h *harness) agentStream(id, lastEventID string) []sseMsg {
	h.t.Helper()
	req, _ := http.NewRequest("GET", h.srv.URL+"/v1/agent/sessions/"+id+"/events", nil)
	req.Header.Set("Authorization", "Bearer "+viewerToken)
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Error(err)
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" {
		h.t.Errorf("agent events: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
		return nil
	}
	var out []sseMsg
	var cur sseMsg
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if cur.event != "" {
				out = append(out, cur)
			}
			cur = sseMsg{}
		case strings.HasPrefix(line, "id: "):
			cur.id, _ = strconv.ParseInt(strings.TrimPrefix(line, "id: "), 10, 64)
		case strings.HasPrefix(line, "event: "):
			cur.event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			cur.data = strings.TrimPrefix(line, "data: ")
		}
	}
	return out
}

func TestAgentEventPages(t *testing.T) {
	h := newHarness(t, withAgents())
	a := h.createSession(agentToken, apiv1.CreateAgentSessionRequest{Kind: apiv1.AgentSessionKindPlan, Title: "pages"})
	for n := range 5 {
		if code, b := h.agentEvent(agentToken, a.Id, map[string]any{"type": "message", "output": n}); code != 201 {
			t.Fatalf("event %d: %d %s", n, code, b)
		}
	}
	var seqs []int64
	path := "/v1/agent/sessions/" + a.Id + "/events?format=json&limit=2"
	after := ""
	for pages := 1; ; pages++ {
		code, b := h.do("GET", path+after, viewerToken, nil)
		l := decode[apiv1.AgentEventList](t, b)
		if code != 200 || len(l.Events) > 2 || pages > 3 {
			t.Fatalf("page %d: %d %s", pages, code, b)
		}
		for _, e := range l.Events {
			seqs = append(seqs, e.Seq)
		}
		if l.NextAfter == nil {
			break
		}
		after = "&after=" + strconv.FormatInt(*l.NextAfter, 10)
	}
	if len(seqs) != 5 {
		t.Fatalf("paged seqs %v, want 5", seqs)
	}
	for _, q := range []string{"limit=0", "limit=2001", "limit=x"} {
		if code, b := h.do("GET", "/v1/agent/sessions/"+a.Id+"/events?format=json&"+q, viewerToken, nil); code != 400 || errCode(t, b) != "invalid_request" {
			t.Errorf("%s: %d %s", q, code, b)
		}
	}

	// NUL and invalid UTF-8 are cleaned, not a 500.
	code, b := h.agentEvent(agentToken, a.Id, `{"type":"tool_result","tool":"sh\u0000","output":{"stdout":"ok\u0000\ud800"}}`)
	if code != 201 || strings.Contains(string(b), `\u0000`) {
		t.Fatalf("NUL event: %d %s", code, b)
	}
}

func TestAgentSessionFull(t *testing.T) {
	h := newHarness(t, func(h *harness, _ *runs.Config, d *Deps) { d.Agents = fullLog{agentlog.New(h.store)} })
	a := h.createSession(agentToken, apiv1.CreateAgentSessionRequest{Kind: apiv1.AgentSessionKindPlan, Title: "full"})
	if code, b := h.agentEvent(agentToken, a.Id, map[string]any{"type": "message"}); code != 409 || errCode(t, b) != "session_full" {
		t.Fatalf("full session: %d %s", code, b)
	}
}

func TestIdleAgentSessionStreamEnds(t *testing.T) {
	var log *agentlog.Service
	h := newHarness(t, func(h *harness, _ *runs.Config, d *Deps) {
		log = agentlog.New(h.store)
		d.Agents = log
	})
	a := h.createSession(agentToken, apiv1.CreateAgentSessionRequest{Kind: apiv1.AgentSessionKindPlan, Title: "abandoned"})
	msgs := make(chan []sseMsg, 1)
	go func() { msgs <- h.agentStream(a.Id, "") }()
	time.Sleep(50 * time.Millisecond)
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go log.RunSweeper(ctx, 100*time.Millisecond, slog.New(slog.DiscardHandler))

	var got []sseMsg
	select {
	case got = <-msgs:
	case <-time.After(5 * time.Second):
		t.Fatal("the stream of an idle session did not end")
	}
	if n := names(got); len(got) != 2 || n[0] != "snapshot" || n[1] != "snapshot" {
		t.Fatalf("stream: %v", n)
	}
	final := decode[apiv1.AgentSession](t, []byte(got[1].data))
	if final.Status != apiv1.AgentSessionStatusCancelled || deref(final.Summary) != "no activity for 100ms; closed by the platform" {
		t.Fatalf("final snapshot: %+v", final)
	}
	if code, b := h.agentEvent(agentToken, a.Id, map[string]any{"type": "message"}); code != 409 || errCode(t, b) != "agent_session_finished" {
		t.Fatalf("event after the sweep: %d %s", code, b)
	}
}

func TestAgentStreamReplaysInPages(t *testing.T) {
	var log *agentlog.Service
	h := newHarness(t, func(h *harness, _ *runs.Config, d *Deps) {
		log = agentlog.New(h.store)
		d.Agents = log
	})
	a := h.createSession(agentToken, apiv1.CreateAgentSessionRequest{Kind: apiv1.AgentSessionKindPlan, Title: "long"})
	ai := auth.Principal{Name: "ai", Role: apiv1.RoleAgent}
	n := 2*agentlog.DefaultEventPage + 7
	for range n {
		if _, err := log.Append(t.Context(), ai, a.Id, agentlog.EventInput{Type: apiv1.AgentEventTypeMessage}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := log.Finish(t.Context(), ai, a.Id, apiv1.FinishAgentSessionRequest{Status: apiv1.AgentSessionStatusSucceeded}); err != nil {
		t.Fatal(err)
	}
	got := h.agentStream(a.Id, "")
	if len(got) != n+2 || got[0].event != "snapshot" || got[len(got)-1].event != "snapshot" {
		t.Fatalf("%d messages for %d events", len(got), n)
	}
	for i := 2; i <= n; i++ {
		if got[i].id <= got[i-1].id {
			t.Fatalf("message %d has id %d after %d", i, got[i].id, got[i-1].id)
		}
	}
}
