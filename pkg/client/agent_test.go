package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

const (
	sessionJSON = `{"id":"ags_1","kind":"plan","title":"plan gke -> eks","actor":"planner","status":"running","createdAt":"2026-11-02T09:00:00Z","totals":{"events":2,"toolCalls":1,"inputTokens":10,"outputTokens":2,"durationMillis":40}}`
	eventJSON   = `{"seq":7,"sessionId":"ags_1","time":"2026-11-02T09:00:01Z","type":"tool_call","tool":"list_envs","input":{"env":"gke"}}`
	reportJSON  = `{"generatedAt":"2026-11-02T09:00:00Z","summary":{"bySeverity":{"total":1,"critical":0,"high":1,"medium":0,"low":0,"info":0},"byEnvironment":{"gke":{"total":1,"critical":0,"high":1,"medium":0,"low":0,"info":0}}},"sources":[{"name":"kubernetes","provider":"gcp","environment":"gke","status":"ok","checkedAt":"2026-11-02T09:00:00Z","findings":1}],"findings":[{"id":"sf_1","environment":"gke","provider":"gcp","source":"kubernetes","check":"k8s.privileged","category":"workload","severity":"high","title":"Privileged container","resource":"shop/Deployment/web/app","detail":"d","recommendation":"r","observedAt":"2026-11-02T09:00:00Z"}]}`
)

func TestSecurityAndAgentOperations(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name        string
		call        func(*Client) (any, error)
		method      string
		path, query string
		body        string
		status      int
		resp        string
		got         func(any) string
		want        string
	}{
		{name: "GetSecurity", call: func(c *Client) (any, error) { return c.GetSecurity(ctx, SecurityOptions{}) },
			method: "GET", path: "/v1/security", resp: reportJSON,
			got: func(v any) string { return v.(*apiv1.SecurityReport).Findings[0].Title }, want: "Privileged container"},
		{name: "GetSecurity filters", call: func(c *Client) (any, error) {
			return c.GetSecurity(ctx, SecurityOptions{Environment: "gke", Severity: apiv1.SecuritySeverityHigh, Refresh: true})
		},
			method: "GET", path: "/v1/security", query: "environment=gke&refresh=true&severity=high", resp: reportJSON,
			got: func(v any) string { return fmt.Sprint(v.(*apiv1.SecurityReport).Summary.BySeverity.High) }, want: "1"},
		{name: "CreateAgentSession", call: func(c *Client) (any, error) {
			return c.CreateAgentSession(ctx, apiv1.CreateAgentSessionRequest{Kind: apiv1.AgentSessionKindPlan, Title: "plan gke -> eks"})
		},
			method: "POST", path: "/v1/agent/sessions", body: `{"kind":"plan","title":"plan gke -> eks"}`, status: 201, resp: sessionJSON,
			got: func(v any) string { return v.(*apiv1.AgentSession).Id }, want: "ags_1"},
		{name: "AppendAgentEvent", call: func(c *Client) (any, error) {
			return c.AppendAgentEvent(ctx, "ags_1", apiv1.AgentEventRequest{Type: apiv1.AgentEventTypeToolCall, Tool: ptr("list_envs"), Input: map[string]any{"env": "gke"}})
		},
			method: "POST", path: "/v1/agent/sessions/ags_1/events", body: `{"type":"tool_call","tool":"list_envs","input":{"env":"gke"}}`, status: 201, resp: eventJSON,
			got: func(v any) string { return fmt.Sprint(v.(*apiv1.AgentEvent).Seq) }, want: "7"},
		{name: "FinishAgentSession", call: func(c *Client) (any, error) {
			return c.FinishAgentSession(ctx, "ags_1", apiv1.FinishAgentSessionRequest{Status: apiv1.AgentSessionStatusSucceeded})
		},
			method: "POST", path: "/v1/agent/sessions/ags_1/finish", body: `{"status":"succeeded"}`, resp: sessionJSON,
			got: func(v any) string { return v.(*apiv1.AgentSession).Title }, want: "plan gke -> eks"},
		{name: "ListAgentSessions", call: func(c *Client) (any, error) {
			return c.ListAgentSessions(ctx, AgentSessionFilter{PlanRef: "pln_1", RunID: "run_1", Limit: 5})
		},
			method: "GET", path: "/v1/agent/sessions", query: "limit=5&planRef=pln_1&runId=run_1", resp: `{"sessions":[` + sessionJSON + `]}`,
			got: func(v any) string { return fmt.Sprint(v.(*apiv1.AgentSessionList).Sessions[0].Totals.ToolCalls) }, want: "1"},
		{name: "GetAgentSession", call: func(c *Client) (any, error) { return c.GetAgentSession(ctx, "ags_1") },
			method: "GET", path: "/v1/agent/sessions/ags_1", resp: sessionJSON,
			got: func(v any) string { return v.(*apiv1.AgentSession).Actor }, want: "planner"},
		{name: "ListAgentEvents", call: func(c *Client) (any, error) { return c.ListAgentEvents(ctx, "ags_1", 3) },
			method: "GET", path: "/v1/agent/sessions/ags_1/events", query: "after=3&format=json&limit=2000", resp: `{"events":[` + eventJSON + `]}`,
			got: func(v any) string { return *v.(*apiv1.AgentEventList).Events[0].Tool }, want: "list_envs"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status := tc.status
			if status == 0 {
				status = 200
			}
			c, s := fakeAPI(t, status, tc.resp)
			v, err := tc.call(c)
			if err != nil {
				t.Fatal(err)
			}
			if s.method != tc.method || s.path != tc.path || s.query != tc.query {
				t.Errorf("request %s %s?%s, want %s %s?%s", s.method, s.path, s.query, tc.method, tc.path, tc.query)
			}
			if tc.body != "" && !jsonEqual(t, s.body, []byte(tc.body)) {
				t.Errorf("body %s, want %s", s.body, tc.body)
			}
			if got := tc.got(v); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func ptr[T any](v T) *T { return &v }

func writeSessionSnapshot(w io.Writer, status string) {
	fmt.Fprintf(w, "event: snapshot\ndata: %s\n\n", strings.Replace(sessionJSON, `"status":"running"`, `"status":"`+status+`"`, 1))
	w.(http.Flusher).Flush()
}

func writeAgentEvent(w io.Writer, seq int64, typ string) {
	fmt.Fprintf(w, "event: %s\nid: %d\ndata: {\"seq\":%d,\"sessionId\":\"ags_1\",\"time\":\"2026-11-02T09:00:00Z\",\"type\":%q,\"tool\":\"t%d\"}\n\n", typ, seq, seq, typ, seq)
	w.(http.Flusher).Flush()
}

func TestWatchAgentSessionReconnectsWithoutDuplicates(t *testing.T) {
	c, s := newSSEServer(t, func(w http.ResponseWriter, r *http.Request, conn int) {
		switch conn {
		case 0: // the stream drops while the session runs
			writeSessionSnapshot(w, "running")
			writeAgentEvent(w, 1, "tool_call")
			writeAgentEvent(w, 2, "tool_result")
			abort()
		case 1: // replays more than asked, then the session finishes
			writeSessionSnapshot(w, "running")
			writeAgentEvent(w, 2, "tool_result")
			writeAgentEvent(w, 3, "decision")
			writeSessionSnapshot(w, "succeeded")
		default:
			t.Error("reconnected after the session finished")
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var names []string
	var seqs []int64
	err := c.WatchAgentSession(ctx, "ags_1", func(m AgentMessage) error {
		names = append(names, m.Name)
		if m.Event != nil {
			seqs = append(seqs, m.Seq)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := []int64{1, 2, 3}; !reflect.DeepEqual(seqs, want) {
		t.Errorf("seqs = %v, want %v", seqs, want)
	}
	if want := "snapshot,tool_call,tool_result,snapshot,decision,snapshot"; strings.Join(names, ",") != want {
		t.Errorf("messages = %v, want %s", names, want)
	}
	if want := []string{"", "2"}; !reflect.DeepEqual(s.lastEventIDs(), want) {
		t.Errorf("Last-Event-ID per connection = %q, want %q", s.lastEventIDs(), want)
	}
	if !strings.HasSuffix(s.paths[0], "/v1/agent/sessions/ags_1/events") {
		t.Errorf("path %s", s.paths[0])
	}
}

func TestWatchAgentSessionErrors(t *testing.T) {
	c, _ := fakeAPI(t, 404, `{"error":{"code":"agent_session_not_found","message":"agent session not found"}}`)
	err := c.WatchAgentSession(context.Background(), "ags_x", func(AgentMessage) error { return nil })
	if ErrorCode(err) != "agent_session_not_found" {
		t.Fatalf("unknown session: %v", err)
	}
	c, _ = newSSEServer(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		fmt.Fprint(w, "event: snapshot\ndata: {not json}\n\n")
	})
	err = c.WatchAgentSession(context.Background(), "ags_1", func(AgentMessage) error { return nil })
	var pe *protocolError
	if !errors.As(err, &pe) || !strings.Contains(err.Error(), "decode agent session snapshot") {
		t.Fatalf("bad snapshot: %v", err)
	}
}

func TestListAgentEventsFollowsPages(t *testing.T) {
	var queries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.RawQuery)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Query().Get("after") {
		case "":
			fmt.Fprint(w, `{"events":[{"seq":1,"sessionId":"ags_1","time":"2026-11-02T09:00:00Z","type":"message"},{"seq":4,"sessionId":"ags_1","time":"2026-11-02T09:00:00Z","type":"message"}],"nextAfter":4}`)
		case "4":
			fmt.Fprint(w, `{"events":[{"seq":9,"sessionId":"ags_1","time":"2026-11-02T09:00:00Z","type":"message"}]}`)
		case "9":
			fmt.Fprint(w, `{"events":[],"nextAfter":9}`) // a server that points back
		}
	}))
	t.Cleanup(srv.Close)
	c := New(srv.URL+"/", "tok")
	l, err := c.ListAgentEvents(context.Background(), "ags_1", 0)
	if err != nil {
		t.Fatal(err)
	}
	var seqs []int64
	for _, e := range l.Events {
		seqs = append(seqs, e.Seq)
	}
	if fmt.Sprint(seqs) != "[1 4 9]" || fmt.Sprint(queries) != "[format=json&limit=2000 after=4&format=json&limit=2000]" {
		t.Fatalf("seqs %v, queries %v", seqs, queries)
	}
	var pe *protocolError
	if _, err := c.ListAgentEvents(context.Background(), "ags_1", 9); !errors.As(err, &pe) {
		t.Fatalf("a page pointing back: %v", err)
	}
}
