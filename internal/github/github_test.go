package github

import (
	"crypto"
	"crypto/rand"
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
	"net/http/httptest"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

var (
	keyOnce sync.Once
	testKey *rsa.PrivateKey
)

func key(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	keyOnce.Do(func() {
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			panic(err)
		}
		testKey = k
	})
	return testKey
}

func keyPEM(t *testing.T) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key(t))})
}

const sha = "9fceb02d0ae598e95dc970b74767f19372d61af8"

// fakeGitHub answers the App endpoints the client uses.
type fakeGitHub struct {
	t   *testing.T
	now func() time.Time

	mu         sync.Mutex
	tokens     int // unscoped access tokens issued
	pushTokens int // access tokens issued with a body (repository_ids, permissions)
	pushBodies []string
	revoked    []string // tokens revoked with DELETE /installation/token
	lookups    int      // installation lookups
	tokenTTL   time.Duration
	installID  int64
	refs       map[string]string   // "owner/name@ref" -> sha
	files      map[string]string   // "owner/name@ref:path" -> content
	dirs       map[string]bool     // "owner/name@ref:path"
	meta       map[string]fakeRepo // "owner/name" as asked -> GET /repos answer; absent is 404
	repos      []string
	fail       int // answer every request with this status
	// scoped, when set, changes the answer to a scoped token request.
	scoped func(answer map[string]any)
}

// fakeRepo is GET /repos/{owner}/{name}: a renamed repository answers
// with its new full name.
type fakeRepo struct {
	id       int64
	fullName string
	topics   []string
}

func newFake(t *testing.T, now func() time.Time) (*fakeGitHub, *httptest.Server) {
	f := &fakeGitHub{t: t, now: now, tokenTTL: time.Hour, installID: 42,
		refs: map[string]string{}, files: map[string]string{}, dirs: map[string]bool{}, meta: map[string]fakeRepo{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeGitHub) token(n int) string     { return "ghs_test_" + strconv.Itoa(n) }
func (f *fakeGitHub) pushToken(n int) string { return "ghs_push_" + strconv.Itoa(n) }

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != 0 {
		http.Error(w, `{"message":"boom"}`, f.fail)
		return
	}
	if r.Header.Get("X-GitHub-Api-Version") == "" || r.Header.Get("User-Agent") == "" {
		f.t.Errorf("%s %s: missing API version or user agent", r.Method, r.URL.Path)
	}
	bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	path := r.URL.EscapedPath()
	switch {
	case r.Method == "GET" && path == "/orgs/acme/installation":
		f.checkJWT(bearer)
		f.lookups++
		writeJSON(w, map[string]any{"id": f.installID})
		return
	case r.Method == "POST" && path == fmt.Sprintf("/app/installations/%d/access_tokens", f.installID):
		f.checkJWT(bearer)
		body, _ := io.ReadAll(r.Body)
		tok := ""
		if len(body) == 0 {
			f.tokens++
			tok = f.token(f.tokens)
		} else {
			f.scopedToken(w, r, body)
			return
		}
		writeJSON(w, map[string]any{"token": tok, "expires_at": f.now().Add(f.tokenTTL).UTC().Format(time.RFC3339)})
		return
	case r.Method == "DELETE" && path == "/installation/token":
		for n := 1; n <= f.pushTokens; n++ {
			if bearer == f.pushToken(n) && !slices.Contains(f.revoked, bearer) {
				f.revoked = append(f.revoked, bearer)
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
		return
	}
	if bearer != f.token(f.tokens) || f.tokens == 0 {
		http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
		return
	}
	switch {
	case strings.HasPrefix(path, "/repos/") && strings.Count(path, "/") == 3:
		m, ok := f.meta[strings.TrimPrefix(r.URL.Path, "/repos/")]
		if !ok {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		owner, _, _ := strings.Cut(m.fullName, "/")
		writeJSON(w, map[string]any{"id": m.id, "full_name": m.fullName, "owner": map[string]any{"login": owner}, "private": true, "topics": m.topics})
	case strings.HasPrefix(path, "/repos/") && strings.Contains(path, "/commits/"):
		repo, ref, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/repos/"), "/commits/")
		if r.Header.Get("Accept") != "application/vnd.github.sha" {
			f.t.Errorf("commits: Accept %q", r.Header.Get("Accept"))
		}
		s, ok := f.refs[repo+"@"+ref]
		if !ok {
			http.Error(w, `{"message":"No commit found for SHA: `+ref+`"}`, http.StatusUnprocessableEntity)
			return
		}
		_, _ = w.Write([]byte(s))
	case strings.HasPrefix(path, "/repos/") && strings.Contains(path, "/contents/"):
		repo, p, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/repos/"), "/contents/")
		k := repo + "@" + r.URL.Query().Get("ref") + ":" + p
		content, ok := f.files[k]
		switch {
		case f.dirs[k]:
			writeJSON(w, map[string]any{"type": "dir", "entries": []any{}})
		case !ok:
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		case r.Header.Get("Accept") == "application/vnd.github.raw":
			_, _ = w.Write([]byte(content))
		case len(content) > 100: // stands in for GitHub's 1 MB
			writeJSON(w, map[string]any{"type": "file", "encoding": "none", "content": "", "size": len(content)})
		default:
			b64 := base64.StdEncoding.EncodeToString([]byte(content))
			writeJSON(w, map[string]any{"type": "file", "encoding": "base64", "content": b64[:len(b64)/2] + "\n" + b64[len(b64)/2:], "size": len(content)})
		}
	case path == "/installation/repositories":
		per, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		var list []map[string]any
		for i := (page - 1) * per; i < len(f.repos) && i < page*per; i++ {
			list = append(list, map[string]any{"full_name": f.repos[i], "private": i%2 == 0, "default_branch": "main", "html_url": "https://github.com/" + f.repos[i]})
		}
		writeJSON(w, map[string]any{"total_count": len(f.repos), "repositories": list})
	default:
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
	}
}

// scopedToken answers a token request with repository_ids and permissions
// the way GitHub does: 422 for a repository the installation does not have,
// else the token with the repositories and the permissions plus metadata
// read. f.mu is held.
func (f *fakeGitHub) scopedToken(w http.ResponseWriter, r *http.Request, body []byte) {
	if r.Header.Get("Content-Type") != "application/json" {
		f.t.Errorf("access token body sent as %q", r.Header.Get("Content-Type"))
	}
	f.pushBodies = append(f.pushBodies, string(body))
	var req struct {
		RepositoryIDs []int64           `json:"repository_ids"`
		Permissions   map[string]string `json:"permissions"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		f.t.Errorf("access token body %s: %v", body, err)
	}
	var repos []map[string]any
	for _, id := range req.RepositoryIDs {
		found := false
		for _, m := range f.meta {
			if m.id == id && !found {
				repos = append(repos, map[string]any{"id": id, "full_name": m.fullName})
				found = true
			}
		}
		if !found {
			http.Error(w, `{"message":"There is at least one repository that does not exist or is not accessible to the parent installation."}`, http.StatusUnprocessableEntity)
			return
		}
	}
	perms := map[string]any{"metadata": "read"}
	for k, v := range req.Permissions {
		perms[k] = v
	}
	f.pushTokens++
	answer := map[string]any{"token": f.pushToken(f.pushTokens), "expires_at": f.now().Add(f.tokenTTL).UTC().Format(time.RFC3339),
		"repository_selection": "selected", "repositories": repos, "permissions": perms}
	if f.scoped != nil {
		f.scoped(answer)
	}
	writeJSON(w, answer)
}

// checkJWT verifies the App JWT: RS256 with the App key, iss the App id,
// issued 60 seconds ago and expiring at most 10 minutes after now.
func (f *fakeGitHub) checkJWT(tok string) {
	f.t.Helper()
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		f.t.Errorf("JWT %q has %d parts", tok, len(parts))
		return
	}
	enc := base64.RawURLEncoding
	var header struct{ Alg, Typ string }
	h, _ := enc.DecodeString(parts[0])
	if err := json.Unmarshal(h, &header); err != nil || header.Alg != "RS256" {
		f.t.Errorf("JWT header %s", h)
	}
	sig, _ := enc.DecodeString(parts[2])
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(&key(f.t).PublicKey, crypto.SHA256, sum[:], sig); err != nil {
		f.t.Errorf("JWT signature: %v", err)
	}
	var claims struct {
		Iat, Exp int64
		Iss      string
	}
	c, _ := enc.DecodeString(parts[1])
	if err := json.Unmarshal(c, &claims); err != nil {
		f.t.Errorf("JWT claims %s: %v", c, err)
	}
	now := f.now().Unix()
	if claims.Iss != "1234" || claims.Iat != now-60 || claims.Exp <= now || claims.Exp-now > 600 {
		f.t.Errorf("JWT claims %s at %d", c, now)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// clock is a settable test clock.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newClient(t *testing.T, installation int64) (*Client, *fakeGitHub, *clock) {
	t.Helper()
	clk := &clock{t: time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)}
	f, srv := newFake(t, clk.now)
	c, err := New(Config{AppID: 1234, PrivateKey: keyPEM(t), InstallationID: installation, Org: "acme", APIURL: srv.URL + "/", Now: clk.now})
	if err != nil {
		t.Fatal(err)
	}
	return c, f, clk
}

func TestTokenIsCachedAndRefreshedBeforeExpiry(t *testing.T) {
	cases := []struct {
		name         string
		installation int64
		wantLookups  int
	}{
		{"configured installation", 42, 0},
		{"installation found from the org", 0, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, f, clk := newClient(t, tc.installation)
			ctx := t.Context()
			steps := []struct {
				advance    time.Duration
				wantToken  string
				wantIssued int
			}{
				{0, "ghs_test_1", 1},
				{30 * time.Minute, "ghs_test_1", 1}, // 30m left: cached
				{24 * time.Minute, "ghs_test_1", 1}, // 6m left: cached
				{90 * time.Second, "ghs_test_2", 2}, // 4m30s left: refreshed
				{10 * time.Second, "ghs_test_2", 2}, // the new one is cached
				{2 * time.Hour, "ghs_test_3", 3},    // expired: refreshed
			}
			for i, s := range steps {
				clk.add(s.advance)
				got, err := c.Token(ctx)
				if err != nil {
					t.Fatalf("step %d: %v", i, err)
				}
				if got != s.wantToken || f.tokens != s.wantIssued {
					t.Fatalf("step %d: token %q after %d issued; want %q after %d", i, got, f.tokens, s.wantToken, s.wantIssued)
				}
			}
			if f.lookups != tc.wantLookups {
				t.Fatalf("installation looked up %d times, want %d", f.lookups, tc.wantLookups)
			}
		})
	}
}

func TestTokenConcurrentCallersShareOne(t *testing.T) {
	c, f, _ := newClient(t, 42)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if _, err := c.Token(t.Context()); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if f.tokens != 1 {
		t.Fatalf("%d tokens issued, want 1", f.tokens)
	}
}

func TestTokenErrors(t *testing.T) {
	c, f, _ := newClient(t, 0)
	f.fail = http.StatusNotFound
	_, err := c.Token(t.Context())
	if err == nil || !strings.Contains(err.Error(), "installation on acme") {
		t.Fatalf("err = %v", err)
	}
	// Not installed is a broken configuration, not a missing repository.
	if _, err := c.ResolveRef(t.Context(), "acme/shop", "main"); err == nil || errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("ResolveRef without an installation: %v", err)
	}
	f.fail = 0
	if _, err := c.Token(t.Context()); err != nil {
		t.Fatalf("after recovery: %v", err)
	}
}

func TestResolveRef(t *testing.T) {
	c, f, _ := newClient(t, 42)
	f.refs["acme/shop@main"] = sha
	f.refs["acme/shop@feature/x"] = sha
	f.refs["acme/shop@v1.0"] = sha
	f.refs["acme/shop@bad"] = "not-a-sha"
	cases := []struct {
		repo, ref string
		notFound  bool
		wantErr   bool
	}{
		{repo: "acme/shop", ref: "main"},
		{repo: "acme/shop", ref: "feature/x"},
		{repo: "acme/shop", ref: "v1.0"},
		{repo: "acme/shop", ref: "gone", notFound: true, wantErr: true},
		{repo: "acme/none", ref: "main", notFound: true, wantErr: true},
		{repo: "acme/shop", ref: "bad", wantErr: true},
		{repo: "../shop", ref: "main", wantErr: true},
		{repo: "acme/..", ref: "main", wantErr: true},
		{repo: "acme/shop", ref: "../../orgs", wantErr: true},
		{repo: "acme/shop", ref: "a b", wantErr: true},
		{repo: "acme/shop", ref: "", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.repo+"@"+tc.ref, func(t *testing.T) {
			got, err := c.ResolveRef(t.Context(), tc.repo, tc.ref)
			if (err != nil) != tc.wantErr {
				t.Fatalf("got %q, %v", got, err)
			}
			if errors.Is(err, fs.ErrNotExist) != tc.notFound || errors.Is(err, ErrNotFound) != tc.notFound {
				t.Fatalf("not found = %v, want %v (%v)", errors.Is(err, ErrNotFound), tc.notFound, err)
			}
			if err == nil && got != sha {
				t.Fatalf("got %q", got)
			}
		})
	}

	f.fail = http.StatusBadGateway
	if _, err := c.ResolveRef(t.Context(), "acme/shop", "main"); err == nil || errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a 502 is %v, want an error that is not not-found", err)
	}
}

func TestReadFile(t *testing.T) {
	c, f, _ := newClient(t, 42)
	big := strings.Repeat("x", 500)
	f.files["acme/shop@"+sha+":platform.yaml"] = "name: shop\n"
	f.files["acme/shop@"+sha+":web/Dockerfile"] = "FROM scratch\n"
	f.files["acme/shop@"+sha+":big.txt"] = big
	f.dirs["acme/shop@"+sha+":web"] = true
	cases := []struct {
		path, want string
		notFound   bool
		wantErr    bool
	}{
		{path: "platform.yaml", want: "name: shop\n"},
		{path: "web/Dockerfile", want: "FROM scratch\n"},
		{path: "big.txt", want: big},
		{path: "Dockerfile", notFound: true, wantErr: true},
		{path: "web", notFound: true, wantErr: true},
		{path: "../etc/passwd", wantErr: true},
		{path: "/etc/passwd", wantErr: true},
		{path: "./Dockerfile", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			got, err := c.ReadFile(t.Context(), "acme/shop", sha, tc.path)
			if (err != nil) != tc.wantErr || errors.Is(err, fs.ErrNotExist) != tc.notFound {
				t.Fatalf("got %q, %v", got, err)
			}
			if string(got) != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestListReposPaginates(t *testing.T) {
	c, f, _ := newClient(t, 42)
	for i := 250; i > 0; i-- { // GitHub's order is not by name
		f.repos = append(f.repos, fmt.Sprintf("acme/repo-%03d", i))
	}
	got, err := c.ListRepos(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 250 {
		t.Fatalf("%d repos, want 250", len(got))
	}
	for i, r := range got {
		if want := fmt.Sprintf("acme/repo-%03d", i+1); r.FullName != want || r.DefaultBranch != "main" || r.HTMLURL != "https://github.com/"+want {
			t.Fatalf("repo %d = %+v, want %s", i, r, want)
		}
	}

	f.repos = nil
	if got, err := c.ListRepos(t.Context()); err != nil || len(got) != 0 {
		t.Fatalf("no repos: %v, %v", got, err)
	}
}

func TestPushToken(t *testing.T) {
	const scope = `{"permissions":{"contents":"write"},"repository_ids":[7]}`
	shop := RepoInfo{ID: 7, FullName: "acme/shop", Owner: "acme"}
	cases := []struct {
		name         string
		installation int64
		repo         RepoInfo
		warm         bool // Token was called first, so a token is cached
		wantErr      error
		wantAnyErr   bool
	}{
		{name: "cached token kept", installation: 42, repo: shop, warm: true},
		{name: "nothing cached", installation: 42, repo: shop},
		{name: "installation found from the org", repo: shop},
		{name: "owner in another case", installation: 42, repo: RepoInfo{ID: 7, FullName: "ACME/shop", Owner: "Acme"}},
		{name: "another org", installation: 42, repo: RepoInfo{ID: 7, FullName: "other/shop", Owner: "other"}, wantErr: ErrNotInOrg},
		{name: "org name as a suffix", installation: 42, repo: RepoInfo{ID: 7, FullName: "notacme/shop", Owner: "notacme"}, wantErr: ErrNotInOrg},
		{name: "owner login elsewhere", installation: 42, repo: RepoInfo{ID: 7, FullName: "acme/shop", Owner: "other"}, wantErr: ErrNotInOrg},
		{name: "no id", installation: 42, repo: RepoInfo{FullName: "acme/shop", Owner: "acme"}, wantAnyErr: true},
		{name: "not owner/name", installation: 42, repo: RepoInfo{ID: 7, FullName: "shop", Owner: "acme"}, wantAnyErr: true},
		{name: "dot dot", installation: 42, repo: RepoInfo{ID: 7, FullName: "acme/..", Owner: "acme"}, wantAnyErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, f, clk := newClient(t, tc.installation)
			f.meta["acme/shop"] = fakeRepo{id: 7, fullName: "acme/shop"}
			ctx := t.Context()
			var cached string
			if tc.warm {
				var err error
				if cached, err = c.Token(ctx); err != nil {
					t.Fatal(err)
				}
			}
			got, err := c.PushToken(ctx, tc.repo)
			wantErr := tc.wantErr != nil || tc.wantAnyErr
			if (err != nil) != wantErr || (tc.wantErr != nil && !errors.Is(err, tc.wantErr)) || errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("got %+v, %v", got, err)
			}
			if wantErr {
				if len(f.pushBodies) != 0 || f.lookups != 0 {
					t.Fatalf("refused, but asked GitHub: %d token requests, %d lookups", len(f.pushBodies), f.lookups)
				}
				return
			}
			if got.Token != "ghs_push_1" || !got.ExpiresAt.Equal(clk.now().Add(time.Hour)) {
				t.Fatalf("got %+v", got)
			}
			if len(f.pushBodies) != 1 {
				t.Fatalf("%d scoped token requests", len(f.pushBodies))
			}
			var body, want any
			_ = json.Unmarshal([]byte(f.pushBodies[0]), &body)
			_ = json.Unmarshal([]byte(scope), &want)
			if !reflect.DeepEqual(body, want) {
				t.Fatalf("access token body %s, want %s", f.pushBodies[0], scope)
			}
			if len(f.revoked) != 0 {
				t.Fatalf("revoked %v", f.revoked)
			}
			// The cached, unscoped token is neither handed out nor replaced.
			now, err := c.Token(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if tc.warm && now != cached {
				t.Fatalf("cached token %q became %q", cached, now)
			}
			if now != "ghs_test_1" || f.tokens != 1 {
				t.Fatalf("Token = %q after %d unscoped tokens, want ghs_test_1 after 1", now, f.tokens)
			}
			if f.lookups > 1 {
				t.Fatalf("installation looked up %d times", f.lookups)
			}
		})
	}
}

func TestPushTokenChecksTheAnswer(t *testing.T) {
	cases := []struct {
		name    string
		scoped  func(map[string]any)
		wantErr error // nil: accepted
	}{
		{name: "as asked", scoped: nil},
		{name: "no metadata permission", scoped: func(a map[string]any) { a["permissions"] = map[string]any{"contents": "write"} }},
		{name: "contents read only", scoped: func(a map[string]any) { a["permissions"] = map[string]any{"contents": "read", "metadata": "read"} }, wantErr: ErrUnexpectedScope},
		{name: "unscoped token", scoped: func(a map[string]any) {
			a["repository_selection"] = "all"
			a["permissions"] = map[string]any{"contents": "write", "metadata": "read", "administration": "write", "workflows": "write"}
			delete(a, "repositories")
		}, wantErr: ErrUnexpectedScope},
		{name: "extra permission", scoped: func(a map[string]any) {
			a["permissions"] = map[string]any{"contents": "write", "metadata": "read", "workflows": "write"}
		}, wantErr: ErrUnexpectedScope},
		{name: "wrong repository", scoped: func(a map[string]any) {
			a["repositories"] = []any{map[string]any{"id": 8, "full_name": "acme/gitops"}}
		}, wantErr: ErrUnexpectedScope},
		{name: "two repositories", scoped: func(a map[string]any) {
			a["repositories"] = []any{map[string]any{"id": 7, "full_name": "acme/shop"}, map[string]any{"id": 8, "full_name": "acme/gitops"}}
		}, wantErr: ErrUnexpectedScope},
		{name: "no repositories", scoped: func(a map[string]any) { a["repositories"] = []any{} }, wantErr: ErrUnexpectedScope},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, f, _ := newClient(t, 42)
			f.meta["acme/shop"] = fakeRepo{id: 7, fullName: "acme/shop"}
			f.scoped = tc.scoped
			got, err := c.PushToken(t.Context(), RepoInfo{ID: 7, FullName: "acme/shop", Owner: "acme"})
			if tc.wantErr == nil {
				if err != nil || got.Token != "ghs_push_1" || len(f.revoked) != 0 {
					t.Fatalf("got %+v, %v; revoked %v", got, err, f.revoked)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) || got.Token != "" || strings.Contains(err.Error(), "ghs_") {
				t.Fatalf("got %+v, %v", got, err)
			}
			if !slices.Equal(f.revoked, []string{"ghs_push_1"}) {
				t.Fatalf("revoked %v, want the token GitHub created", f.revoked)
			}
		})
	}
}

func TestPushTokenErrors(t *testing.T) {
	shop := RepoInfo{ID: 7, FullName: "acme/shop", Owner: "acme"}
	c, f, _ := newClient(t, 42)
	// Not in the installation: GitHub refuses the scope with 422.
	if _, err := c.PushToken(t.Context(), shop); !errors.Is(err, ErrNotInInstallation) || errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a repository the installation lacks: %v", err)
	}
	f.meta["acme/shop"] = fakeRepo{id: 7, fullName: "acme/shop"}
	for _, status := range []int{http.StatusInternalServerError, http.StatusForbidden, http.StatusNotFound} {
		f.fail = status
		_, err := c.PushToken(t.Context(), shop)
		if err == nil || errors.Is(err, ErrNotInInstallation) || errors.Is(err, fs.ErrNotExist) || strings.Contains(err.Error(), "ghs_") {
			t.Fatalf("a %d is %v, want an error that is neither not-found nor not-in-installation", status, err)
		}
	}
}

func TestRevokeToken(t *testing.T) {
	c, f, _ := newClient(t, 42)
	f.meta["acme/shop"] = fakeRepo{id: 7, fullName: "acme/shop"}
	tok, err := c.PushToken(t.Context(), RepoInfo{ID: 7, FullName: "acme/shop", Owner: "acme"})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.RevokeToken(t.Context(), tok.Token); err != nil || !slices.Equal(f.revoked, []string{tok.Token}) {
		t.Fatalf("revoke: %v, revoked %v", err, f.revoked)
	}
	if err := c.RevokeToken(t.Context(), tok.Token); err == nil || strings.Contains(err.Error(), tok.Token) {
		t.Fatalf("revoking twice: %v", err)
	}
	if err := c.RevokeToken(t.Context(), ""); err == nil {
		t.Fatal("revoked an empty token")
	}
	if f.tokens != 0 {
		t.Fatalf("RevokeToken created %d platform tokens", f.tokens)
	}
}

func TestReadRepo(t *testing.T) {
	c, f, _ := newClient(t, 42)
	f.meta["acme/shop"] = fakeRepo{id: 7, fullName: "acme/shop", topics: []string{"go", ImportedTopic}}
	f.meta["acme/plain"] = fakeRepo{id: 8, fullName: "acme/plain", topics: []string{}}
	f.meta["acme/old-name"] = fakeRepo{id: 9, fullName: "acme/new-name", topics: []string{ImportedTopic}}
	f.meta["acme/moved"] = fakeRepo{id: 10, fullName: "other/moved"}
	f.meta["acme/bad"] = fakeRepo{id: 0, fullName: "acme/bad"}
	cases := []struct {
		repo     string
		want     RepoInfo
		notFound bool
		wantErr  bool
	}{
		{repo: "acme/shop", want: RepoInfo{ID: 7, FullName: "acme/shop", Owner: "acme", Topics: []string{"go", ImportedTopic}}},
		{repo: "acme/plain", want: RepoInfo{ID: 8, FullName: "acme/plain", Owner: "acme", Topics: []string{}}},
		{repo: "acme/old-name", want: RepoInfo{ID: 9, FullName: "acme/new-name", Owner: "acme", Topics: []string{ImportedTopic}}},
		{repo: "acme/moved", want: RepoInfo{ID: 10, FullName: "other/moved", Owner: "other"}},
		{repo: "acme/bad", wantErr: true},
		{repo: "acme/none", notFound: true, wantErr: true},
		{repo: "../shop", wantErr: true},
		{repo: "acme/..", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.repo, func(t *testing.T) {
			got, err := c.ReadRepo(t.Context(), tc.repo)
			if (err != nil) != tc.wantErr || errors.Is(err, ErrNotFound) != tc.notFound || errors.Is(err, fs.ErrNotExist) != tc.notFound {
				t.Fatalf("got %+v, %v", got, err)
			}
			if !tc.wantErr && !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}

	f.fail = http.StatusBadGateway
	if _, err := c.ReadRepo(t.Context(), "acme/shop"); err == nil || errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a 502 is %v, want an error that is not not-found", err)
	}
}

func TestFromEnv(t *testing.T) {
	pemKey := string(keyPEM(t))
	cases := []struct {
		name    string
		env     map[string]string
		wantNil bool
		wantErr string
	}{
		{name: "unset", env: map[string]string{"PLATFORM_GITHUB_ORG": "acme"}, wantNil: true},
		{name: "configured", env: map[string]string{"PLATFORM_GITHUB_APP_ID": "1234", "PLATFORM_GITHUB_APP_PRIVATE_KEY": pemKey}},
		{name: "with installation", env: map[string]string{"PLATFORM_GITHUB_APP_ID": "1234", "PLATFORM_GITHUB_APP_PRIVATE_KEY": pemKey, "PLATFORM_GITHUB_INSTALLATION_ID": "42"}},
		{name: "no key", env: map[string]string{"PLATFORM_GITHUB_APP_ID": "1234"}, wantErr: "no private key"},
		{name: "no id", env: map[string]string{"PLATFORM_GITHUB_APP_PRIVATE_KEY": pemKey}, wantErr: "numeric id"},
		{name: "both keys", env: map[string]string{"PLATFORM_GITHUB_APP_ID": "1", "PLATFORM_GITHUB_APP_PRIVATE_KEY": pemKey, "PLATFORM_GITHUB_APP_PRIVATE_KEY_FILE": "k.pem"}, wantErr: "not both"},
		{name: "bad key", env: map[string]string{"PLATFORM_GITHUB_APP_ID": "1", "PLATFORM_GITHUB_APP_PRIVATE_KEY": "secret-not-pem"}, wantErr: "not PEM"},
		{name: "bad installation", env: map[string]string{"PLATFORM_GITHUB_APP_ID": "1", "PLATFORM_GITHUB_APP_PRIVATE_KEY": pemKey, "PLATFORM_GITHUB_INSTALLATION_ID": "x"}, wantErr: "installation id"},
		{name: "bad URL", env: map[string]string{"PLATFORM_GITHUB_APP_ID": "1", "PLATFORM_GITHUB_APP_PRIVATE_KEY": pemKey, "PLATFORM_GITHUB_API_URL": "api.github.com"}, wantErr: "http(s) URL"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := FromEnv(func(k string) string { return tc.env[k] })
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				if strings.Contains(err.Error(), "secret-not-pem") {
					t.Fatal("the error echoes the key")
				}
				return
			}
			if err != nil || (c == nil) != tc.wantNil {
				t.Fatalf("client %v, err %v", c, err)
			}
		})
	}
}
