package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// The reads onboarding's discovery makes of a repository (ADR 0042, "1.
// 조사"), with the installation token.

// RepoDefaultBranch returns repo's default branch. A repository the
// installation cannot see is ErrNotFound: the App is not installed on it.
func (c *Client) RepoDefaultBranch(ctx context.Context, repo string) (string, error) {
	p, err := repoPath(repo)
	if err != nil {
		return "", err
	}
	var r struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := c.getJSON(ctx, p, 1<<20, &r); err != nil {
		return "", fmt.Errorf("read %s: %w", repo, err)
	}
	if r.DefaultBranch == "" {
		return "", fmt.Errorf("github: %s has no default branch", repo)
	}
	return r.DefaultBranch, nil
}

// TreeEntry is one path of a commit's tree.
type TreeEntry struct {
	Path string
	Type string // blob, tree, commit
	Size int64
}

// RepoTree returns every path of the tree of commit sha. Truncated says
// GitHub cut the list (over 100,000 entries or 7 MB).
func (c *Client) RepoTree(ctx context.Context, repo, sha string) (entries []TreeEntry, truncated bool, err error) {
	p, err := repoPath(repo)
	if err != nil {
		return nil, false, err
	}
	if !shaHex.MatchString(sha) {
		return nil, false, fmt.Errorf("github: %q is not a commit sha", sha)
	}
	var r struct {
		Truncated bool `json:"truncated"`
		Tree      []struct {
			Path string `json:"path"`
			Type string `json:"type"`
			Size int64  `json:"size"`
		} `json:"tree"`
	}
	if err := c.getJSON(ctx, p+"/git/trees/"+sha+"?recursive=1", 16<<20, &r); err != nil {
		return nil, false, fmt.Errorf("read the tree of %s@%s: %w", repo, sha, err)
	}
	for _, e := range r.Tree {
		entries = append(entries, TreeEntry{Path: e.Path, Type: e.Type, Size: e.Size})
	}
	return entries, r.Truncated, nil
}

// ActionsWorkflow is one GitHub Actions workflow.
type ActionsWorkflow struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Path  string `json:"path"`
	State string `json:"state"`
}

// RepoWorkflows lists repo's workflows. Without the App's actions
// permission GitHub answers 403 (an *APIError).
func (c *Client) RepoWorkflows(ctx context.Context, repo string) ([]ActionsWorkflow, error) {
	p, err := repoPath(repo)
	if err != nil {
		return nil, err
	}
	var out []ActionsWorkflow
	for page := 1; page <= 10; page++ {
		var r struct {
			TotalCount int               `json:"total_count"`
			Workflows  []ActionsWorkflow `json:"workflows"`
		}
		if err := c.getJSON(ctx, p+"/actions/workflows?per_page=100&page="+strconv.Itoa(page), 4<<20, &r); err != nil {
			return nil, fmt.Errorf("list the workflows of %s: %w", repo, err)
		}
		out = append(out, r.Workflows...)
		if len(r.Workflows) == 0 || len(out) >= r.TotalCount {
			break
		}
	}
	return out, nil
}

// WorkflowRunInfo is one workflow run.
type WorkflowRunInfo struct {
	ID        int64     `json:"id"`
	HeadSHA   string    `json:"head_sha"`
	CreatedAt time.Time `json:"created_at"`
}

// WorkflowLastSuccess returns the newest successful run of a workflow, or
// nil when it has none.
func (c *Client) WorkflowLastSuccess(ctx context.Context, repo string, workflowID int64) (*WorkflowRunInfo, error) {
	p, err := repoPath(repo)
	if err != nil {
		return nil, err
	}
	var r struct {
		Runs []WorkflowRunInfo `json:"workflow_runs"`
	}
	path := p + "/actions/workflows/" + strconv.FormatInt(workflowID, 10) + "/runs?status=success&per_page=1"
	if err := c.getJSON(ctx, path, 4<<20, &r); err != nil {
		return nil, fmt.Errorf("list the runs of workflow %d of %s: %w", workflowID, repo, err)
	}
	if len(r.Runs) == 0 {
		return nil, nil
	}
	return &r.Runs[0], nil
}

// getJSON GETs an API path with the installation token and decodes it.
func (c *Client) getJSON(ctx context.Context, path string, limit int64, out any) error {
	req, err := c.request(ctx, http.MethodGet, path, "application/vnd.github+json")
	if err != nil {
		return err
	}
	body, err := c.send(req, limit)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("github: decode %s: %w", req.URL.Path, err)
	}
	return nil
}
