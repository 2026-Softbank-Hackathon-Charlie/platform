package deploy

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DefaultGitOpsRepo and DefaultGitOpsBranch are the gitops repository the
// infra ApplicationSet reads.
const (
	DefaultGitOpsRepo   = "2026-Softbank-Hackathon-Charlie/gitops"
	DefaultGitOpsBranch = "main"
)

// GitHubGitOps writes the gitops repository through the GitHub Git Data API
// (trees, commits, refs), so all files of a deployment land in one commit
// without a clone. The ref update is not forced: when another commit landed
// in between, it starts again from the new head.
type GitHubGitOps struct {
	Repo   string // owner/name
	Branch string
	// Token returns a token that may write the repository (the GitHub App
	// installation's).
	Token func(ctx context.Context) (string, error)
	// APIURL is the API root (default https://api.github.com).
	APIURL string
	HTTP   *http.Client // default: 30s timeout
	// Attempts bounds the retries after a non-fast-forward (default 5).
	Attempts int
}

// gitHubAPIError is a non-2xx answer.
type gitHubAPIError struct {
	Status  int
	Message string
}

func (e *gitHubAPIError) Error() string {
	return fmt.Sprintf("GitHub API %d: %s", e.Status, e.Message)
}

type treeEntry struct {
	Path string `json:"path"`
	Mode string `json:"mode"`
	Type string `json:"type"`
	SHA  string `json:"sha"`
}

// head is the branch head and its files.
type head struct {
	commit, tree string
	blobs        map[string]string // path -> blob sha, files only
}

func (g *GitHubGitOps) Write(ctx context.Context, files map[string][]byte, message string) (string, bool, error) {
	return g.update(ctx, message, func(h head) []map[string]any {
		var changes []map[string]any
		for _, p := range sortedKeys(files) {
			if h.blobs[p] != blobSHA(files[p]) {
				changes = append(changes, map[string]any{"path": p, "mode": "100644", "type": "blob", "content": string(files[p])})
			}
		}
		return changes
	})
}

func (g *GitHubGitOps) Delete(ctx context.Context, dirs []string, message string) (string, bool, error) {
	if err := checkDelete(dirs); err != nil {
		return "", false, err
	}
	return g.update(ctx, message, func(h head) []map[string]any {
		var changes []map[string]any
		for _, p := range sortedKeys(h.blobs) {
			if underAny(p, dirs) {
				changes = append(changes, map[string]any{"path": p, "mode": "100644", "type": "blob", "sha": nil})
			}
		}
		return changes
	})
}

// underAny reports whether path is under one of dirs.
func underAny(path string, dirs []string) bool {
	for _, d := range dirs {
		if strings.HasPrefix(path, strings.TrimSuffix(d, "/")+"/") {
			return true
		}
	}
	return false
}

// update builds a commit on the head from the tree changes diff returns and
// moves the branch to it, starting over when the branch moved meanwhile.
func (g *GitHubGitOps) update(ctx context.Context, message string, diff func(head) []map[string]any) (string, bool, error) {
	attempts := g.Attempts
	if attempts <= 0 {
		attempts = 5
	}
	for i := 1; ; i++ {
		h, err := g.head(ctx)
		if err != nil {
			return "", false, err
		}
		changes := diff(h)
		if len(changes) == 0 {
			return h.commit, false, nil
		}
		var tree struct{ SHA string }
		if err := g.do(ctx, http.MethodPost, "/git/trees", map[string]any{"base_tree": h.tree, "tree": changes}, &tree); err != nil {
			return "", false, fmt.Errorf("create tree: %w", err)
		}
		var commit struct{ SHA string }
		if err := g.do(ctx, http.MethodPost, "/git/commits", map[string]any{"message": message, "tree": tree.SHA, "parents": []string{h.commit}}, &commit); err != nil {
			return "", false, fmt.Errorf("create commit: %w", err)
		}
		err = g.do(ctx, http.MethodPatch, "/git/refs/heads/"+g.branch(), map[string]any{"sha": commit.SHA, "force": false}, nil)
		var ae *gitHubAPIError
		if errors.As(err, &ae) && (ae.Status == http.StatusUnprocessableEntity || ae.Status == http.StatusConflict) && i < attempts {
			select { // another commit landed first: build on it
			case <-ctx.Done():
				return "", false, ctx.Err()
			case <-time.After(time.Duration(i) * 200 * time.Millisecond):
			}
			continue
		}
		if err != nil {
			return "", false, fmt.Errorf("move %s to the new commit: %w", g.branch(), err)
		}
		return commit.SHA, true, nil
	}
}

func (g *GitHubGitOps) head(ctx context.Context) (head, error) {
	var ref struct {
		Object struct{ SHA string } `json:"object"`
	}
	if err := g.do(ctx, http.MethodGet, "/git/ref/heads/"+g.branch(), nil, &ref); err != nil {
		return head{}, fmt.Errorf("read branch %s of %s: %w", g.branch(), g.Repo, err)
	}
	var commit struct {
		Tree struct{ SHA string } `json:"tree"`
	}
	if err := g.do(ctx, http.MethodGet, "/git/commits/"+ref.Object.SHA, nil, &commit); err != nil {
		return head{}, fmt.Errorf("read commit %s: %w", short(ref.Object.SHA), err)
	}
	var tree struct {
		Tree      []treeEntry `json:"tree"`
		Truncated bool        `json:"truncated"`
	}
	if err := g.do(ctx, http.MethodGet, "/git/trees/"+commit.Tree.SHA+"?recursive=1", nil, &tree); err != nil {
		return head{}, fmt.Errorf("read tree of %s: %w", short(ref.Object.SHA), err)
	}
	if tree.Truncated {
		return head{}, fmt.Errorf("the tree of %s is too large for one GitHub API answer", g.Repo)
	}
	h := head{commit: ref.Object.SHA, tree: commit.Tree.SHA, blobs: map[string]string{}}
	for _, e := range tree.Tree {
		if e.Type == "blob" {
			h.blobs[e.Path] = e.SHA
		}
	}
	return h, nil
}

// Contains asks GitHub to compare the two commits.
func (g *GitHubGitOps) Contains(ctx context.Context, revision, commit string) (bool, error) {
	if revision == commit {
		return true, nil
	}
	var cmp struct {
		Status string `json:"status"` // ahead, behind, identical, diverged
	}
	err := g.do(ctx, http.MethodGet, "/compare/"+url.PathEscape(commit)+"..."+url.PathEscape(revision), nil, &cmp)
	var ae *gitHubAPIError
	if errors.As(err, &ae) && ae.Status == http.StatusNotFound {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return cmp.Status == "ahead" || cmp.Status == "identical", nil
}

func (g *GitHubGitOps) branch() string {
	if g.Branch == "" {
		return DefaultGitOpsBranch
	}
	return g.Branch
}

func (g *GitHubGitOps) do(ctx context.Context, method, path string, in, out any) error {
	token, err := g.Token(ctx)
	if err != nil {
		return fmt.Errorf("GitHub token: %w", err)
	}
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	api := strings.TrimRight(g.APIURL, "/")
	if api == "" {
		api = "https://api.github.com"
	}
	req, err := http.NewRequestWithContext(ctx, method, api+"/repos/"+g.Repo+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	hc := g.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("GitHub API %s %s: %w", method, strings.SplitN(path, "?", 2)[0], err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		var e struct{ Message string }
		_ = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&e)
		if e.Message == "" {
			e.Message = http.StatusText(resp.StatusCode)
		}
		return &gitHubAPIError{Status: resp.StatusCode, Message: e.Message}
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// blobSHA is git's object id of a blob with content b.
func blobSHA(b []byte) string {
	h := sha1.New()
	h.Write([]byte("blob " + strconv.Itoa(len(b)) + "\x00"))
	h.Write(b)
	return hex.EncodeToString(h.Sum(nil))
}
