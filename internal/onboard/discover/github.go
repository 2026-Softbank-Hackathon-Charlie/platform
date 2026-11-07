package discover

import (
	"context"
	"errors"
	"path"
	"regexp"
	"slices"
	"strings"

	gh "github.com/2026-Softbank-Hackathon-Charlie/platform/internal/github"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/onboard/facts"
)

// GitHub is the part of the GitHub App client discovery reads with
// (internal/github.Client).
type GitHub interface {
	RepoDefaultBranch(ctx context.Context, repo string) (string, error)
	ResolveRef(ctx context.Context, repo, ref string) (string, error)
	RepoTree(ctx context.Context, repo, sha string) ([]gh.TreeEntry, bool, error)
	ReadFile(ctx context.Context, repo, ref, path string) ([]byte, error)
	RepoWorkflows(ctx context.Context, repo string) ([]gh.ActionsWorkflow, error)
	WorkflowLastSuccess(ctx context.Context, repo string, workflowID int64) (*gh.WorkflowRunInfo, error)
}

var _ GitHub = (*gh.Client)(nil)

// Repo is what discovery read of the repository: Facts.GitHub, and the
// proxy configuration that names the public service.
type Repo struct {
	Facts facts.GitHub
	// ProxyTargets are the service:port pairs the repository's reverse
	// proxy configuration sends to (Caddyfile reverse_proxy, nginx
	// proxy_pass), in the order found.
	ProxyTargets []ServicePort
}

// ServicePort is a compose service and a port.
type ServicePort struct {
	Service string
	Port    int
}

var (
	composeFile = regexp.MustCompile(`(^|/)(docker-)?compose[^/]*\.ya?ml$`)
	dockerfile  = regexp.MustCompile(`(^|/)([^/]*\.)?Dockerfile(\.[^/]*)?$`)
	proxyConfig = regexp.MustCompile(`(?i)(^|/)(caddyfile[^/]*|[^/]*nginx[^/]*\.conf|conf\.d/[^/]+\.conf|default\.conf)$`)
	proxyTarget = regexp.MustCompile(`(?m)(?:reverse_proxy|proxy_pass\s+https?://|server)\s+([a-z0-9][a-z0-9_.-]*):([0-9]{2,5})\b`)

	// A workflow deploys to the VM when it reaches a host: ssh, scp or
	// rsync (a command or an action), a docker context or DOCKER_HOST over
	// ssh, SSM send-command, gcloud compute ssh, or a secret named like a
	// host or an ssh key; or runs docker compose on a self-hosted runner;
	// or calls a reusable workflow of the repository that does.
	remoteStep = regexp.MustCompile(`(?i)(\bssh\s|\bscp\s|\brsync\b|ssh-agent|DOCKER_HOST:\s*["']?ssh://|docker\s+context\b|aws\s+ssm\s+send-command|gcloud\s+compute\s+(ssh|scp)|uses:\s*["']?[^\s"']*(ssh|scp|rsync)[^\s"']*|secrets\.[A-Z0-9_]*(HOST|SSH|SERVER)[A-Z0-9_]*)`)
	localCall  = regexp.MustCompile(`uses:\s*["']?\./(\.github/workflows/[^\s"'@]+)`)
	composeRun = regexp.MustCompile(`docker[ -]compose`)
	selfHosted = regexp.MustCompile(`(?m)runs-on:.*self-hosted`)
)

// DeploysToVM is the heuristic of facts.Workflow.DeploysToVM on a
// workflow file's text.
func DeploysToVM(workflow string) bool {
	return remoteStep.MatchString(workflow) || (composeRun.MatchString(workflow) && selfHosted.MatchString(workflow))
}

// maxConfigFiles bounds the proxy configuration files read.
const maxConfigFiles = 8

// readRepo reads the repository. A repository the installation cannot see
// returns Installed false and no error.
func readRepo(ctx context.Context, g GitHub, repo string) (Repo, error) {
	out := Repo{Facts: facts.GitHub{ComposeFiles: []string{}, Dockerfiles: []string{}, Workflows: []facts.Workflow{}}}
	branch, err := g.RepoDefaultBranch(ctx, repo)
	if errors.Is(err, gh.ErrNotFound) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	head, err := g.ResolveRef(ctx, repo, branch)
	if err != nil {
		return out, err
	}
	out.Facts.Installed, out.Facts.DefaultBranch, out.Facts.Head = true, branch, head
	tree, _, err := g.RepoTree(ctx, repo, head)
	if err != nil {
		return out, err
	}
	var workflowFiles, configs []string
	for _, e := range tree {
		if e.Type != "blob" {
			continue
		}
		switch {
		case e.Path == "platform.yaml":
			out.Facts.PlatformYAML = true
		case composeFile.MatchString(e.Path):
			out.Facts.ComposeFiles = append(out.Facts.ComposeFiles, e.Path)
		case dockerfile.MatchString(e.Path):
			out.Facts.Dockerfiles = append(out.Facts.Dockerfiles, e.Path)
		case strings.HasPrefix(e.Path, ".github/workflows/") && (strings.HasSuffix(e.Path, ".yml") || strings.HasSuffix(e.Path, ".yaml")):
			workflowFiles = append(workflowFiles, e.Path)
		case proxyConfig.MatchString(e.Path) && e.Size < 64<<10:
			configs = append(configs, e.Path)
		}
	}
	// The workflows as Actions knows them; without the actions permission
	// (403), the files alone.
	byPath := map[string]gh.ActionsWorkflow{}
	if wfs, err := g.RepoWorkflows(ctx, repo); err == nil {
		for _, w := range wfs {
			byPath[w.Path] = w
		}
	} else if !isForbidden(err) {
		return out, err
	}
	texts := map[string]string{}
	for _, p := range workflowFiles {
		w := facts.Workflow{Path: p, Name: path.Base(p)}
		if a, ok := byPath[p]; ok {
			w.ID, w.Name, w.State = a.ID, a.Name, a.State
		}
		text, err := g.ReadFile(ctx, repo, head, p)
		if err != nil {
			return out, err
		}
		w.DeploysToVM = DeploysToVM(string(text))
		texts[p] = string(text)
		if w.ID != 0 {
			run, err := g.WorkflowLastSuccess(ctx, repo, w.ID)
			if err != nil && !isForbidden(err) {
				return out, err
			}
			if run != nil {
				w.LastSuccess = &facts.WorkflowRun{ID: run.ID, Commit: run.HeadSHA, At: run.CreatedAt}
			}
		}
		out.Facts.Workflows = append(out.Facts.Workflows, w)
	}
	markCallers(out.Facts.Workflows, texts)
	for _, p := range configs[:min(len(configs), maxConfigFiles)] {
		text, err := g.ReadFile(ctx, repo, head, p)
		if err != nil {
			return out, err
		}
		for _, m := range proxyTarget.FindAllStringSubmatch(string(text), -1) {
			sp := ServicePort{Service: m[1], Port: atoi(m[2])}
			if sp.Port > 0 && !slices.Contains(out.ProxyTargets, sp) {
				out.ProxyTargets = append(out.ProxyTargets, sp)
			}
		}
	}
	return out, nil
}

// markCallers marks the workflows that call a deploying reusable workflow
// of the repository (uses: ./.github/workflows/x.yml), transitively.
func markCallers(wfs []facts.Workflow, texts map[string]string) {
	deploys := map[string]bool{}
	for _, w := range wfs {
		deploys[w.Path] = w.DeploysToVM
	}
	for changed := true; changed; {
		changed = false
		for i, w := range wfs {
			if w.DeploysToVM {
				continue
			}
			for _, m := range localCall.FindAllStringSubmatch(texts[w.Path], -1) {
				if deploys[m[1]] {
					wfs[i].DeploysToVM, deploys[w.Path], changed = true, true, true
					break
				}
			}
		}
	}
}

func isForbidden(err error) bool {
	var ae *gh.APIError
	return errors.As(err, &ae) && ae.Status == 403
}
