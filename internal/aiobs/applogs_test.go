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

func TestRedactHidesSecretsAndPeople(t *testing.T) {
	cases := []struct{ in, hidden string }{
		{`connect postgres://app:hunter2@db:5432/app failed`, "hunter2"},
		{`Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.abc.def`, "eyJhbGci"},
		{`login failed password=hunter2 user=kim`, "hunter2"},
		{`{"api_key":"sk-live-abc123","x":1}`, "sk-live-abc123"},
		{`SECRET_TOKEN: abcd1234efgh`, "abcd1234efgh"},
		{`mail to kim.minsu@example.com bounced`, "kim.minsu@example.com"},
		{`key AKIAIOSFODNN7EXAMPLE rejected`, "AKIAIOSFODNN7EXAMPLE"},
		{`cookie=session-9f8e7d6c5b4a`, "session-9f8e7d6c5b4a"},
		{`token "abcDEF123456abcDEF123456abcDEF"`, "abcDEF123456abcDEF123456abcDEF"},
	}
	for _, c := range cases {
		got := Redact(c.in)
		if strings.Contains(got, c.hidden) {
			t.Errorf("Redact(%q) = %q still shows %q", c.in, got, c.hidden)
		}
	}
}

func TestRedactKeepsWhatHelpsReading(t *testing.T) {
	for _, in := range []string{
		"connection refused to db:5432",
		"relation \"orders\" does not exist",
		"GET /healthz 500 internal server error",
		"upstream timed out while reading response header",
	} {
		if got := Redact(in); got != in {
			t.Errorf("Redact(%q) = %q, want it unchanged", in, got)
		}
	}
}

func TestPatternFoldsIdsAndNumbersAndTruncates(t *testing.T) {
	a := Pattern("order 1042 failed for user 7 request 3f2a9c1b-0d4e-4a7b-9c11-2e5f6a7b8c9d")
	b := Pattern("order 2077 failed for user 12 request 11111111-2222-3333-4444-555555555555")
	if a != b || strings.ContainsAny(a, "0123456789") && !strings.Contains(a, "#") {
		t.Fatalf("a = %q, b = %q, want the same failure to fold into one pattern", a, b)
	}
	if got := Pattern(strings.Repeat("가", 400)); len([]rune(got)) != patternLen+1 || !strings.HasSuffix(got, "…") {
		t.Fatalf("len = %d", len([]rune(got)))
	}
	if Pattern("   \n  ") != "" {
		t.Fatal("a blank line is not a pattern")
	}
}

// fakeClickHouse answers the three queries App makes, told apart by what they select.
func fakeClickHouse(t *testing.T, sawParams *map[string]string, sawQueries *[]string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		q := string(b)
		*sawQueries = append(*sawQueries, q)
		*sawParams = map[string]string{}
		for k := range r.URL.Query() {
			(*sawParams)[k] = r.URL.Query().Get(k)
		}
		switch {
		case strings.Contains(q, "GROUP BY ServiceName"):
			io.WriteString(w, `{"name":"docker-doom-web","total":"900","errors":"3","warnings":"10","last":"1794037095000"}`+"\n"+
				`{"name":"docker-doom-db","total":"300","errors":"0","warnings":"2","last":"1794037000000"}`+"\n")
		case strings.Contains(q, "GROUP BY t"):
			io.WriteString(w, `{"t":"1794036900","total":"600","errors":"1","warnings":"5"}`+"\n"+`{"t":"1794037200","total":"600","errors":"2","warnings":"7"}`+"\n")
		default:
			io.WriteString(w,
				`{"ts":"1794037095000","svc":"docker-doom-web","sev":"error","body":"db error password=hunter2 order 1042 failed"}`+"\n"+
					`{"ts":"1794037090000","svc":"docker-doom-web","sev":"error","body":"db error password=other order 2077 failed"}`+"\n"+
					`{"ts":"1794037080000","svc":"docker-doom-web","sev":"warn","body":"slow query 812 ms"}`+"\n"+
					`{"ts":"1794037070000","svc":"docker-doom-web","sev":"error","body":"timeout talking to upstream"}`+"\n")
		}
	}))
}

func TestAppSummarisesServicesBucketsAndPatterns(t *testing.T) {
	var params map[string]string
	var queries []string
	srv := fakeClickHouse(t, &params, &queries)
	defer srv.Close()
	c, err := New(srv.URL, "reader", "pw", "")
	if err != nil {
		t.Fatal(err)
	}
	o, err := c.App(context.Background(), "docker-doom", time.UnixMilli(1794030000000))
	if err != nil {
		t.Fatal(err)
	}
	if o.Total != 1200 || o.Errors != 3 || o.Warnings != 12 || len(o.Services) != 2 || o.Services[0].Name != "docker-doom-web" || o.Services[0].Total != 900 {
		t.Fatalf("totals: %+v", o)
	}
	if len(o.Buckets) != 2 || o.Buckets[0].Total != 600 || !o.Buckets[0].Time.Before(o.Buckets[1].Time) {
		t.Fatalf("buckets: %+v", o.Buckets)
	}
	// two errors with different ids and passwords are one pattern with a count of 2, and errors come before warnings
	if len(o.Patterns) != 3 || o.Patterns[0].Severity != "error" || o.Patterns[0].Count != 2 || !strings.Contains(o.Patterns[0].Text, "order # failed") {
		t.Fatalf("patterns: %+v", o.Patterns)
	}
	for _, p := range o.Patterns {
		if strings.Contains(p.Text, "hunter2") || strings.Contains(p.Text, "other") {
			t.Fatalf("a secret reached the answer: %+v", p)
		}
	}
	if o.Patterns[2].Severity != "warning" {
		t.Fatalf("a warning comes last: %+v", o.Patterns)
	}
	if params["param_app"] != "docker-doom" || params["param_prefix"] != "docker-doom-" || params["param_since"] != "1794030000000" {
		t.Fatalf("params: %v", params)
	}
	for _, q := range queries {
		if strings.Contains(q, "docker-doom") || strings.Contains(q, "hunter2") {
			t.Fatalf("the app name must be bound as a parameter, not spliced: %s", q)
		}
	}
	if len(queries) != 3 {
		t.Fatalf("queries = %d", len(queries))
	}
}

func TestAppSkipsThePatternQueryWhenThereAreNoErrors(t *testing.T) {
	var queries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		queries = append(queries, string(b))
		if strings.Contains(string(b), "GROUP BY ServiceName") {
			io.WriteString(w, `{"name":"pxls-web","total":"50","errors":"0","warnings":"0","last":"1794037095000"}`+"\n")
		}
	}))
	defer srv.Close()
	c, _ := New(srv.URL, "", "", "")
	o, err := c.App(context.Background(), "pxls", time.Now().Add(-time.Hour))
	if err != nil || o.Total != 50 || len(o.Patterns) != 0 || len(queries) != 2 {
		t.Fatalf("%+v %v %d", o, err, len(queries))
	}
}

func TestAppRefusesOtherNamesAndHidesErrors(t *testing.T) {
	c, _ := New("http://127.0.0.1:1", "", "", "")
	for _, name := range []string{"", "Docker", "a b", "a'; DROP", "-x", "../x", strings.Repeat("a", 64)} {
		if _, err := c.App(context.Background(), name, time.Now()); err == nil {
			t.Errorf("%q accepted", name)
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, "SELECT … password=hunter2")
	}))
	defer srv.Close()
	c, _ = New(srv.URL, "", "", "")
	_, err := c.App(context.Background(), "pxls", time.Now())
	if err == nil || strings.Contains(err.Error(), "hunter2") || !strings.Contains(err.Error(), "500") {
		t.Fatalf("err = %v", err)
	}
}
