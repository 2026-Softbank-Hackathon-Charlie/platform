package client

import (
	"context"
	"net/http"
	"net/url"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

// ListAppSecrets returns the names of a registered app's secrets, without
// values: the app-wide ones and each instance's overrides (ADR 0019).
func (c *Client) ListAppSecrets(ctx context.Context, app string) (*apiv1.AppSecrets, error) {
	return call[apiv1.AppSecrets](ctx, c, request{method: http.MethodGet, path: "/v1/apps/%s/secrets", params: []string{app}})
}

// UpdateAppSecrets sets and deletes secrets of a registered app in one
// change: the app-wide ones, or one instance's overrides when req.Instance
// is set. It returns the names after the change. Running instances get the
// new values at their next deployment; CreateDeployment with Redeploy
// applies them without a build.
func (c *Client) UpdateAppSecrets(ctx context.Context, app string, req apiv1.AppSecretsUpdate) (*apiv1.AppSecrets, error) {
	return call[apiv1.AppSecrets](ctx, c, request{method: http.MethodPut, path: "/v1/apps/%s/secrets", params: []string{app}, body: req})
}

// GetAppSecret returns one secret with its value: the app-wide one when
// instance is empty, else that instance's override. The server records
// every call in the audit log. It needs the operator role.
func (c *Client) GetAppSecret(ctx context.Context, app, name, instance string) (*apiv1.AppSecretValue, error) {
	var q url.Values
	if instance != "" {
		q = url.Values{"instance": {instance}}
	}
	return call[apiv1.AppSecretValue](ctx, c, request{method: http.MethodGet, path: "/v1/apps/%s/secrets/%s", params: []string{app, name}, query: q})
}
