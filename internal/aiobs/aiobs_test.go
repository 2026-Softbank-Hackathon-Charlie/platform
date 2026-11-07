package aiobs

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 7, 7, 38, 15, 0, time.UTC)

func TestParseRealLines(t *testing.T) {
	e, ok := Parse(t0, `message → {"model":"claude-sonnet-5-5","stopReason":"tool_use","toolCalls":["get_cnpg_cluster","list_storage_classes","node_capacity","get_app"]} [토큰 5543/203]`)
	if !ok || e.Kind != KindModel || e.Model != "claude-sonnet-5-5" || e.StopReason != "tool_use" || len(e.ToolCalls) != 4 || e.ToolCalls[3] != "get_app" || e.TokensIn != 5543 || e.TokensOut != 203 {
		t.Fatalf("model tool_use: %+v ok=%v", e, ok)
	}
	e, ok = Parse(t0, `message → {"model":"claude-sonnet-5-5","stopReason":"end_turn","text":"{\"plan\":{\"id\":\"x\"…"} [토큰 9406/2375]`)
	if !ok || e.StopReason != "end_turn" || len(e.ToolCalls) != 0 || e.TokensIn != 9406 {
		t.Fatalf("model end_turn: %+v ok=%v", e, ok)
	}
	e, ok = Parse(t0, `tool_call sandbox_create {"attempt":1,"cluster":"eks"}`)
	if !ok || e.Kind != KindToolCall || e.Tool != "sandbox_create" {
		t.Fatalf("tool_call: %+v", e)
	}
	e, ok = Parse(t0, `tool_result get_cnpg_cluster (ok) → {"name":"moimroom-mig-db"}`)
	if !ok || e.Kind != KindToolDone || e.Tool != "get_cnpg_cluster" || e.Outcome != "ok" {
		t.Fatalf("tool_result: %+v", e)
	}
	e, ok = Parse(t0, `blocked curl (expected) {"command":"curl http://169.254.169.254/"} → "NetworkPolicy"`)
	if !ok || e.Kind != KindBlocked || e.Tool != "curl" {
		t.Fatalf("blocked: %+v", e)
	}
	e, ok = Parse(t0, `리허설 1회차 (sandbox-moimroom-mig-25bb): 통과=true 엔진=succeeded 102초`)
	if !ok || e.Kind != KindRehearsal || e.Passed == nil || !*e.Passed || e.Seconds != 102 || e.Outcome != "succeeded" {
		t.Fatalf("rehearsal: %+v", e)
	}
}

func TestParseIgnoresOtherLinesAndNeverKeepsThePayload(t *testing.T) {
	for _, body := range []string{
		`이벤트 tool_call engine.prepare: Post "http://hopcharlie-api:8080/…": connect: connection refused`,
		`message sandbox_prewarm → "AI가 계획하는 동안 미리 준비한 샌드박스 사용"`,
		``,
	} {
		if e, ok := Parse(t0, body); ok {
			t.Errorf("%q parsed as %+v", body, e)
		}
	}
	e, _ := Parse(t0, `tool_result get_cnpg_cluster (ok) → {"secret":"hunter2"}`)
	if strings.Contains(strings.ToLower(e.Tool+e.Outcome), "hunter2") {
		t.Fatal("the payload leaked")
	}
}

func TestNewChecksSettings(t *testing.T) {
	for _, u := range []string{"", "clickhouse:8123", "ftp://h", "http://u:p@h:8123", "http://h:8123/?a=b"} {
		if _, err := New(u, "", "", ""); err == nil {
			t.Errorf("%q accepted", u)
		}
	}
	for _, tb := range []string{"a b", "x;DROP TABLE y", "a.b.c", "t`"} {
		if _, err := New("http://h:8123", "", "", tb); err == nil {
			t.Errorf("table %q accepted", tb)
		}
	}
	if c, err := New("http://h:8123/", "u", "p", ""); err != nil || c.Table != "default.otel_logs" || c.URL != "http://h:8123" {
		t.Fatalf("%+v %v", c, err)
	}
}

func TestEventsQueryAndOrder(t *testing.T) {
	var gotQuery, gotUser string
	gotParams := map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotQuery = string(b)
		gotUser, _, _ = r.BasicAuth()
		for k := range r.URL.Query() {
			gotParams[k] = r.URL.Query().Get(k)
		}
		// newest first, as the query asks
		io.WriteString(w, `{"ts":"1794037095000","Body":"리허설 1회차 (s): 통과=true 엔진=succeeded 102초"}`+"\n"+
			`{"ts":"1794037000000","Body":"unrelated line"}`+"\n"+
			`{"ts":"1794036995000","Body":"tool_call node_capacity {}"}`+"\n")
	}))
	defer srv.Close()
	c, err := New(srv.URL, "reader", "pw", "default.otel_logs")
	if err != nil {
		t.Fatal(err)
	}
	ev, err := c.Events(context.Background(), time.UnixMilli(1794030000000), 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(ev) != 2 || ev[0].Kind != KindToolCall || ev[1].Kind != KindRehearsal || !ev[0].Time.Before(ev[1].Time) {
		t.Fatalf("events: %+v", ev)
	}
	if gotUser != "reader" || gotParams["readonly"] != "1" || gotParams["param_svc"] != Service || gotParams["param_since"] != "1794030000000" || gotParams["param_limit"] != "50" {
		t.Fatalf("request: user=%q params=%v", gotUser, gotParams)
	}
	if !strings.Contains(gotQuery, "FROM default.otel_logs WHERE ServiceName = {svc:String}") || strings.Contains(gotQuery, Service) {
		t.Fatalf("query must bind the service name as a parameter: %s", gotQuery)
	}
	s := Summarize(ev)
	if s.Rehearsals != 1 || s.Tools["node_capacity"] != 1 || s.LastEvent == nil {
		t.Fatalf("summary: %+v", s)
	}
}

func TestEventsErrorsDoNotEchoTheBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, "Code: 497. DB::Exception: reader: Not enough privileges. SELECT … password=hunter2")
	}))
	defer srv.Close()
	c, _ := New(srv.URL, "reader", "pw", "")
	_, err := c.Events(context.Background(), time.Now().Add(-time.Hour), 10)
	if err == nil || strings.Contains(err.Error(), "hunter2") || !strings.Contains(err.Error(), "403") {
		t.Fatalf("err = %v", err)
	}
}
