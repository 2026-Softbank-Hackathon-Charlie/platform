package client

import (
	"context"
	"testing"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

const appSecretsJSON = `{"app":[{"name":"HOST_CODE","updatedAt":"2026-11-04T09:00:00Z","updatedBy":"op"}],"instances":[{"instance":"moimroom--dev","secrets":[{"name":"HOST_CODE","updatedAt":"2026-11-04T09:00:00Z","updatedBy":"op"}]}]}`

func TestSecretOperations(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name        string
		call        func(*Client) (any, error)
		method      string
		path, query string
		body        string // expected JSON request body; "" means none
		resp        string
		got         func(any) string
		want        string
	}{
		{name: "ListAppSecrets", call: func(c *Client) (any, error) { return c.ListAppSecrets(ctx, "moimroom") },
			method: "GET", path: "/v1/apps/moimroom/secrets", resp: appSecretsJSON,
			got: func(v any) string {
				s := v.(*apiv1.AppSecrets)
				return s.App[0].Name + " " + s.Instances[0].Instance + " " + s.Instances[0].Secrets[0].UpdatedBy
			},
			want: "HOST_CODE moimroom--dev op"},
		{name: "UpdateAppSecrets app-wide", call: func(c *Client) (any, error) {
			return c.UpdateAppSecrets(ctx, "moimroom", apiv1.AppSecretsUpdate{Set: &map[string]string{"HOST_CODE": "4821"}, Delete: &[]string{"OLD"}})
		},
			method: "PUT", path: "/v1/apps/moimroom/secrets", body: `{"set":{"HOST_CODE":"4821"},"delete":["OLD"]}`, resp: appSecretsJSON,
			got: func(v any) string { return v.(*apiv1.AppSecrets).App[0].Name }, want: "HOST_CODE"},
		{name: "UpdateAppSecrets instance", call: func(c *Client) (any, error) {
			return c.UpdateAppSecrets(ctx, "moimroom", apiv1.AppSecretsUpdate{Instance: ptr("moimroom--dev"), Delete: &[]string{"HOST_CODE"}})
		},
			method: "PUT", path: "/v1/apps/moimroom/secrets", body: `{"instance":"moimroom--dev","delete":["HOST_CODE"]}`, resp: appSecretsJSON,
			got: func(v any) string { return v.(*apiv1.AppSecrets).Instances[0].Instance }, want: "moimroom--dev"},
		{name: "GetAppSecret app-wide", call: func(c *Client) (any, error) { return c.GetAppSecret(ctx, "moimroom", "HOST_CODE", "") },
			method: "GET", path: "/v1/apps/moimroom/secrets/HOST_CODE",
			resp: `{"name":"HOST_CODE","value":"4821","updatedAt":"2026-11-04T09:00:00Z","updatedBy":"op"}`,
			got: func(v any) string {
				s := v.(*apiv1.AppSecretValue)
				return s.Value + " " + map[bool]string{true: "app-wide", false: "instance"}[s.Instance == nil]
			},
			want: "4821 app-wide"},
		{name: "GetAppSecret instance", call: func(c *Client) (any, error) { return c.GetAppSecret(ctx, "moimroom", "HOST_CODE", "moimroom--dev") },
			method: "GET", path: "/v1/apps/moimroom/secrets/HOST_CODE", query: "instance=moimroom--dev",
			resp: `{"name":"HOST_CODE","instance":"moimroom--dev","value":"1111","updatedAt":"2026-11-04T09:00:00Z","updatedBy":"op"}`,
			got:  func(v any) string { return *v.(*apiv1.AppSecretValue).Instance + " " + v.(*apiv1.AppSecretValue).Value },
			want: "moimroom--dev 1111"},
		{name: "CreateDeployment redeploy", call: func(c *Client) (any, error) {
			return c.CreateDeployment(ctx, "moimroom", apiv1.DeploymentCreateRequest{Branch: ptr("dev"), Redeploy: ptr(true)})
		},
			method: "POST", path: "/v1/apps/moimroom/deployments", body: `{"branch":"dev","redeploy":true}`, resp: deploymentJSON,
			got: func(v any) string { return v.(*apiv1.Deployment).Id }, want: "dep_1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, s := fakeAPI(t, 200, tc.resp)
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

func TestSecretErrors(t *testing.T) {
	ctx := context.Background()
	c, _ := fakeAPI(t, 503, `{"error":{"code":"secrets_not_configured","message":"no key"}}`)
	if _, err := c.ListAppSecrets(ctx, "moimroom"); ErrorCode(err) != "secrets_not_configured" {
		t.Fatalf("ListAppSecrets: %v", err)
	}
	c, _ = fakeAPI(t, 404, `{"error":{"code":"secret_not_found","message":"no"}}`)
	if _, err := c.GetAppSecret(ctx, "moimroom", "NOPE", ""); ErrorCode(err) != "secret_not_found" {
		t.Fatalf("GetAppSecret: %v", err)
	}
	c, s := fakeAPI(t, 200, `{}`)
	if _, err := c.GetAppSecret(ctx, "moimroom", "", ""); err == nil || s.method != "" {
		t.Fatalf("empty name: %v, sent %s", err, s.method)
	}
}
