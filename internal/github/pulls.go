package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
)

// The calls an onboarding makes with the App's installation token (ADR
// 0042): its contract PR, and the deploy workflows of the source it stops.
// They need the App's pull_requests:write and actions:write ("한 번 하는
// 설정").

// PullRequest is one pull request as an onboarding reads it.
type PullRequest struct {
	Number  int    `json:"number"`
	URL     string `json:"html_url"`
	State   string `json:"state"` // open, closed
	Merged  bool   `json:"merged"`
	HeadSHA string `json:"-"`
	BaseRef string `json:"-"`
}

type prAnswer struct {
	PullRequest
	Head struct {
		SHA string `json:"sha"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

func (a prAnswer) pr() PullRequest {
	p := a.PullRequest
	p.HeadSHA, p.BaseRef = a.Head.SHA, a.Base.Ref
	return p
}

// api sends an installation-token request with in as its JSON body unless
// nil, and decodes a JSON answer into out unless nil.
func (c *Client) api(ctx context.Context, method, path string, in, out any) error {
	tok, err := c.Token(ctx)
	if err != nil {
		return err
	}
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("github: encode %s %s: %w", method, path, err)
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return fmt.Errorf("github: %w", err)
	}
	setHeaders(req, tok, "application/vnd.github+json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	raw, err := c.send(req, 4<<20)
	if err != nil {
		return err
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("github: decode %s %s: %w", method, path, err)
	}
	return nil
}

// OpenPR opens a pull request from head into base of repo, or returns the
// open one from head when there is one already.
func (c *Client) OpenPR(ctx context.Context, repo, head, base, title, body string) (PullRequest, error) {
	p, err := repoPath(repo)
	if err != nil {
		return PullRequest{}, err
	}
	if err := errors.Join(checkRef(head), checkRef(base)); err != nil {
		return PullRequest{}, err
	}
	owner, _, _ := cutRepo(repo)
	var open []prAnswer
	if err := c.api(ctx, http.MethodGet, p+"/pulls?state=open&head="+url.QueryEscape(owner+":"+head), nil, &open); err != nil {
		return PullRequest{}, fmt.Errorf("list the pull requests of %s: %w", repo, err)
	}
	if len(open) > 0 {
		return open[0].pr(), nil
	}
	var a prAnswer
	if err := c.api(ctx, http.MethodPost, p+"/pulls", map[string]string{"title": title, "head": head, "base": base, "body": body}, &a); err != nil {
		return PullRequest{}, fmt.Errorf("open a pull request on %s: %w", repo, err)
	}
	return a.pr(), nil
}

// GetPR returns a pull request; a missing one is ErrNotFound.
func (c *Client) GetPR(ctx context.Context, repo string, number int) (PullRequest, error) {
	p, err := repoPath(repo)
	if err != nil {
		return PullRequest{}, err
	}
	var a prAnswer
	if err := c.api(ctx, http.MethodGet, p+"/pulls/"+strconv.Itoa(number), nil, &a); err != nil {
		return PullRequest{}, notFound(err, repo)
	}
	return a.pr(), nil
}

// MergePR squash-merges a pull request only while its head is sha.
func (c *Client) MergePR(ctx context.Context, repo string, number int, sha string) error {
	p, err := repoPath(repo)
	if err != nil {
		return err
	}
	if !shaHex.MatchString(sha) {
		return fmt.Errorf("github: %q is not a commit sha", sha)
	}
	return c.api(ctx, http.MethodPut, p+"/pulls/"+strconv.Itoa(number)+"/merge", map[string]string{"merge_method": "squash", "sha": sha}, nil)
}

// ClosePR closes a pull request without merging it.
func (c *Client) ClosePR(ctx context.Context, repo string, number int) error {
	p, err := repoPath(repo)
	if err != nil {
		return err
	}
	return c.api(ctx, http.MethodPatch, p+"/pulls/"+strconv.Itoa(number), map[string]string{"state": "closed"}, nil)
}

// BehindBy is how many commits of base head does not have: 0 means a
// pull request from head is up to date with base.
func (c *Client) BehindBy(ctx context.Context, repo, base, head string) (int, error) {
	p, err := repoPath(repo)
	if err != nil {
		return 0, err
	}
	if err := errors.Join(checkRef(head), checkRef(base)); err != nil {
		return 0, err
	}
	var a struct {
		BehindBy int `json:"behind_by"`
	}
	if err := c.api(ctx, http.MethodGet, p+"/compare/"+escapePath(base)+"..."+escapePath(head), nil, &a); err != nil {
		return 0, notFound(err, repo)
	}
	return a.BehindBy, nil
}

// DisableWorkflow and EnableWorkflow turn a GitHub Actions workflow off and
// on by its id.
func (c *Client) DisableWorkflow(ctx context.Context, repo string, id int64) error {
	return c.workflow(ctx, repo, id, "disable")
}

// EnableWorkflow turns a workflow back on.
func (c *Client) EnableWorkflow(ctx context.Context, repo string, id int64) error {
	return c.workflow(ctx, repo, id, "enable")
}

func (c *Client) workflow(ctx context.Context, repo string, id int64, action string) error {
	p, err := repoPath(repo)
	if err != nil {
		return err
	}
	return c.api(ctx, http.MethodPut, p+"/actions/workflows/"+strconv.FormatInt(id, 10)+"/"+action, nil, nil)
}

// WorkflowRun is one run of a workflow.
type WorkflowRun struct {
	ID     int64  `json:"id"`
	Status string `json:"status"` // queued, in_progress, completed, ...
}

// ActiveRuns returns the runs of a workflow that have not completed.
func (c *Client) ActiveRuns(ctx context.Context, repo string, workflowID int64) ([]WorkflowRun, error) {
	p, err := repoPath(repo)
	if err != nil {
		return nil, err
	}
	var out []WorkflowRun
	for _, status := range []string{"queued", "in_progress", "waiting", "pending", "requested"} {
		var a struct {
			Runs []WorkflowRun `json:"workflow_runs"`
		}
		if err := c.api(ctx, http.MethodGet, p+"/actions/workflows/"+strconv.FormatInt(workflowID, 10)+"/runs?per_page=100&status="+status, nil, &a); err != nil {
			return nil, err
		}
		out = append(out, a.Runs...)
	}
	return out, nil
}

// CancelRun cancels a workflow run.
func (c *Client) CancelRun(ctx context.Context, repo string, runID int64) error {
	p, err := repoPath(repo)
	if err != nil {
		return err
	}
	return c.api(ctx, http.MethodPost, p+"/actions/runs/"+strconv.FormatInt(runID, 10)+"/cancel", nil, nil)
}

func cutRepo(repo string) (owner, name string, ok bool) {
	for i := range repo {
		if repo[i] == '/' {
			return repo[:i], repo[i+1:], true
		}
	}
	return "", "", false
}

func notFound(err error, repo string) error {
	var ae *APIError
	if errors.As(err, &ae) && ae.Status == http.StatusNotFound {
		return fmt.Errorf("%w: %s: %s", ErrNotFound, repo, ae.Message)
	}
	return err
}
