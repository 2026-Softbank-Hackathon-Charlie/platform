package connect

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/oauth2/clientcredentials"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/onboard"
)

// Tailnet mints the auth key of the Tailscale container and deletes its
// device (ADR 0042 "3." step 3, "6." step 2).
type Tailnet interface {
	// AuthKey returns a one-time, preauthorized, non-ephemeral key tagged
	// onboard.TailscaleTag that expires in an hour.
	AuthKey(ctx context.Context, description string) (string, error)
	// DeleteDevice deletes the devices named hostname that carry
	// onboard.TailscaleTag; it reports whether there was one.
	DeleteDevice(ctx context.Context, hostname string) (bool, error)
}

// The environment of the Tailscale OAuth client (infra puts SSM
// /hopcharlie/tailscale-onboard-oauth there). Its scopes are auth_keys and
// devices:core, tagged tag:hop-legacy.
const (
	TailscaleClientIDEnv     = "PLATFORM_TAILSCALE_OAUTH_CLIENT_ID"
	TailscaleClientSecretEnv = "PLATFORM_TAILSCALE_OAUTH_CLIENT_SECRET"
)

// TailscaleAPI is Tailnet over the Tailscale API v2 with an OAuth client.
type TailscaleAPI struct {
	HTTP    *http.Client // authorized with the OAuth client's tokens
	BaseURL string       // default https://api.tailscale.com
}

// TailscaleFromEnv returns the client, or nil when the variables are not
// set.
func TailscaleFromEnv(ctx context.Context, getenv func(string) string) (*TailscaleAPI, error) {
	id, secret := strings.TrimSpace(getenv(TailscaleClientIDEnv)), strings.TrimSpace(getenv(TailscaleClientSecretEnv))
	if id == "" && secret == "" {
		return nil, nil
	}
	if id == "" || secret == "" {
		return nil, fmt.Errorf("set both %s and %s", TailscaleClientIDEnv, TailscaleClientSecretEnv)
	}
	cc := clientcredentials.Config{ClientID: id, ClientSecret: secret, TokenURL: "https://api.tailscale.com/api/v2/oauth/token"}
	c := cc.Client(ctx)
	c.Timeout = 20 * time.Second
	return &TailscaleAPI{HTTP: c}, nil
}

func (t *TailscaleAPI) base() string {
	if t.BaseURL != "" {
		return t.BaseURL
	}
	return "https://api.tailscale.com"
}

// do sends a request; errors carry the status, never a body (a key
// answer holds the key).
func (t *TailscaleAPI) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, t.base()+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := t.HTTP.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			return fmt.Errorf("tailscale: %s %s: %v", method, path, ue.Err)
		}
		return fmt.Errorf("tailscale: %s %s failed", method, path)
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return fmt.Errorf("tailscale: %s %s: HTTP %d", method, path, res.StatusCode)
	}
	if out != nil {
		if err := json.NewDecoder(io.LimitReader(res.Body, 8<<20)).Decode(out); err != nil {
			return fmt.Errorf("tailscale: %s %s: invalid answer", method, path)
		}
	}
	return nil
}

// AuthKey implements Tailnet.
func (t *TailscaleAPI) AuthKey(ctx context.Context, description string) (string, error) {
	in := map[string]any{
		"capabilities": map[string]any{"devices": map[string]any{"create": map[string]any{
			"reusable": false, "ephemeral": false, "preauthorized": true, "tags": []string{onboard.TailscaleTag}}}},
		"expirySeconds": 3600,
		"description":   description,
	}
	var out struct {
		Key string `json:"key"`
	}
	if err := t.do(ctx, http.MethodPost, "/api/v2/tailnet/-/keys", in, &out); err != nil {
		return "", err
	}
	if !strings.HasPrefix(out.Key, "tskey-auth-") {
		return "", errors.New("tailscale: the answer has no auth key")
	}
	return out.Key, nil
}

// DeleteDevice implements Tailnet.
func (t *TailscaleAPI) DeleteDevice(ctx context.Context, hostname string) (bool, error) {
	var out struct {
		Devices []struct {
			ID       string   `json:"id"`
			Hostname string   `json:"hostname"`
			Name     string   `json:"name"`
			Tags     []string `json:"tags"`
		} `json:"devices"`
	}
	if err := t.do(ctx, http.MethodGet, "/api/v2/tailnet/-/devices", nil, &out); err != nil {
		return false, err
	}
	found := false
	for _, d := range out.Devices {
		short, _, _ := strings.Cut(d.Name, ".")
		if d.Hostname != hostname && short != hostname {
			continue
		}
		tagged := false
		for _, tg := range d.Tags {
			tagged = tagged || tg == onboard.TailscaleTag
		}
		if !tagged {
			continue
		}
		if err := t.do(ctx, http.MethodDelete, "/api/v2/device/"+url.PathEscape(d.ID), nil, nil); err != nil {
			return found, err
		}
		found = true
	}
	return found, nil
}
