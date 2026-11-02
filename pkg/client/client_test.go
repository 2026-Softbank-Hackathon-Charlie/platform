package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

const (
	planJSON = `{"ref":"pln_1","status":"approved","revision":1,"summary":{"planId":"p1","source":{"env":"gke","namespace":"shop","cluster":"pg"},"target":{"env":"eks","namespace":"shop","cluster":"pg"},"files":true,"gatewayMode":"http","fenceGate":true,"maxDowntime":"3m"},"submittedBy":"planner","submittedAt":"2026-11-02T09:00:00Z"}`
	runJSON  = `{"runId":"run_1","planRef":"pln_1","planId":"p1","source":{"env":"gke","namespace":"shop","cluster":"pg"},"target":{"env":"eks","namespace":"shop","cluster":"pg"},"status":"running","phase":"sync","autoSwitch":false,"startedBy":"op","startedAt":"2026-11-02T09:00:00Z","updatedAt":"2026-11-02T09:00:00Z","result":{},"engine":{"runId":"run_1","planId":"p1","createdAt":"2026-11-02T09:00:00Z","updatedAt":"2026-11-02T09:00:00Z","outcome":"running","steps":{},"writesHeld":false,"sourceDemoted":false,"promoteRequested":false,"trafficOnTarget":false}}`
	envJSON  = `{"name":"gke","provider":"gcp","displayName":"GCP GKE","gatewayBackend":"gke","live":true,"status":{"reachable":true,"checkedAt":"2026-11-02T09:00:00Z"}}`
)

type seen struct {
	method, path, query string
	header              http.Header
	body                []byte
}

// fakeAPI answers every request with status and body and records the request.
func fakeAPI(t *testing.T, status int, body string) (*Client, *seen) {
	t.Helper()
	s := &seen{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.method, s.path, s.query, s.header = r.Method, r.URL.EscapedPath(), r.URL.RawQuery, r.Header.Clone()
		s.body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL+"/", "tok", WithUserAgent("test-agent/1")), s
}

func TestOperations(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name        string
		call        func(*Client) (any, error)
		method      string
		path, query string
		body        string // expected JSON request body; "" means none
		header      map[string]string
		noAuth      bool
		status      int
		resp        string
		got         func(any) string
		want        string
	}{
		{name: "Health", call: func(c *Client) (any, error) { return c.Health(ctx) },
			method: "GET", path: "/healthz", noAuth: true, resp: `{"status":"ok"}`,
			got: func(v any) string { return v.(*apiv1.Health).Status }, want: "ok"},
		{name: "Me", call: func(c *Client) (any, error) { return c.Me(ctx) },
			method: "GET", path: "/v1/me", resp: `{"name":"alice","role":"operator"}`,
			got: func(v any) string { return string(v.(*apiv1.Principal).Role) }, want: string(apiv1.RoleOperator)},
		{name: "SubmitPlan", call: func(c *Client) (any, error) { return c.SubmitPlan(ctx, []byte(`{"id":"p1"}`)) },
			method: "POST", path: "/v1/plans", body: `{"id":"p1"}`, status: 201, resp: planJSON,
			got: func(v any) string { return v.(*apiv1.PlanRecord).Ref }, want: "pln_1"},
		{name: "ListPlans", call: func(c *Client) (any, error) { return c.ListPlans(ctx) },
			method: "GET", path: "/v1/plans", resp: `{"plans":[` + planJSON + `]}`,
			got: func(v any) string { return v.(*apiv1.PlanList).Plans[0].Summary.Target.Env }, want: "eks"},
		{name: "GetPlan escapes", call: func(c *Client) (any, error) { return c.GetPlan(ctx, "pln/../x y?") },
			method: "GET", path: "/v1/plans/pln%2F..%2Fx%20y%3F", resp: planJSON,
			got: func(v any) string { return string(v.(*apiv1.PlanRecord).Status) }, want: "approved"},
		{name: "ApprovePlan", call: func(c *Client) (any, error) { return c.ApprovePlan(ctx, "pln_1", "looks good", 3) },
			method: "POST", path: "/v1/plans/pln_1/approve", body: `{"comment":"looks good","revision":3}`, resp: planJSON,
			got: func(v any) string { return v.(*apiv1.PlanRecord).Ref }, want: "pln_1"},
		{name: "ApprovePlan no comment", call: func(c *Client) (any, error) { return c.ApprovePlan(ctx, "pln_1", "", 0) },
			method: "POST", path: "/v1/plans/pln_1/approve", body: `{}`, resp: planJSON,
			got: func(v any) string { return v.(*apiv1.PlanRecord).Ref }, want: "pln_1"},
		{name: "RejectPlan", call: func(c *Client) (any, error) { return c.RejectPlan(ctx, "pln_1", "wrong target") },
			method: "POST", path: "/v1/plans/pln_1/reject", body: `{"comment":"wrong target"}`, resp: planJSON,
			got: func(v any) string { return v.(*apiv1.PlanRecord).Ref }, want: "pln_1"},
		{name: "StartMigration", call: func(c *Client) (any, error) {
			auto := true
			return c.StartMigration(ctx, apiv1.StartMigrationRequest{PlanRef: "pln_1", AutoSwitch: &auto,
				Canary: &apiv1.CanaryOptions{Weights: []int{10, 50}}}, "key-1")
		},
			method: "POST", path: "/v1/migrations", body: `{"planRef":"pln_1","autoSwitch":true,"canary":{"weights":[10,50]}}`,
			header: map[string]string{"Idempotency-Key": "key-1"}, status: 202,
			resp: `{"runId":"run_1","planRef":"pln_1","links":{"self":"/v1/migrations/run_1","events":"/v1/migrations/run_1/events"}}`,
			got:  func(v any) string { return v.(*apiv1.StartMigrationResponse).Links.Events }, want: "/v1/migrations/run_1/events"},
		{name: "StartMigration no key", call: func(c *Client) (any, error) {
			return c.StartMigration(ctx, apiv1.StartMigrationRequest{PlanRef: "pln_1"}, "")
		},
			method: "POST", path: "/v1/migrations", body: `{"planRef":"pln_1"}`, header: map[string]string{"Idempotency-Key": ""}, status: 202,
			resp: `{"runId":"run_1","planRef":"pln_1","links":{"self":"s","events":"e"}}`,
			got:  func(v any) string { return v.(*apiv1.StartMigrationResponse).RunId }, want: "run_1"},
		{name: "ListMigrations", call: func(c *Client) (any, error) { return c.ListMigrations(ctx) },
			method: "GET", path: "/v1/migrations", resp: `{"runs":[` + runJSON + `]}`,
			got: func(v any) string { return v.(*apiv1.RunList).Runs[0].RunId }, want: "run_1"},
		{name: "GetMigration", call: func(c *Client) (any, error) { return c.GetMigration(ctx, "run 1") },
			method: "GET", path: "/v1/migrations/run%201", resp: runJSON,
			got: func(v any) string { return string(v.(*apiv1.Run).Phase) }, want: "sync"},
		{name: "SwitchMigration", call: func(c *Client) (any, error) { return c.SwitchMigration(ctx, "run_1") },
			method: "POST", path: "/v1/migrations/run_1/switch", resp: runJSON,
			got: func(v any) string { return v.(*apiv1.Run).RunId }, want: "run_1"},
		{name: "AbortMigration", call: func(c *Client) (any, error) { return c.AbortMigration(ctx, "run_1", "lag too high") },
			method: "POST", path: "/v1/migrations/run_1/abort", body: `{"reason":"lag too high"}`, resp: runJSON,
			got: func(v any) string { return v.(*apiv1.Run).RunId }, want: "run_1"},
		{name: "ResumeMigration", call: func(c *Client) (any, error) { return c.ResumeMigration(ctx, "run_1") },
			method: "POST", path: "/v1/migrations/run_1/resume", resp: runJSON,
			got: func(v any) string { return v.(*apiv1.Run).RunId }, want: "run_1"},
		{name: "ListEnvironments", call: func(c *Client) (any, error) { return c.ListEnvironments(ctx) },
			method: "GET", path: "/v1/envs", resp: `{"environments":[` + envJSON + `]}`,
			got: func(v any) string { return string(v.(*apiv1.EnvironmentList).Environments[0].Provider) }, want: string(apiv1.ProviderGcp)},
		{name: "GetEnvironment", call: func(c *Client) (any, error) { return c.GetEnvironment(ctx, "on prem") },
			method: "GET", path: "/v1/envs/on%20prem", resp: envJSON,
			got: func(v any) string { return v.(*apiv1.Environment).DisplayName }, want: "GCP GKE"},
		{name: "GetTraffic", call: func(c *Client) (any, error) { return c.GetTraffic(ctx) },
			method: "GET", path: "/v1/traffic",
			resp: `{"writeBackend":"gke","readWeights":{"gke":90,"eks":10},"writesHeld":false,"heldWrites":0,"maxHold":"30s","inFlight":{"reads":1,"writes":0},"backends":[],"stats":{},"totals":{},"updatedAt":"2026-11-02T09:00:00Z"}`,
			got:  func(v any) string { return fmt.Sprint(v.(*apiv1.TrafficState).ReadWeights["eks"]) }, want: "10"},
		{name: "GetMatrix", call: func(c *Client) (any, error) { return c.GetMatrix(ctx) },
			method: "GET", path: "/v1/matrix", resp: `{"environments":["gke","eks"],"routes":[{"source":"gke","target":"eks","status":"succeeded"}]}`,
			got: func(v any) string { return string(*v.(*apiv1.Matrix).Routes[0].Status) }, want: "succeeded"},
		{name: "ListAudit", call: func(c *Client) (any, error) { return c.ListAudit(ctx, 42, 10) },
			method: "GET", path: "/v1/audit", query: "before=42&limit=10",
			resp: `{"entries":[{"id":41,"time":"2026-11-02T09:00:00Z","actor":"op","role":"operator","action":"POST /v1/migrations","target":"/v1/migrations","status":202}],"nextBefore":41}`,
			got:  func(v any) string { return fmt.Sprint(*v.(*apiv1.AuditList).NextBefore) }, want: "41"},
		{name: "ListAudit defaults", call: func(c *Client) (any, error) { return c.ListAudit(ctx, 0, 0) },
			method: "GET", path: "/v1/audit", query: "", resp: `{"entries":[]}`,
			got: func(v any) string { return fmt.Sprint(len(v.(*apiv1.AuditList).Entries)) }, want: "0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status := tc.status
			if status == 0 {
				status = http.StatusOK
			}
			c, s := fakeAPI(t, status, tc.resp)
			v, err := tc.call(c)
			if err != nil {
				t.Fatalf("call: %v", err)
			}
			if s.method != tc.method || s.path != tc.path || s.query != tc.query {
				t.Errorf("request = %s %s ?%s, want %s %s ?%s", s.method, s.path, s.query, tc.method, tc.path, tc.query)
			}
			wantAuth := "Bearer tok"
			if tc.noAuth {
				wantAuth = ""
			}
			if got := s.header.Get("Authorization"); got != wantAuth {
				t.Errorf("Authorization = %q, want %q", got, wantAuth)
			}
			if got := s.header.Get("User-Agent"); got != "test-agent/1" {
				t.Errorf("User-Agent = %q", got)
			}
			for k, want := range tc.header {
				if got := s.header.Get(k); got != want {
					t.Errorf("%s = %q, want %q", k, got, want)
				}
			}
			if tc.body == "" {
				if len(s.body) != 0 {
					t.Errorf("unexpected body %s", s.body)
				}
			} else {
				if ct := s.header.Get("Content-Type"); ct != "application/json" {
					t.Errorf("Content-Type = %q", ct)
				}
				if !jsonEqual(t, s.body, []byte(tc.body)) {
					t.Errorf("body = %s, want %s", s.body, tc.body)
				}
			}
			if got := tc.got(v); got != tc.want {
				t.Errorf("decoded %q, want %q", got, tc.want)
			}
		})
	}
}

func jsonEqual(t *testing.T, a, b []byte) bool {
	t.Helper()
	var x, y any
	if err := json.Unmarshal(a, &x); err != nil {
		t.Fatalf("bad JSON %s: %v", a, err)
	}
	if err := json.Unmarshal(b, &y); err != nil {
		t.Fatalf("bad JSON %s: %v", b, err)
	}
	return reflect.DeepEqual(x, y)
}

func TestEmptyPathParam(t *testing.T) {
	c, s := fakeAPI(t, 200, planJSON)
	if _, err := c.GetPlan(context.Background(), ""); err == nil {
		t.Fatal("want an error for an empty ref")
	}
	if s.method != "" {
		t.Fatalf("no request should be sent, got %s %s", s.method, s.path)
	}
}

func TestErrors(t *testing.T) {
	cases := []struct {
		name        string
		status      int
		body        string
		code, msg   string
		errContains string
	}{
		{"error body", 404, `{"error":{"code":"plan_not_found","message":"plan not found"}}`, "plan_not_found", "plan not found", "plan_not_found: plan not found (HTTP 404)"},
		{"html page", 502, "<html>\n<body>\n  <h1>502 Bad Gateway</h1>\n</body></html>", "", "<html> <body> <h1>502 Bad Gateway</h1> </body></html>", "HTTP 502: <html>"},
		{"empty body", 500, "", "", "Internal Server Error", "HTTP 500: Internal Server Error"},
		{"json without code", 409, `{"error":{}}`, "", `{"error":{}}`, "HTTP 409"},
		{"long body", 503, strings.Repeat("x", 500), "", strings.Repeat("x", 200) + "...", "HTTP 503"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := fakeAPI(t, tc.status, tc.body)
			_, err := c.GetPlan(context.Background(), "pln_1")
			var ae *APIError
			if !errors.As(err, &ae) {
				t.Fatalf("err = %v (%T), want *APIError", err, err)
			}
			if ae.Status != tc.status || ae.Code != tc.code || ae.Message != tc.msg {
				t.Errorf("got %+v, want status %d code %q message %q", *ae, tc.status, tc.code, tc.msg)
			}
			if !strings.Contains(err.Error(), tc.errContains) {
				t.Errorf("Error() = %q, want it to contain %q", err.Error(), tc.errContains)
			}
			if got := ErrorCode(fmt.Errorf("wrapped: %w", err)); got != tc.code {
				t.Errorf("ErrorCode = %q, want %q", got, tc.code)
			}
		})
	}
	if ErrorCode(nil) != "" || ErrorCode(errors.New("plain")) != "" {
		t.Error("ErrorCode of a non-API error must be empty")
	}
}

func TestDecodeFailure(t *testing.T) {
	c, _ := fakeAPI(t, 200, `{"ref":`)
	if _, err := c.GetPlan(context.Background(), "pln_1"); err == nil || ErrorCode(err) != "" {
		t.Fatalf("want a decode error, got %v", err)
	}
}
