package client

import (
	"context"
	"net/http"
	"testing"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

const (
	authSessionJSON = `{"token":"hcs_abc","expiresAt":"2026-11-04T21:00:00Z","principal":{"name":"seohyun","role":"operator"}}`
	deviceLoginJSON = `{"userCode":"BCDF-GHJK","client":"charlie v0.5.0 on rin-sbx-12","status":"approved","createdAt":"2026-11-04T09:00:00Z","expiresAt":"2026-11-04T09:10:00Z","reviewedBy":"seohyun","role":"agent"}`
)

func TestAuthOperations(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name   string
		call   func(*Client) (any, error)
		method string
		path   string
		body   string // expected JSON request body; "" means none
		noAuth bool
		status int
		resp   string
		got    func(any) string
		want   string
	}{
		{name: "Login", call: func(c *Client) (any, error) { return c.Login(ctx, "seohyun", "pa ss") },
			method: "POST", path: "/v1/auth/login", body: `{"username":"seohyun","password":"pa ss"}`, noAuth: true, resp: authSessionJSON,
			got: func(v any) string { s := v.(*apiv1.Session); return s.Token + " " + s.Principal.Name }, want: "hcs_abc seohyun"},
		{name: "Logout", call: func(c *Client) (any, error) { return nil, c.Logout(ctx) },
			method: "POST", path: "/v1/auth/logout", status: http.StatusNoContent,
			got: func(any) string { return "" }, want: ""},
		{name: "StartDeviceLogin", call: func(c *Client) (any, error) { return c.StartDeviceLogin(ctx, "charlie v0.5.0 on rin-sbx-12") },
			method: "POST", path: "/v1/auth/device", body: `{"client":"charlie v0.5.0 on rin-sbx-12"}`, noAuth: true,
			resp: `{"deviceCode":"hcd_x","userCode":"BCDF-GHJK","verificationUri":"https://console.hopcharlie.dev/#device","expiresIn":600,"interval":5}`,
			got: func(v any) string {
				d := v.(*apiv1.DeviceLoginStart)
				return d.UserCode + " " + d.VerificationUri
			},
			want: "BCDF-GHJK https://console.hopcharlie.dev/#device"},
		{name: "PollDeviceLogin", call: func(c *Client) (any, error) { return c.PollDeviceLogin(ctx, "hcd_x") },
			method: "POST", path: "/v1/auth/device/token", body: `{"deviceCode":"hcd_x"}`, noAuth: true, resp: authSessionJSON,
			got: func(v any) string { return string(v.(*apiv1.Session).Principal.Role) }, want: "operator"},
		{name: "GetDeviceLogin escapes", call: func(c *Client) (any, error) { return c.GetDeviceLogin(ctx, "bcdf ghjk") },
			method: "GET", path: "/v1/auth/device/bcdf%20ghjk", resp: deviceLoginJSON,
			got: func(v any) string { return v.(*apiv1.DeviceLogin).Client }, want: "charlie v0.5.0 on rin-sbx-12"},
		{name: "ApproveDeviceLogin", call: func(c *Client) (any, error) { return c.ApproveDeviceLogin(ctx, "BCDF-GHJK", apiv1.RoleAgent) },
			method: "POST", path: "/v1/auth/device/BCDF-GHJK/approve", body: `{"role":"agent"}`, resp: deviceLoginJSON,
			got: func(v any) string {
				d := v.(*apiv1.DeviceLogin)
				return *d.ReviewedBy + " " + string(*d.Role)
			},
			want: "seohyun agent"},
		{name: "DenyDeviceLogin", call: func(c *Client) (any, error) { return c.DenyDeviceLogin(ctx, "BCDF-GHJK") },
			method: "POST", path: "/v1/auth/device/BCDF-GHJK/deny", resp: deviceLoginJSON,
			got: func(v any) string { return string(v.(*apiv1.DeviceLogin).Status) }, want: "approved"},
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
				t.Fatal(err)
			}
			if s.method != tc.method || s.path != tc.path {
				t.Errorf("request %s %s, want %s %s", s.method, s.path, tc.method, tc.path)
			}
			wantAuth := "Bearer tok"
			if tc.noAuth {
				wantAuth = ""
			}
			if got := s.header.Get("Authorization"); got != wantAuth {
				t.Errorf("Authorization = %q, want %q", got, wantAuth)
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

// The CLI's poll loop tells the three outcomes apart by ErrorCode.
func TestPollDeviceLoginErrors(t *testing.T) {
	for _, code := range []string{"authorization_pending", "access_denied", "expired_token"} {
		c, _ := fakeAPI(t, http.StatusBadRequest, `{"error":{"code":"`+code+`","message":"m"}}`)
		s, err := c.PollDeviceLogin(context.Background(), "hcd_x")
		if s != nil || ErrorCode(err) != code {
			t.Errorf("%s: session %v, ErrorCode(%v) = %q", code, s, err, ErrorCode(err))
		}
	}
}
