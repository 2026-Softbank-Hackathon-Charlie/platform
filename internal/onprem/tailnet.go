package onprem

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"golang.org/x/oauth2/clientcredentials"
)

// Registration uses a separate OAuth client scoped to OnpremTailnetTag. The
// legacy compose onboarding client is deliberately not used as a fallback.
type Tailnet struct {
	HTTP            *http.Client
	BaseURL, Domain string
}

func TailnetFromEnv(ctx context.Context, getenv func(string) string) (*Tailnet, error) {
	id, secret := getenv("PLATFORM_ONPREM_TAILSCALE_OAUTH_CLIENT_ID"), getenv("PLATFORM_ONPREM_TAILSCALE_OAUTH_CLIENT_SECRET")
	domain := strings.TrimSuffix(getenv("PLATFORM_ONPREM_TAILNET_DOMAIN"), ".")
	if id == "" && secret == "" {
		return nil, nil
	}
	if id == "" || secret == "" || !strings.HasSuffix(domain, ".ts.net") || strings.ContainsAny(domain, "/:@ \r\n") {
		return nil, errors.New("onprem OAuth client and tailnet domain must be configured together")
	}
	cc := clientcredentials.Config{ClientID: id, ClientSecret: secret, TokenURL: "https://api.tailscale.com/api/v2/oauth/token"}
	c := cc.Client(ctx)
	c.Timeout = 20 * time.Second
	return &Tailnet{HTTP: c, Domain: domain}, nil
}

func (t *Tailnet) request(ctx context.Context, method, path string, input, output any) error {
	base := t.BaseURL
	if base == "" {
		base = "https://api.tailscale.com"
	}
	var body io.Reader
	if input != nil {
		b, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	r, err := http.NewRequestWithContext(ctx, method, base+path, body)
	if err != nil {
		return errors.New("invalid tailnet request")
	}
	r.Header.Set("Content-Type", "application/json")
	response, err := t.HTTP.Do(r)
	if err != nil {
		return errors.New("onprem tailnet request failed")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return fmt.Errorf("onprem tailnet HTTP %d", response.StatusCode)
	}
	if output != nil {
		if json.NewDecoder(io.LimitReader(response.Body, 8<<20)).Decode(output) != nil {
			return errors.New("invalid tailnet answer")
		}
	}
	return nil
}

func (t *Tailnet) AuthKey(ctx context.Context, hostname string) (string, error) {
	var out struct {
		Key string `json:"key"`
	}
	in := map[string]any{"capabilities": map[string]any{"devices": map[string]any{"create": map[string]any{"reusable": false, "ephemeral": false, "preauthorized": true, "tags": []string{OnpremTailnetTag}}}}, "expirySeconds": 3600, "description": "hopcharlie onprem " + hostname}
	if err := t.request(ctx, "POST", "/api/v2/tailnet/-/keys", in, &out); err != nil {
		return "", err
	}
	if !strings.HasPrefix(out.Key, "tskey-auth-") {
		return "", errors.New("tailnet auth key missing")
	}
	return out.Key, nil
}

func (t *Tailnet) Device(ctx context.Context, hostname string) (string, bool, error) {
	var out struct {
		Devices []struct {
			Hostname, Name string
			Tags           []string
			Authorized     bool
		} `json:"devices"`
	}
	if err := t.request(ctx, "GET", "/api/v2/tailnet/-/devices?fields=all", nil, &out); err != nil {
		return "", false, err
	}
	expected := hostname + "." + t.Domain
	found := false
	for _, d := range out.Devices {
		if d.Hostname != hostname || strings.TrimSuffix(d.Name, ".") != expected {
			continue
		}
		tagged := false
		for _, tag := range d.Tags {
			tagged = tagged || tag == OnpremTailnetTag
		}
		if !tagged || !d.Authorized {
			return "", false, errors.New("onprem tailnet device is not owned by this registration")
		}
		if found {
			return "", false, errors.New("duplicate onprem tailnet identity")
		}
		found = true
	}
	return expected, found, nil
}
