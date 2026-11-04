package client

import (
	"context"
	"net/http"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

// Login logs an administrator in with a username and password and returns a
// console session; pass its Token to New for the calls that follow. It sends
// no token. Error codes: invalid_credentials, too_many_attempts,
// login_not_configured.
func (c *Client) Login(ctx context.Context, username, password string) (*apiv1.Session, error) {
	return call[apiv1.Session](ctx, c, request{method: http.MethodPost, path: "/v1/auth/login", public: true,
		body: apiv1.LoginRequest{Username: username, Password: password}})
}

// Logout ends the console session or CLI token the client was created with.
// A static token keeps working; the call succeeds all the same.
func (c *Client) Logout(ctx context.Context) error {
	resp, err := c.send(ctx, request{method: http.MethodPost, path: "/v1/auth/logout"})
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// StartDeviceLogin starts a CLI device login. client says what is asking
// (e.g. "charlie v0.5.0 on rin-sbx-12") and is shown to the administrator.
// Show the person UserCode and VerificationUri, keep DeviceCode secret, and
// poll with PollDeviceLogin every Interval seconds. It sends no token.
func (c *Client) StartDeviceLogin(ctx context.Context, client string) (*apiv1.DeviceLoginStart, error) {
	return call[apiv1.DeviceLoginStart](ctx, c, request{method: http.MethodPost, path: "/v1/auth/device", public: true,
		body: apiv1.DeviceLoginStartRequest{Client: client}})
}

// PollDeviceLogin asks once whether the device login was approved and, if
// so, returns the CLI token with the approver's name and the role they
// picked. Until then
// it fails with ErrorCode(err) == "authorization_pending" (ask again after
// the interval); "access_denied" and "expired_token" mean the device login
// is over. It sends no token.
func (c *Client) PollDeviceLogin(ctx context.Context, deviceCode string) (*apiv1.Session, error) {
	return call[apiv1.Session](ctx, c, request{method: http.MethodPost, path: "/v1/auth/device/token", public: true,
		body: apiv1.DeviceTokenRequest{DeviceCode: deviceCode}})
}

// GetDeviceLogin returns a device login by the code the CLI shows (case,
// spaces and '-' do not matter), to see which client asks before approving.
// It, ApproveDeviceLogin and DenyDeviceLogin need a console session (a
// token from Login); other tokens get console_session_required.
func (c *Client) GetDeviceLogin(ctx context.Context, userCode string) (*apiv1.DeviceLogin, error) {
	return call[apiv1.DeviceLogin](ctx, c, request{method: http.MethodGet, path: "/v1/auth/device/%s", params: []string{userCode}})
}

// ApproveDeviceLogin approves a pending device login. Its CLI token gets
// the caller's name and the given role, not the caller's own: give a bot only
// what it needs (viewer to read, agent to also submit plans, operator to
// deploy and approve). Approving gives the CLI at least 10 more minutes to
// exchange it.
func (c *Client) ApproveDeviceLogin(ctx context.Context, userCode string, role apiv1.Role) (*apiv1.DeviceLogin, error) {
	return call[apiv1.DeviceLogin](ctx, c, request{method: http.MethodPost, path: "/v1/auth/device/%s/approve", params: []string{userCode},
		body: apiv1.DeviceLoginApproveRequest{Role: role}})
}

// DenyDeviceLogin denies a pending device login.
func (c *Client) DenyDeviceLogin(ctx context.Context, userCode string) (*apiv1.DeviceLogin, error) {
	return call[apiv1.DeviceLogin](ctx, c, request{method: http.MethodPost, path: "/v1/auth/device/%s/deny", params: []string{userCode}})
}
