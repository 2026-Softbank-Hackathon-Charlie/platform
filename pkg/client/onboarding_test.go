package client

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

const onboardingJSON = `{"id":"onb-3f9a1c2b7d4e","repo":"o/moimroom-legacy","serviceUrl":"https://moimroom-legacy.hopcharlie.dev","app":"moimroom-legacy","targetEnv":"eks","state":"awaiting_approval","deleting":false,"legacyDeployStopped":false,"humanActions":[],"report":{"blockers":[],"warnings":[]},"pr":{"number":12,"url":"https://github.com/o/moimroom-legacy/pull/12","headSha":"abc123"},"legacyChanges":[],"createdBy":"op","createdAt":"2026-11-07T09:00:00Z","updatedAt":"2026-11-07T09:00:00Z"}`

func TestOnboardingOperations(t *testing.T) {
	ctx := context.Background()
	id := "onb-3f9a1c2b7d4e"
	state := func(v any) string { return string(v.(*apiv1.Onboarding).State) }
	cases := []struct {
		name   string
		call   func(*Client) (any, error)
		method string
		path   string
		body   string
		status int
		resp   string
		got    func(any) string
		want   string
	}{
		{name: "StartOnboarding", call: func(c *Client) (any, error) {
			return c.StartOnboarding(ctx, apiv1.OnboardingRequest{Repo: "o/moimroom-legacy", ServiceUrl: "https://moimroom-legacy.hopcharlie.dev", TargetEnv: ptr("eks")})
		},
			method: "POST", path: "/v1/onboardings", status: 202, resp: onboardingJSON,
			body: `{"repo":"o/moimroom-legacy","serviceUrl":"https://moimroom-legacy.hopcharlie.dev","targetEnv":"eks"}`,
			got:  func(v any) string { return v.(*apiv1.Onboarding).Id }, want: id},
		{name: "ListOnboardings", call: func(c *Client) (any, error) { return c.ListOnboardings(ctx) },
			method: "GET", path: "/v1/onboardings", resp: `{"onboardings":[` + onboardingJSON + `]}`,
			got: func(v any) string { return v.(*apiv1.OnboardingList).Onboardings[0].App }, want: "moimroom-legacy"},
		{name: "GetOnboarding", call: func(c *Client) (any, error) { return c.GetOnboarding(ctx, id) },
			method: "GET", path: "/v1/onboardings/" + id, resp: onboardingJSON,
			got: func(v any) string { return v.(*apiv1.Onboarding).Pr.HeadSha }, want: "abc123"},
		{name: "DeleteOnboarding", call: func(c *Client) (any, error) { return c.DeleteOnboarding(ctx, id) },
			method: "DELETE", path: "/v1/onboardings/" + id, status: 202, resp: onboardingJSON, got: state, want: "awaiting_approval"},
		{name: "ApproveOnboarding", call: func(c *Client) (any, error) { return c.ApproveOnboarding(ctx, id, "abc123") },
			method: "POST", path: "/v1/onboardings/" + id + "/approve", body: `{"headSha":"abc123"}`, status: 202,
			resp: strings.Replace(onboardingJSON, "awaiting_approval", "connecting", 1), got: state, want: "connecting"},
		{name: "CutOverOnboarding", call: func(c *Client) (any, error) { return c.CutOverOnboarding(ctx, id) },
			method: "POST", path: "/v1/onboardings/" + id + "/cutover", status: 202,
			resp: strings.Replace(onboardingJSON, `"state":"awaiting_approval"`, `"state":"cutting_over","runId":"run_1"`, 1),
			got:  func(v any) string { return *v.(*apiv1.Onboarding).RunId }, want: "run_1"},
		{name: "DecommissionOnboarding", call: func(c *Client) (any, error) { return c.DecommissionOnboarding(ctx, id) },
			method: "POST", path: "/v1/onboardings/" + id + "/decommission", status: 202, resp: onboardingJSON, got: state, want: "awaiting_approval"},
		{name: "ResumeOnboarding escapes", call: func(c *Client) (any, error) { return c.ResumeOnboarding(ctx, "onb/x") },
			method: "POST", path: "/v1/onboardings/onb%2Fx/resume", status: 202, resp: onboardingJSON, got: state, want: "awaiting_approval"},
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
				t.Errorf("request %s %s, want %s %s", s.method, s.path, tc.method, tc.path)
			}
			switch {
			case tc.body != "" && !jsonEqual(t, s.body, []byte(tc.body)):
				t.Errorf("body %s, want %s", s.body, tc.body)
			case tc.body == "" && len(s.body) != 0:
				t.Errorf("body %s, want none", s.body)
			}
			if got := tc.got(v); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestApproveOnboardingHeadMismatch(t *testing.T) {
	c, _ := fakeAPI(t, 409, `{"error":{"code":"pr_head_mismatch","message":"the PR head is def456"}}`)
	_, err := c.ApproveOnboarding(context.Background(), "onb-3f9a1c2b7d4e", "abc123")
	if ErrorCode(err) != "pr_head_mismatch" {
		t.Fatalf("err = %v", err)
	}
}

func writeOnboardingSnapshot(w io.Writer, state string, deleting bool) {
	doc := strings.Replace(onboardingJSON, `"state":"awaiting_approval"`, `"state":"`+state+`"`, 1)
	doc = strings.Replace(doc, `"deleting":false`, fmt.Sprintf(`"deleting":%t`, deleting), 1)
	fmt.Fprintf(w, "event: snapshot\ndata: %s\n\n", doc)
	w.(http.Flusher).Flush()
}

func writeOnboardingEvent(w io.Writer, seq int64, kind, state string) {
	fmt.Fprintf(w, "event: %s\nid: %d\ndata: {\"seq\":%d,\"onboardingId\":\"onb-3f9a1c2b7d4e\",\"time\":\"2026-11-07T09:00:00Z\",\"state\":%q,\"kind\":%q,\"message\":\"m%d\"}\n\n", kind, seq, seq, state, kind, seq)
	w.(http.Flusher).Flush()
}

func TestWatchOnboardingReconnectsUntilDone(t *testing.T) {
	c, s := newSSEServer(t, func(w http.ResponseWriter, r *http.Request, conn int) {
		switch conn {
		case 0:
			writeOnboardingSnapshot(w, "syncing", false)
			writeOnboardingEvent(w, 1, "step", "syncing")
			abort()
		case 1: // replays seq 1; failed is followed on
			writeOnboardingSnapshot(w, "failed", false)
			writeOnboardingEvent(w, 1, "step", "syncing")
			writeOnboardingEvent(w, 2, "error", "syncing")
		case 2:
			writeOnboardingSnapshot(w, "decommissioning", false)
			writeOnboardingEvent(w, 3, "state", "done")
			writeOnboardingSnapshot(w, "done", false)
		default:
			t.Error("reconnected after done")
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var names []string
	err := c.WatchOnboarding(ctx, "onb-3f9a1c2b7d4e", func(m OnboardingMessage) error {
		names = append(names, m.Name)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := "snapshot,step,snapshot,error,snapshot,state,snapshot"; strings.Join(names, ",") != want {
		t.Errorf("messages = %v, want %s", names, want)
	}
	if want := []string{"", "1", "2"}; !reflect.DeepEqual(s.lastEventIDs(), want) {
		t.Errorf("Last-Event-ID per connection = %q, want %q", s.lastEventIDs(), want)
	}
	if !strings.HasSuffix(s.paths[0], "/v1/onboardings/onb-3f9a1c2b7d4e/events") {
		t.Errorf("path %s", s.paths[0])
	}
}

func TestWatchOnboardingEndsWhenDeleted(t *testing.T) {
	c, s := newSSEServer(t, func(w http.ResponseWriter, r *http.Request, conn int) {
		if conn == 0 {
			writeOnboardingSnapshot(w, "syncing", true)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(404)
		_, _ = io.WriteString(w, `{"error":{"code":"onboarding_not_found","message":"no such onboarding"}}`)
	})
	if err := c.WatchOnboarding(context.Background(), "onb-3f9a1c2b7d4e", func(OnboardingMessage) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if s.conns() != 2 {
		t.Errorf("%d connections, want 2", s.conns())
	}
}

func TestWatchOnboardingNotFound(t *testing.T) {
	c, _ := fakeAPI(t, 404, `{"error":{"code":"onboarding_not_found","message":"no such onboarding"}}`)
	err := c.WatchOnboarding(context.Background(), "onb-x", func(OnboardingMessage) error { return nil })
	if ErrorCode(err) != "onboarding_not_found" {
		t.Fatalf("err = %v", err)
	}
}
