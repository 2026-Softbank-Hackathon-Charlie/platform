package client

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
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
	deploymentJSON = `{"id":"dep_1","app":"moimroom","ref":"main","commit":"9fceb02d0ae598e95dc970b74767f19372d61af8","trigger":"manual","triggeredBy":"op","status":"queued","images":[],"environments":[{"env":"eks","status":"pending"}],"createdAt":"2026-11-03T09:00:00Z"}`
	appJSON        = `{"name":"moimroom","namespace":"moimroom","database":{"name":"app","user":"app","schemas":[]},"environments":[],"deploy":{"repo":"o/moimroom","branch":"main","environments":["eks"],"autoDeploy":true,"spec":{"name":"moimroom","services":[{"name":"web","path":".","dockerfile":"Dockerfile","port":8080,"public":true,"replicas":1}]},"specCommit":"9fceb02d","url":"https://moimroom.hopcharlie.dev","createdAt":"2026-11-03T09:00:00Z","createdBy":"op","status":[{"env":"eks","status":"pending"}]}}`
	validationJSON = `{"valid":false,"spec":{"name":"moimroom","services":[{"name":"web","path":".","dockerfile":"Dockerfile","port":0,"public":false,"replicas":1}]},"commit":"9fceb02d","errors":[{"path":"services[0].port","message":"is required"}]}`
)

func TestDeployOperations(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name        string
		call        func(*Client) (any, error)
		method      string
		path, query string
		body        string // expected JSON request body; "" means none
		status      int
		resp        string
		got         func(any) string
		want        string
	}{
		{name: "CreateApp", call: func(c *Client) (any, error) {
			return c.CreateApp(ctx, apiv1.AppCreateRequest{Repo: "o/moimroom", Environments: []string{"eks"}, AutoDeploy: ptr(false)})
		},
			method: "POST", path: "/v1/apps", body: `{"repo":"o/moimroom","environments":["eks"],"autoDeploy":false}`, status: 201,
			resp: `{"app":` + appJSON + `,"deployment":` + deploymentJSON + `}`,
			got: func(v any) string {
				return v.(*apiv1.AppCreated).App.Deploy.Spec.Services[0].Name + " " + v.(*apiv1.AppCreated).Deployment.Id
			},
			want: "web dep_1"},
		{name: "GetApp escapes", call: func(c *Client) (any, error) { return c.GetApp(ctx, "moim/room") },
			method: "GET", path: "/v1/apps/moim%2Froom", resp: appJSON,
			got: func(v any) string { return *v.(*apiv1.App).Deploy.Url }, want: "https://moimroom.hopcharlie.dev"},
		{name: "DeleteApp", call: func(c *Client) (any, error) { return nil, c.DeleteApp(ctx, "moimroom") },
			method: "DELETE", path: "/v1/apps/moimroom", status: 202,
			got: func(any) string { return "" }, want: ""},
		{name: "ValidateAppSpec repo", call: func(c *Client) (any, error) {
			return c.ValidateAppSpec(ctx, apiv1.AppSpecValidateRequest{Repo: ptr("o/moimroom"), Branch: ptr("dev")})
		},
			method: "POST", path: "/v1/apps/validate", body: `{"repo":"o/moimroom","branch":"dev"}`, resp: validationJSON,
			got: func(v any) string {
				r := v.(*apiv1.AppSpecValidation)
				return fmt.Sprint(r.Valid, " ", *r.Commit, " ", r.Errors[0].Path)
			},
			want: "false 9fceb02d services[0].port"},
		{name: "ValidateAppSpec content", call: func(c *Client) (any, error) {
			return c.ValidateAppSpec(ctx, apiv1.AppSpecValidateRequest{Spec: ptr("name: x\n")})
		},
			method: "POST", path: "/v1/apps/validate", body: `{"spec":"name: x\n"}`, resp: `{"valid":true,"errors":[]}`,
			got: func(v any) string { return fmt.Sprint(v.(*apiv1.AppSpecValidation).Valid) }, want: "true"},
		{name: "CreateDeployment defaults", call: func(c *Client) (any, error) {
			return c.CreateDeployment(ctx, "moimroom", apiv1.DeploymentCreateRequest{})
		},
			method: "POST", path: "/v1/apps/moimroom/deployments", body: `{}`, status: 202, resp: deploymentJSON,
			got: func(v any) string { return string(v.(*apiv1.Deployment).Status) }, want: "queued"},
		{name: "CreateDeployment ref and environments", call: func(c *Client) (any, error) {
			return c.CreateDeployment(ctx, "moimroom", apiv1.DeploymentCreateRequest{Ref: ptr("9fceb02"), Environments: &[]string{"eks"}})
		},
			method: "POST", path: "/v1/apps/moimroom/deployments", body: `{"ref":"9fceb02","environments":["eks"]}`, status: 202, resp: deploymentJSON,
			got: func(v any) string { return v.(*apiv1.Deployment).Environments[0].Env }, want: "eks"},
		{name: "ListDeployments", call: func(c *Client) (any, error) { return c.ListDeployments(ctx, "moimroom", 5) },
			method: "GET", path: "/v1/apps/moimroom/deployments", query: "limit=5", resp: `{"deployments":[` + deploymentJSON + `]}`,
			got: func(v any) string { return v.(*apiv1.DeploymentList).Deployments[0].Commit }, want: "9fceb02d0ae598e95dc970b74767f19372d61af8"},
		{name: "ListDeployments server default", call: func(c *Client) (any, error) { return c.ListDeployments(ctx, "moimroom", 0) },
			method: "GET", path: "/v1/apps/moimroom/deployments", resp: `{"deployments":[]}`,
			got: func(v any) string { return fmt.Sprint(len(v.(*apiv1.DeploymentList).Deployments)) }, want: "0"},
		{name: "GetDeployment", call: func(c *Client) (any, error) { return c.GetDeployment(ctx, "dep_1") },
			method: "GET", path: "/v1/deployments/dep_1", resp: deploymentJSON,
			got: func(v any) string { return string(v.(*apiv1.Deployment).Trigger) }, want: "manual"},
		{name: "ListGitHubRepos", call: func(c *Client) (any, error) { return c.ListGitHubRepos(ctx) },
			method: "GET", path: "/v1/github/repos",
			resp: `{"repos":[{"fullName":"o/moimroom","private":true,"defaultBranch":"main","htmlUrl":"https://github.com/o/moimroom"}]}`,
			got:  func(v any) string { return v.(*apiv1.GitHubRepoList).Repos[0].DefaultBranch }, want: "main"},
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
			if got := s.header.Get("Authorization"); got != "Bearer tok" {
				t.Errorf("Authorization = %q", got)
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

func TestAppSpecError(t *testing.T) {
	ctx := context.Background()
	body := `{"error":{"code":"invalid_app_spec","message":"platform.yaml breaks 1 rule"},"validation":` + validationJSON + `}`
	for name, call := range map[string]func(*Client) error{
		"CreateApp": func(c *Client) error {
			_, err := c.CreateApp(ctx, apiv1.AppCreateRequest{Repo: "o/moimroom", Environments: []string{"eks"}})
			return err
		},
		"CreateDeployment": func(c *Client) error {
			_, err := c.CreateDeployment(ctx, "moimroom", apiv1.DeploymentCreateRequest{})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			c, _ := fakeAPI(t, 422, body)
			err := call(c)
			var se *AppSpecError
			if !errors.As(err, &se) {
				t.Fatalf("err = %T %v, want *AppSpecError", err, err)
			}
			if se.Validation.Valid || len(se.Validation.Errors) != 1 || se.Validation.Errors[0].Message != "is required" || *se.Validation.Commit != "9fceb02d" {
				t.Errorf("validation = %+v", se.Validation)
			}
			var ae *APIError
			if !errors.As(err, &ae) || ae.Status != 422 || ErrorCode(err) != "invalid_app_spec" {
				t.Errorf("as APIError: %v, code %q", ae, ErrorCode(err))
			}
			if err.Error() != "invalid_app_spec: platform.yaml breaks 1 rule (HTTP 422)" {
				t.Errorf("Error() = %q", err.Error())
			}
		})
	}

	// Other errors stay plain *APIErrors, also a 422 with another code.
	c, _ := fakeAPI(t, 409, `{"error":{"code":"app_exists","message":"an app named moimroom is registered"}}`)
	_, err := c.CreateApp(ctx, apiv1.AppCreateRequest{Repo: "o/moimroom", Environments: []string{"eks"}})
	var se *AppSpecError
	if errors.As(err, &se) || ErrorCode(err) != "app_exists" {
		t.Fatalf("conflict: %T %v", err, err)
	}
	c, _ = fakeAPI(t, 422, `{"error":{"code":"something_else","message":"m"}}`)
	if _, err := c.CreateDeployment(ctx, "moimroom", apiv1.DeploymentCreateRequest{}); errors.As(err, &se) || ErrorCode(err) != "something_else" {
		t.Fatalf("other 422: %T %v", err, err)
	}
}

func TestSendGitHubWebhook(t *testing.T) {
	ctx := context.Background()
	payload := []byte(`{"ref":"refs/heads/main","after":"9fceb02d0ae598e95dc970b74767f19372d61af8","repository":{"full_name":"o/moimroom"}}`)
	mac := hmac.New(sha256.New, []byte("s3cret"))
	mac.Write(payload)
	wantSig := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	c, s := fakeAPI(t, 202, `{"deployments":[`+deploymentJSON+`]}`)
	list, err := c.SendGitHubWebhook(ctx, "push", "72d3162e-cc78-11e3-81ab-4c9367dc0958", payload, "s3cret")
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Deployments) != 1 || list.Deployments[0].Id != "dep_1" {
		t.Fatalf("deployments %+v", list.Deployments)
	}
	if s.method != "POST" || s.path != "/v1/webhooks/github" || string(s.body) != string(payload) {
		t.Errorf("request %s %s %s", s.method, s.path, s.body)
	}
	for k, want := range map[string]string{
		"Authorization":       "",
		"X-GitHub-Event":      "push",
		"X-GitHub-Delivery":   "72d3162e-cc78-11e3-81ab-4c9367dc0958",
		"X-Hub-Signature-256": wantSig,
		"Content-Type":        "application/json",
	} {
		if got := s.header.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}

	// 204: nothing to do, an empty list.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	list, err = New(srv.URL, "tok").SendGitHubWebhook(ctx, "ping", "d2", nil, "s3cret")
	if err != nil || list == nil || list.Deployments == nil || len(list.Deployments) != 0 {
		t.Fatalf("204: %+v, %v", list, err)
	}

	c, _ = fakeAPI(t, 401, `{"error":{"code":"invalid_signature","message":"X-Hub-Signature-256 does not match the body"}}`)
	if _, err := c.SendGitHubWebhook(ctx, "push", "d3", payload, "wrong"); ErrorCode(err) != "invalid_signature" {
		t.Fatalf("401: %v", err)
	}
}

func writeDeploymentSnapshot(w io.Writer, status string) {
	fmt.Fprintf(w, "event: snapshot\ndata: %s\n\n", strings.Replace(deploymentJSON, `"status":"queued"`, `"status":"`+status+`"`, 1))
	w.(http.Flusher).Flush()
}

func writeDeploymentEvent(w io.Writer, seq int64, typ, phase string) {
	fmt.Fprintf(w, "event: %s\nid: %d\ndata: {\"seq\":%d,\"deploymentId\":\"dep_1\",\"time\":\"2026-11-03T09:00:00Z\",\"type\":%q,\"phase\":%q,\"message\":\"m%d\"}\n\n", typ, seq, seq, typ, phase, seq)
	w.(http.Flusher).Flush()
}

func TestWatchDeploymentReconnectsWithoutDuplicates(t *testing.T) {
	c, s := newSSEServer(t, func(w http.ResponseWriter, r *http.Request, conn int) {
		switch conn {
		case 0: // the stream drops while the image builds
			writeDeploymentSnapshot(w, "queued")
			writeDeploymentEvent(w, 1, "phase", "building")
			writeDeploymentEvent(w, 2, "log", "building")
			abort()
		case 1: // replays more than asked, then the deployment finishes
			writeDeploymentSnapshot(w, "building")
			writeDeploymentEvent(w, 2, "log", "building")
			writeDeploymentEvent(w, 3, "environment", "syncing")
			writeDeploymentEvent(w, 4, "done", "healthy")
			writeDeploymentSnapshot(w, "healthy")
		default:
			t.Error("reconnected after the deployment finished")
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var names []string
	var seqs []int64
	err := c.WatchDeployment(ctx, "dep_1", func(m DeploymentMessage) error {
		names = append(names, m.Name)
		if m.Event != nil {
			seqs = append(seqs, m.Seq)
			if m.Event.DeploymentId != "dep_1" || *m.Event.Message != fmt.Sprintf("m%d", m.Seq) {
				t.Errorf("event %+v", m.Event)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := []int64{1, 2, 3, 4}; !reflect.DeepEqual(seqs, want) {
		t.Errorf("seqs = %v, want %v", seqs, want)
	}
	if want := "snapshot,phase,log,snapshot,environment,done,snapshot"; strings.Join(names, ",") != want {
		t.Errorf("messages = %v, want %s", names, want)
	}
	if want := []string{"", "2"}; !reflect.DeepEqual(s.lastEventIDs(), want) {
		t.Errorf("Last-Event-ID per connection = %q, want %q", s.lastEventIDs(), want)
	}
	if !strings.HasSuffix(s.paths[0], "/v1/deployments/dep_1/events") {
		t.Errorf("path %s", s.paths[0])
	}
}

func TestWatchDeploymentEndsOnEveryFinalStatus(t *testing.T) {
	for _, status := range []string{"healthy", "failed", "superseded"} {
		c, s := newSSEServer(t, func(w http.ResponseWriter, r *http.Request, conn int) {
			writeDeploymentSnapshot(w, status)
		})
		if err := c.WatchDeployment(context.Background(), "dep_1", func(DeploymentMessage) error { return nil }); err != nil {
			t.Fatalf("%s: %v", status, err)
		}
		if s.conns() != 1 {
			t.Errorf("%s: %d connections, want 1", status, s.conns())
		}
	}
}

func TestWatchDeploymentErrors(t *testing.T) {
	c, _ := fakeAPI(t, 404, `{"error":{"code":"deployment_not_found","message":"deployment not found"}}`)
	err := c.WatchDeployment(context.Background(), "dep_x", func(DeploymentMessage) error { return nil })
	if ErrorCode(err) != "deployment_not_found" {
		t.Fatalf("unknown deployment: %v", err)
	}
	c, _ = newSSEServer(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		fmt.Fprint(w, "event: snapshot\ndata: {not json}\n\n")
	})
	err = c.WatchDeployment(context.Background(), "dep_1", func(DeploymentMessage) error { return nil })
	var pe *protocolError
	if !errors.As(err, &pe) || !strings.Contains(err.Error(), "decode deployment snapshot") {
		t.Fatalf("bad snapshot: %v", err)
	}
	c, _ = fakeAPI(t, 200, `{}`) // JSON instead of an event stream
	err = c.StreamDeploymentEvents(context.Background(), "dep_1", 0, func(DeploymentMessage) error { return nil })
	if !errors.As(err, &pe) || !strings.Contains(err.Error(), "Content-Type") {
		t.Fatalf("wrong content type: %v", err)
	}
}
