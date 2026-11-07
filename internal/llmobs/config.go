package llmobs

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type Binding struct {
	ProjectID     string `json:"projectId"`
	MetadataKey   string `json:"metadataKey"`
	PublicKeyFile string `json:"publicKeyFile"`
	SecretKeyFile string `json:"secretKeyFile"`
}
type Config struct {
	BaseURL   string             `json:"baseUrl"`
	UIBaseURL string             `json:"uiBaseUrl,omitempty"`
	Apps      map[string]Binding `json:"apps"`
}
type credentials struct{ public, secret string }
type Client struct {
	config             Config
	keys               map[string]credentials
	http               *http.Client
	configurationError bool
}

// InvalidConfiguration preserves the difference between absent and broken setup.
func InvalidConfiguration() *Client { return &Client{configurationError: true} }

var identifier = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
var metadataKey = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,80}$`)
var modelName = regexp.MustCompile(`^[A-Za-z0-9_.:/-]{1,100}$`)
var serverVersion = regexp.MustCompile(`^v?([0-9]{1,3})\.([0-9]{1,5})\.([0-9]{1,5})(?:[-+][A-Za-z0-9.-]{1,40})?$`)

func plainURL(raw string, internal bool) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("invalid Langfuse address")
	}
	if u.Scheme != "https" {
		host := u.Hostname()
		ip := net.ParseIP(host)
		if !internal || u.Scheme != "http" || !(host == "localhost" || ip != nil && ip.IsLoopback() || strings.HasSuffix(host, ".svc.cluster.local")) {
			return nil, errors.New("Langfuse requires HTTPS outside the cluster")
		}
	}
	u.Path = ""
	return u, nil
}

// Load reads an operator-owned config and mounted project key files only.
// No URL, project, credential or metadata filter is accepted from API callers.
func Load(path string) (*Client, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.New("Langfuse configuration cannot be read")
	}
	defer f.Close()
	var cfg Config
	dec := json.NewDecoder(io.LimitReader(f, 64<<10))
	dec.DisallowUnknownFields()
	if dec.Decode(&cfg) != nil {
		return nil, errors.New("invalid Langfuse configuration")
	}
	if dec.Decode(new(any)) != io.EOF {
		return nil, errors.New("invalid Langfuse configuration")
	}
	return New(cfg)
}

func New(cfg Config) (*Client, error) {
	base, err := plainURL(cfg.BaseURL, true)
	if err != nil {
		return nil, err
	}
	cfg.BaseURL = base.String()
	if cfg.UIBaseURL != "" {
		ui, err := plainURL(cfg.UIBaseURL, false)
		if err != nil {
			return nil, err
		}
		cfg.UIBaseURL = ui.String()
	}
	keys := map[string]credentials{}
	for app, b := range cfg.Apps {
		if !identifier.MatchString(app) || !identifier.MatchString(b.ProjectID) || !metadataKey.MatchString(b.MetadataKey) {
			return nil, errors.New("invalid Langfuse app binding")
		}
		pub, e1 := readKey(b.PublicKeyFile)
		secret, e2 := readKey(b.SecretKeyFile)
		if e1 != nil || e2 != nil {
			return nil, errors.New("Langfuse project keys cannot be read")
		}
		keys[app] = credentials{pub, secret}
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &Client{config: cfg, keys: keys, http: &http.Client{Transport: transport, Timeout: 8 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

func readKey(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", errors.New("key file must be absolute")
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil || len(b) > 4096 {
		return "", errors.New("invalid project key")
	}
	value := strings.TrimSpace(string(b))
	if len(value) < 8 || strings.ContainsAny(value, "\r\n") {
		return "", errors.New("invalid project key")
	}
	return value, nil
}
