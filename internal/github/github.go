// Package github is the platform's GitHub App client. It signs a JWT with the
// App's private key, exchanges it for an installation access token (cached
// until about five minutes before it expires) and reads the app repositories
// the installation can see: refs, files and the repository list. The token
// also authenticates git clone and the gitops commits.
package github

import (
	"bytes"
	"cmp"
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

// ErrNotFound means the repository, ref or file does not exist, or the
// installation cannot see it. It wraps fs.ErrNotExist, so callers can check
// errors.Is(err, fs.ErrNotExist) without importing this package.
var ErrNotFound = fmt.Errorf("github: not found: %w", fs.ErrNotExist)

// Defaults.
const (
	DefaultAPIURL = "https://api.github.com"
	DefaultOrg    = "2026-Softbank-Hackathon-Charlie"
)

// refreshBefore is how long before expiry a cached installation token is
// replaced. GitHub issues tokens for an hour.
const refreshBefore = 5 * time.Minute

// maxFileBytes bounds ReadFile.
const maxFileBytes = 32 << 20

// maxRepoPages bounds ListRepos at 100 repositories a page.
const maxRepoPages = 50

// Config configures the client.
type Config struct {
	AppID int64
	// PrivateKey is the App's private key, PEM (PKCS#1 as GitHub issues it, or PKCS#8).
	PrivateKey []byte
	// InstallationID is the App's installation; 0 looks it up for Org.
	InstallationID int64
	Org            string // default DefaultOrg
	APIURL         string // default DefaultAPIURL
	HTTP           *http.Client
	// Now is the clock; nil is time.Now.
	Now func() time.Time
}

// Repo is a repository the installation can see.
type Repo struct {
	FullName      string
	Private       bool
	DefaultBranch string
	HTMLURL       string
}

// Client is a GitHub App client. It is safe for concurrent use.
type Client struct {
	appID   string
	key     *rsa.PrivateKey
	org     string
	baseURL string
	http    *http.Client
	now     func() time.Time

	mu           sync.Mutex // serializes token refresh
	installation int64
	token        string
	expires      time.Time
}

// FromEnv builds a client from the environment (getenv is os.Getenv in the
// server). It returns nil and no error when neither PLATFORM_GITHUB_APP_ID
// nor a private key is set; setting only one of them is an error.
//
//	PLATFORM_GITHUB_APP_ID
//	PLATFORM_GITHUB_APP_PRIVATE_KEY (PEM) or PLATFORM_GITHUB_APP_PRIVATE_KEY_FILE
//	PLATFORM_GITHUB_INSTALLATION_ID (optional; found from the org when unset)
//	PLATFORM_GITHUB_ORG (default 2026-Softbank-Hackathon-Charlie)
//	PLATFORM_GITHUB_API_URL (default https://api.github.com)
func FromEnv(getenv func(string) string) (*Client, error) {
	id, key, keyFile := getenv("PLATFORM_GITHUB_APP_ID"), getenv("PLATFORM_GITHUB_APP_PRIVATE_KEY"), getenv("PLATFORM_GITHUB_APP_PRIVATE_KEY_FILE")
	if id == "" && key == "" && keyFile == "" {
		return nil, nil
	}
	cfg := Config{Org: getenv("PLATFORM_GITHUB_ORG"), APIURL: getenv("PLATFORM_GITHUB_API_URL")}
	var err error
	if cfg.AppID, err = strconv.ParseInt(id, 10, 64); err != nil || cfg.AppID <= 0 {
		return nil, errors.New("PLATFORM_GITHUB_APP_ID must be the GitHub App's numeric id")
	}
	switch {
	case key != "" && keyFile != "":
		return nil, errors.New("set PLATFORM_GITHUB_APP_PRIVATE_KEY or PLATFORM_GITHUB_APP_PRIVATE_KEY_FILE, not both")
	case key != "":
		cfg.PrivateKey = []byte(key)
	case keyFile != "":
		if cfg.PrivateKey, err = os.ReadFile(keyFile); err != nil {
			return nil, fmt.Errorf("PLATFORM_GITHUB_APP_PRIVATE_KEY_FILE: %w", err)
		}
	default:
		return nil, errors.New("PLATFORM_GITHUB_APP_ID is set but no private key: set PLATFORM_GITHUB_APP_PRIVATE_KEY or PLATFORM_GITHUB_APP_PRIVATE_KEY_FILE")
	}
	if v := getenv("PLATFORM_GITHUB_INSTALLATION_ID"); v != "" {
		if cfg.InstallationID, err = strconv.ParseInt(v, 10, 64); err != nil || cfg.InstallationID <= 0 {
			return nil, errors.New("PLATFORM_GITHUB_INSTALLATION_ID must be a numeric installation id")
		}
	}
	return New(cfg)
}

// New checks cfg and returns a client. It does not call GitHub.
func New(cfg Config) (*Client, error) {
	if cfg.AppID <= 0 {
		return nil, errors.New("github: the App id is required")
	}
	key, err := parseKey(cfg.PrivateKey)
	if err != nil {
		return nil, err
	}
	c := &Client{
		appID:        strconv.FormatInt(cfg.AppID, 10),
		key:          key,
		org:          cmp.Or(cfg.Org, DefaultOrg),
		baseURL:      strings.TrimRight(cmp.Or(cfg.APIURL, DefaultAPIURL), "/"),
		http:         cfg.HTTP,
		now:          cfg.Now,
		installation: cfg.InstallationID,
	}
	if u, err := url.Parse(c.baseURL); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, fmt.Errorf("github: API URL %q must be an http(s) URL", c.baseURL)
	}
	if c.http == nil {
		c.http = &http.Client{Timeout: 20 * time.Second}
	}
	if c.now == nil {
		c.now = time.Now
	}
	return c, nil
}

// parseKey reads an RSA private key without echoing any of it in errors.
func parseKey(p []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(bytes.TrimSpace(p))
	if block == nil {
		return nil, errors.New("github: the App private key is not PEM")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.New("github: the App private key is not a PKCS#1 or PKCS#8 RSA key")
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("github: the App private key is not an RSA key")
	}
	return rk, nil
}

// --- authentication ---

// jwt is the App's own token: RS256, issued 60 seconds in the past against
// clock drift, valid 10 minutes from then (GitHub's limit).
func (c *Client) jwt() (string, error) {
	now := c.now().Unix()
	enc := base64.RawURLEncoding
	header := enc.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, _ := json.Marshal(map[string]any{"iat": now - 60, "exp": now + 9*60, "iss": c.appID})
	signing := header + "." + enc.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(nil, c.key, crypto.SHA256, sum[:])
	if err != nil {
		return "", fmt.Errorf("github: sign the App JWT: %w", err)
	}
	return signing + "." + enc.EncodeToString(sig), nil
}

// Token returns an installation access token, for the API, git clone and
// the gitops commits. It is cached and replaced about five minutes before
// it expires, so a token it returns is good for at least that long. Its
// errors are never ErrNotFound: an App that is not installed is a broken
// configuration, not a missing repository.
func (c *Client) Token(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && c.expires.Sub(c.now()) > refreshBefore {
		return c.token, nil
	}
	jwt, err := c.jwt()
	if err != nil {
		return "", err
	}
	if c.installation == 0 {
		var inst struct {
			ID int64 `json:"id"`
		}
		if err := c.call(ctx, http.MethodGet, "/orgs/"+url.PathEscape(c.org)+"/installation", jwt, &inst); err != nil {
			return "", fmt.Errorf("github: find the App's installation on %s: %v", c.org, err)
		}
		if inst.ID <= 0 {
			return "", fmt.Errorf("github: no installation of the App on %s", c.org)
		}
		c.installation = inst.ID
	}
	var tok struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := c.call(ctx, http.MethodPost, "/app/installations/"+strconv.FormatInt(c.installation, 10)+"/access_tokens", jwt, &tok); err != nil {
		return "", fmt.Errorf("github: create an installation token: %v", err)
	}
	if tok.Token == "" || tok.ExpiresAt.IsZero() {
		return "", errors.New("github: the installation token answer has no token or expiry")
	}
	c.token, c.expires = tok.Token, tok.ExpiresAt
	return c.token, nil
}

// --- reads ---

var (
	repoName = regexp.MustCompile(`^[A-Za-z0-9-]+/[A-Za-z0-9._-]+$`)
	shaHex   = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)
)

// ResolveRef returns the full commit sha of a branch, tag or commit sha of
// repo (owner/name). A repository or ref that does not exist (or an empty
// repository) is ErrNotFound.
func (c *Client) ResolveRef(ctx context.Context, repo, ref string) (string, error) {
	p, err := repoPath(repo)
	if err != nil {
		return "", err
	}
	if err := checkRef(ref); err != nil {
		return "", err
	}
	req, err := c.request(ctx, http.MethodGet, p+"/commits/"+escapePath(ref), "application/vnd.github.sha")
	if err != nil {
		return "", err
	}
	body, err := c.send(req, 4<<10)
	if err != nil {
		var ae *APIError
		// 422: no such commit; 409: the repository is empty.
		if errors.As(err, &ae) && (ae.Status == http.StatusUnprocessableEntity || ae.Status == http.StatusConflict) {
			return "", fmt.Errorf("%w: %s@%s: %s", ErrNotFound, repo, ref, ae.Message)
		}
		return "", fmt.Errorf("resolve %s@%s: %w", repo, ref, err)
	}
	sha := strings.TrimSpace(string(body))
	if !shaHex.MatchString(sha) {
		return "", fmt.Errorf("github: resolve %s@%s: the answer is not a commit sha", repo, ref)
	}
	return sha, nil
}

// ReadFile returns the file at path (from the repository root) of repo at
// ref. A file that does not exist, or a path that is a directory or a
// submodule, is ErrNotFound.
func (c *Client) ReadFile(ctx context.Context, repo, ref, path string) ([]byte, error) {
	p, err := repoPath(repo)
	if err != nil {
		return nil, err
	}
	if err := checkRef(ref); err != nil {
		return nil, err
	}
	if err := checkFilePath(path); err != nil {
		return nil, err
	}
	endpoint := p + "/contents/" + escapePath(path) + "?ref=" + url.QueryEscape(ref)
	req, err := c.request(ctx, http.MethodGet, endpoint, "application/vnd.github.object+json")
	if err != nil {
		return nil, err
	}
	body, err := c.send(req, 4<<20) // the JSON carries files up to 1 MB, base64
	if err != nil {
		return nil, fmt.Errorf("read %s@%s:%s: %w", repo, ref, path, err)
	}
	var obj struct {
		Type     string `json:"type"`
		Encoding string `json:"encoding"`
		Content  string `json:"content"`
		Size     int64  `json:"size"`
	}
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, fmt.Errorf("github: read %s@%s:%s: decode: %w", repo, ref, path, err)
	}
	if obj.Type != "file" {
		return nil, fmt.Errorf("%w: %s@%s:%s is a %s, not a file", ErrNotFound, repo, ref, path, obj.Type)
	}
	if obj.Encoding == "base64" {
		data, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(obj.Content, "\n", ""))
		if err != nil {
			return nil, fmt.Errorf("github: read %s@%s:%s: decode content: %w", repo, ref, path, err)
		}
		return data, nil
	}
	// Over 1 MB the object has no content; ask for the raw bytes.
	if req, err = c.request(ctx, http.MethodGet, endpoint, "application/vnd.github.raw"); err != nil {
		return nil, err
	}
	data, err := c.send(req, maxFileBytes)
	if err != nil {
		return nil, fmt.Errorf("read %s@%s:%s: %w", repo, ref, path, err)
	}
	return data, nil
}

// ListRepos returns the repositories the installation can see, by full name.
func (c *Client) ListRepos(ctx context.Context) ([]Repo, error) {
	var out []Repo
	for page := 1; page <= maxRepoPages; page++ {
		var resp struct {
			TotalCount   int `json:"total_count"`
			Repositories []struct {
				FullName      string `json:"full_name"`
				Private       bool   `json:"private"`
				DefaultBranch string `json:"default_branch"`
				HTMLURL       string `json:"html_url"`
			} `json:"repositories"`
		}
		req, err := c.request(ctx, http.MethodGet, "/installation/repositories?per_page=100&page="+strconv.Itoa(page), "application/vnd.github+json")
		if err != nil {
			return nil, err
		}
		body, err := c.send(req, 8<<20)
		if err != nil {
			return nil, fmt.Errorf("list repositories: %w", err)
		}
		if err := json.Unmarshal(body, &resp); err != nil {
			return nil, fmt.Errorf("github: list repositories: decode: %w", err)
		}
		for _, r := range resp.Repositories {
			out = append(out, Repo{FullName: r.FullName, Private: r.Private, DefaultBranch: r.DefaultBranch, HTMLURL: r.HTMLURL})
		}
		if len(resp.Repositories) == 0 || len(out) >= resp.TotalCount {
			break
		}
	}
	slices.SortFunc(out, func(a, b Repo) int { return strings.Compare(a.FullName, b.FullName) })
	return out, nil
}

// --- HTTP ---

// APIError is a GitHub answer that is not a success. A 404 is ErrNotFound.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("github: HTTP %d", e.Status)
	}
	return fmt.Sprintf("github: HTTP %d: %s", e.Status, e.Message)
}

// Is makes a 404 match ErrNotFound and so fs.ErrNotExist.
func (e *APIError) Is(target error) bool {
	return e.Status == http.StatusNotFound && (target == ErrNotFound || target == fs.ErrNotExist)
}

// request builds an API request authenticated with the installation token.
func (c *Client) request(ctx context.Context, method, path, accept string) (*http.Request, error) {
	tok, err := c.Token(ctx)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, nil)
	if err != nil {
		return nil, fmt.Errorf("github: %w", err)
	}
	setHeaders(req, tok, accept)
	return req, nil
}

// call sends an App-authenticated (JWT) request and decodes the JSON answer.
func (c *Client) call(ctx context.Context, method, path, jwt string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, nil)
	if err != nil {
		return fmt.Errorf("github: %w", err)
	}
	setHeaders(req, jwt, "application/vnd.github+json")
	body, err := c.send(req, 1<<20)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("github: decode %s %s: %w", method, path, err)
	}
	return nil
}

func setHeaders(req *http.Request, bearer, accept string) {
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Accept", accept)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "hopcharlie-platform")
}

// send returns the body of a 2xx answer, at most limit bytes, or an
// *APIError. Errors never include the token.
func (c *Client) send(req *http.Request, limit int64) ([]byte, error) {
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("github: %s %s: %w", req.Method, req.URL.Path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("github: %s %s: read: %w", req.Method, req.URL.Path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var e struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(body, &e)
		return nil, &APIError{Status: resp.StatusCode, Message: e.Message}
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("github: %s %s: the answer exceeds %d bytes", req.Method, req.URL.Path, limit)
	}
	return body, nil
}

// --- paths ---

func repoPath(repo string) (string, error) {
	owner, name, _ := strings.Cut(repo, "/")
	if !repoName.MatchString(repo) || name == "." || name == ".." {
		return "", fmt.Errorf("github: %q is not owner/name", repo)
	}
	return "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name), nil
}

// checkRef refuses refs git does not allow and that could change the path
// of the request, such as "../..".
func checkRef(ref string) error {
	if ref == "" || len(ref) > 255 || strings.HasPrefix(ref, "/") || strings.HasSuffix(ref, "/") ||
		strings.Contains(ref, "..") || strings.Contains(ref, "//") || strings.ContainsFunc(ref, notRefChar) {
		return fmt.Errorf("github: %q is not a branch, tag or commit sha", ref)
	}
	return nil
}

func notRefChar(r rune) bool {
	return unicode.IsSpace(r) || unicode.IsControl(r) || strings.ContainsRune(`~^:?*[\`, r)
}

func checkFilePath(p string) error {
	if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, `\`) || strings.ContainsFunc(p, unicode.IsControl) {
		return fmt.Errorf("github: %q is not a path in the repository", p)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("github: %q is not a clean path in the repository", p)
		}
	}
	return nil
}

// escapePath escapes each segment and keeps the slashes.
func escapePath(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}
