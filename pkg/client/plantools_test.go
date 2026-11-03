package client

import (
	"context"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

func TestPlanToolsOperations(t *testing.T) {
	ctx := context.Background()
	at := time.Date(2026, 11, 7, 1, 0, 0, 0, time.UTC)
	files := false
	stream := "moimroom-demo"
	cases := []struct {
		name         string
		call         func(*Client) (any, error)
		method, path string
		body         string // expected request body
		contentType  string // expected request Content-Type
		accept       string
		status       int
		resp         string
		got          func(any) string
		want         string
	}{
		{name: "AppendWrittenRecords", call: func(c *Client) (any, error) {
			return c.AppendWrittenRecords(ctx, "moimroom-demo", []apiv1.WrittenRecord{{Table: "app.reservations", Pk: "42", At: &at}, {Table: "app.reservation_slots", Pk: "(3,2026-11-07,14)"}})
		},
			method: "POST", path: "/v1/written-records/moimroom-demo", status: 202, contentType: "application/json",
			body: `{"records":[{"at":"2026-11-07T01:00:00Z","pk":"42","table":"app.reservations"},{"pk":"(3,2026-11-07,14)","table":"app.reservation_slots"}]}`,
			resp: `{"accepted":2}`, got: func(v any) string { return strconv.Itoa(v.(*apiv1.WrittenRecordsAccepted).Accepted) }, want: "2"},
		{name: "GetWrittenRecordStats", call: func(c *Client) (any, error) { return c.GetWrittenRecordStats(ctx, "moimroom-demo") },
			method: "GET", path: "/v1/written-records/moimroom-demo", resp: `{"stream":"moimroom-demo","count":3}`,
			got: func(v any) string { return strconv.FormatInt(v.(*apiv1.WrittenRecordStats).Count, 10) }, want: "3"},
		{name: "PutPlanAttachment", call: func(c *Client) (any, error) {
			return c.PutPlanAttachment(ctx, "pln_1", "target.yaml", []byte("kind: Cluster\n"))
		},
			method: "PUT", path: "/v1/plans/pln_1/attachments/target.yaml", body: "kind: Cluster\n", contentType: "application/octet-stream",
			resp: `{"name":"target.yaml","size":14,"sha256":"ab"}`, got: func(v any) string { return v.(*apiv1.PlanAttachment).Sha256 }, want: "ab"},
		{name: "GetPlanAttachment", call: func(c *Client) (any, error) { return c.GetPlanAttachment(ctx, "pln_1", "target.yaml") },
			method: "GET", path: "/v1/plans/pln_1/attachments/target.yaml", accept: "application/octet-stream", resp: "kind: Cluster\n",
			got: func(v any) string { return string(v.([]byte)) }, want: "kind: Cluster\n"},
		{name: "ListApps", call: func(c *Client) (any, error) { return c.ListApps(ctx) },
			method: "GET", path: "/v1/apps",
			resp: `{"apps":[{"name":"moimroom","namespace":"moimroom","database":{"name":"app","user":"app","schemas":["app"]},"environments":[{"env":"gke","cnpgCluster":"moimroom-db-gke","dbHost":"h:5432"}]}]}`,
			got:  func(v any) string { return v.(*apiv1.AppList).Apps[0].Environments[0].CnpgCluster }, want: "moimroom-db-gke"},
		{name: "DraftPlan", call: func(c *Client) (any, error) {
			return c.DraftPlan(ctx, apiv1.PlanDraftRequest{App: "moimroom", Source: "gke", Target: "eks", Files: &files, WrittenRecordsStream: &stream})
		},
			method: "POST", path: "/v1/plans/draft", contentType: "application/json",
			body: `{"app":"moimroom","files":false,"source":"gke","target":"eks","writtenRecordsStream":"moimroom-demo"}`,
			resp: `{"id":"moimroom-gke-to-eks","source":{"env":"gke","namespace":"moimroom","cluster":"c","dsn":"d"},"target":{"env":"eks","namespace":"moimroom","cluster":"c","dsn":"d"}}`,
			got:  func(v any) string { return v.(*apiv1.EnginePlan).Id }, want: "moimroom-gke-to-eks"},
		{name: "DraftPlanWithSourceStats", call: func(c *Client) (any, error) {
			return c.DraftPlanWithSourceStats(ctx, apiv1.PlanDraftRequest{App: "moimroom", Source: "gke", Target: "eks"})
		},
			method: "POST", path: "/v1/plans/draft", contentType: "application/json",
			body: `{"app":"moimroom","source":"gke","sourceStats":true,"target":"eks"}`,
			resp: `{"plan":{"id":"moimroom-gke-to-eks","source":{"env":"gke","namespace":"moimroom","cluster":"c","dsn":"d"},"target":{"env":"eks","namespace":"moimroom","cluster":"c","dsn":"d"}},"sourceStats":{"sessions":80,"maxConnections":200,"maxWorkerProcesses":8,"windowSeconds":5,"measuredAt":"2026-11-03T12:00:00Z"}}`,
			got: func(v any) string {
				r := v.(*apiv1.PlanDraftResult)
				return fmt.Sprintf("%s %d", r.Plan.Id, r.SourceStats.MaxWorkerProcesses)
			}, want: "moimroom-gke-to-eks 8"},
		{name: "RollbackMigration", call: func(c *Client) (any, error) {
			reason, auto := "demo", false
			return c.RollbackMigration(ctx, "run 1", apiv1.RollbackRequest{Reason: &reason, AutoSwitch: &auto})
		},
			method: "POST", path: "/v1/migrations/run%201/rollback", status: 202, contentType: "application/json",
			body: `{"autoSwitch":false,"reason":"demo"}`,
			resp: `{"runId":"run_2","planRef":"pln_2","links":{"self":"s","events":"e"}}`,
			got:  func(v any) string { return v.(*apiv1.StartMigrationResponse).RunId }, want: "run_2"},
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
			if s.method != tc.method || s.path != tc.path {
				t.Errorf("request = %s %s, want %s %s", s.method, s.path, tc.method, tc.path)
			}
			if got := s.header.Get("Authorization"); got != "Bearer tok" {
				t.Errorf("Authorization = %q", got)
			}
			switch {
			case tc.contentType == "application/json":
				if !jsonEqual(t, s.body, []byte(tc.body)) {
					t.Errorf("body = %s, want %s", s.body, tc.body)
				}
			case string(s.body) != tc.body:
				t.Errorf("body = %q, want %q", s.body, tc.body)
			}
			if tc.contentType != "" && s.header.Get("Content-Type") != tc.contentType {
				t.Errorf("Content-Type = %q, want %q", s.header.Get("Content-Type"), tc.contentType)
			}
			if tc.accept != "" && s.header.Get("Accept") != tc.accept {
				t.Errorf("Accept = %q, want %q", s.header.Get("Accept"), tc.accept)
			}
			if got := tc.got(v); got != tc.want {
				t.Errorf("result = %q, want %q", got, tc.want)
			}
		})
	}

	c, _ := fakeAPI(t, 404, `{"error":{"code":"attachment_not_found","message":"the plan has no attachment named x"}}`)
	if _, err := c.GetPlanAttachment(ctx, "pln_1", "x"); ErrorCode(err) != "attachment_not_found" {
		t.Fatalf("missing attachment: %v", err)
	}
}
