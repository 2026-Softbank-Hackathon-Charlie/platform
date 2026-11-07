// Package aiobs reads what the AI runner (ai-sandbox hopai) did from the
// observability stack's ClickHouse (the ClickStack/HyperDX store), so the
// console can draw the real calls: which model was asked, which read-only
// tools it requested, the sandbox steps and the rehearsal result.
//
// It is read-only and answers parsed fields only. The raw log line (which can
// carry cluster specs) never leaves this package.
package aiobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Service is the OpenTelemetry service name hopai's Job logs under.
const Service = "charlie-platform-hopai"

// Event kinds.
const (
	KindModel     = "model"     // one answer of the model (stopReason tool_use or end_turn)
	KindToolCall  = "tool_call" // hopai ran a tool
	KindToolDone  = "tool_result"
	KindBlocked   = "blocked"   // a sandbox isolation check that was refused, as expected
	KindRehearsal = "rehearsal" // the result of a rehearsal
)

// Event is one parsed log line. Fields that do not apply are zero.
type Event struct {
	Time       time.Time `json:"time"`
	Kind       string    `json:"kind"`
	Tool       string    `json:"tool,omitempty"`
	Outcome    string    `json:"outcome,omitempty"`
	Model      string    `json:"model,omitempty"`
	StopReason string    `json:"stopReason,omitempty"`
	ToolCalls  []string  `json:"toolCalls,omitempty"`
	TokensIn   int       `json:"tokensIn,omitempty"`
	TokensOut  int       `json:"tokensOut,omitempty"`
	Passed     *bool     `json:"passed,omitempty"`
	Seconds    int       `json:"seconds,omitempty"`
}

var (
	reModel     = regexp.MustCompile(`^message → \{"model":"([^"]+)","stopReason":"([^"]+)"(?:,"toolCalls":\[([^\]]*)\])?.*?\[토큰 (\d+)/(\d+)\]\s*$`)
	reToolCall  = regexp.MustCompile(`^tool_call ([A-Za-z0-9_.-]+)`)
	reToolDone  = regexp.MustCompile(`^tool_result ([A-Za-z0-9_.-]+) \(([^)]*)\)`)
	reBlocked   = regexp.MustCompile(`^blocked ([A-Za-z0-9_.-]+)`)
	reRehearsal = regexp.MustCompile(`^리허설 \d+회차 \([^)]*\): 통과=(true|false) 엔진=(\S+) (\d+)초`)
)

// Parse turns one log body into an Event. ok is false for lines that are not
// one of the kinds above (for example the echo of a failed session record).
func Parse(t time.Time, body string) (Event, bool) {
	body = strings.TrimSpace(body)
	if m := reModel.FindStringSubmatch(body); m != nil {
		e := Event{Time: t, Kind: KindModel, Model: m[1], StopReason: m[2]}
		for _, name := range strings.Split(m[3], ",") {
			if name = strings.Trim(strings.TrimSpace(name), `"`); name != "" {
				e.ToolCalls = append(e.ToolCalls, name)
			}
		}
		e.TokensIn, _ = strconv.Atoi(m[4])
		e.TokensOut, _ = strconv.Atoi(m[5])
		return e, true
	}
	if m := reToolCall.FindStringSubmatch(body); m != nil {
		return Event{Time: t, Kind: KindToolCall, Tool: m[1]}, true
	}
	if m := reToolDone.FindStringSubmatch(body); m != nil {
		return Event{Time: t, Kind: KindToolDone, Tool: m[1], Outcome: m[2]}, true
	}
	if m := reBlocked.FindStringSubmatch(body); m != nil {
		return Event{Time: t, Kind: KindBlocked, Tool: m[1]}, true
	}
	if m := reRehearsal.FindStringSubmatch(body); m != nil {
		passed := m[1] == "true"
		secs, _ := strconv.Atoi(m[3])
		return Event{Time: t, Kind: KindRehearsal, Passed: &passed, Outcome: m[2], Seconds: secs}, true
	}
	return Event{}, false
}

// Client reads hopai's log lines over ClickHouse's HTTP interface.
type Client struct {
	URL      string // e.g. http://clickstack-clickhouse-clickhouse-headless.observability.svc:8123
	User     string
	Password string
	Table    string // default.otel_logs
	HTTP     *http.Client
}

var tableName = regexp.MustCompile(`^[A-Za-z0-9_]+(\.[A-Za-z0-9_]+)?$`)

// New checks the settings. The table name is spliced into the query (it cannot
// be a parameter), so it is limited to database.table identifiers.
func New(rawURL, user, password, table string) (*Client, error) {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" {
		return nil, errors.New("aiobs: the ClickHouse address must be an http(s) URL without credentials or a query")
	}
	if table == "" {
		table = "default.otel_logs"
	}
	if !tableName.MatchString(table) {
		return nil, fmt.Errorf("aiobs: %q is not a table name", table)
	}
	return &Client{URL: strings.TrimRight(rawURL, "/"), User: user, Password: password, Table: table,
		HTTP: &http.Client{Timeout: 10 * time.Second}}, nil
}

const maxRows = 2000

// do runs one query (the parameters are bound, never spliced) and returns the
// response body. The settings keep a query short and its answer small.
func (c *Client) do(ctx context.Context, q string, v url.Values) ([]byte, error) {
	v.Set("max_execution_time", "5")
	v.Set("max_result_rows", strconv.Itoa(maxRows))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL+"/?"+v.Encode(), strings.NewReader(q))
	if err != nil {
		return nil, err
	}
	if c.User != "" {
		req.SetBasicAuth(c.User, c.Password)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("aiobs: query: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, fmt.Errorf("aiobs: read: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		// The body can echo the query; keep only the status.
		return nil, fmt.Errorf("aiobs: ClickHouse answered %d", resp.StatusCode)
	}
	return body, nil
}

// Events returns hopai's parsed events since the given time, oldest first,
// at most limit of the newest (capped at 2000).
func (c *Client) Events(ctx context.Context, since time.Time, limit int) ([]Event, error) {
	if limit < 1 || limit > maxRows {
		limit = maxRows
	}
	q := "SELECT toString(toUnixTimestamp64Milli(Timestamp)) AS ts, Body FROM " + c.Table +
		" WHERE ServiceName = {svc:String} AND Timestamp >= fromUnixTimestamp64Milli({since:Int64})" +
		" ORDER BY Timestamp DESC LIMIT {limit:UInt32} FORMAT JSONEachRow"
	v := url.Values{}
	v.Set("param_svc", Service)
	v.Set("param_since", strconv.FormatInt(since.UnixMilli(), 10))
	v.Set("param_limit", strconv.Itoa(limit))
	body, err := c.do(ctx, q, v)
	if err != nil {
		return nil, err
	}
	var out []Event
	for _, line := range strings.Split(string(body), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var row struct {
			TS   string `json:"ts"`
			Body string `json:"Body"`
		}
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			return nil, fmt.Errorf("aiobs: bad row: %w", err)
		}
		ms, err := strconv.ParseInt(row.TS, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("aiobs: bad timestamp %q", row.TS)
		}
		if e, ok := Parse(time.UnixMilli(ms).UTC(), row.Body); ok {
			out = append(out, e)
		}
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 { // the query is newest first
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// Summary counts what the events show.
type Summary struct {
	ModelCalls int            `json:"modelCalls"`
	Model      string         `json:"model,omitempty"`
	TokensIn   int            `json:"tokensIn"`
	TokensOut  int            `json:"tokensOut"`
	Tools      map[string]int `json:"tools"`
	Blocked    int            `json:"blocked"`
	Rehearsals int            `json:"rehearsals"`
	LastEvent  *time.Time     `json:"lastEvent,omitempty"`
}

// Summarize totals the events.
func Summarize(events []Event) Summary {
	s := Summary{Tools: map[string]int{}}
	for _, e := range events {
		switch e.Kind {
		case KindModel:
			s.ModelCalls++
			s.Model = e.Model
			s.TokensIn += e.TokensIn
			s.TokensOut += e.TokensOut
		case KindToolCall:
			s.Tools[e.Tool]++
		case KindBlocked:
			s.Blocked++
		case KindRehearsal:
			s.Rehearsals++
		}
		t := e.Time
		s.LastEvent = &t
	}
	return s
}
