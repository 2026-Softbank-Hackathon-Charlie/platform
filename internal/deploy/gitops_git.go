package deploy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// GitCLIGitOps writes a gitops repository with the git CLI, for the E2E
// tests' local git server (or any URL git can push to). It keeps one clone
// in a temporary directory and, for every write, fetches the branch, resets
// to it, writes, commits and pushes, starting over when the push is
// rejected. An empty repository gets the branch with the first commit.
type GitCLIGitOps struct {
	URL    string
	Branch string
	// Dir is the clone; empty means a new temporary directory.
	Dir string
	// Git is the command (default git).
	Git string
	// Attempts bounds the retries after a rejected push (default 5).
	Attempts int

	mu    sync.Mutex
	ready bool
}

const (
	gitName  = "HopCharlie platform"
	gitEmail = "platform@hopcharlie.dev"
)

func (g *GitCLIGitOps) Write(ctx context.Context, files map[string][]byte, message string) (string, bool, error) {
	return g.update(ctx, message, func() ([]string, error) {
		paths := sortedKeys(files)
		for _, p := range paths {
			full := filepath.Join(g.Dir, filepath.FromSlash(p))
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				return nil, err
			}
			if err := os.WriteFile(full, files[p], 0o644); err != nil {
				return nil, err
			}
		}
		return append([]string{"add", "--"}, paths...), nil
	})
}

func (g *GitCLIGitOps) Delete(ctx context.Context, dirs []string, message string) (string, bool, error) {
	if err := checkDelete(dirs); err != nil {
		return "", false, err
	}
	return g.update(ctx, message, func() ([]string, error) {
		args := []string{"rm", "-r", "-q", "--ignore-unmatch", "--"}
		for _, d := range dirs {
			args = append(args, strings.TrimSuffix(d, "/"))
		}
		return args, nil
	})
}

// update resets the clone to the remote branch, lets change edit the work
// tree and name the git command that stages the edit, then commits and
// pushes. Nothing staged means no commit.
func (g *GitCLIGitOps) update(ctx context.Context, message string, change func() ([]string, error)) (string, bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	attempts := g.Attempts
	if attempts <= 0 {
		attempts = 5
	}
	for i := 1; ; i++ {
		exists, err := g.reset(ctx)
		if err != nil {
			return "", false, err
		}
		stage, err := change()
		if err != nil {
			return "", false, err
		}
		if _, err := g.git(ctx, stage...); err != nil {
			return "", false, err
		}
		if _, err := g.git(ctx, "diff", "--cached", "--quiet"); err == nil {
			if !exists {
				return "", false, nil
			}
			sha, err := g.git(ctx, "rev-parse", "HEAD")
			return sha, false, err
		}
		if _, err := g.git(ctx, "-c", "user.name="+gitName, "-c", "user.email="+gitEmail, "commit", "-q", "-m", message); err != nil {
			return "", false, err
		}
		sha, err := g.git(ctx, "rev-parse", "HEAD")
		if err != nil {
			return "", false, err
		}
		if _, err := g.git(ctx, "push", "-q", "origin", "HEAD:refs/heads/"+g.branch()); err != nil {
			if i >= attempts || ctx.Err() != nil {
				return "", false, fmt.Errorf("push to %s: %w", g.branch(), err)
			}
			select { // rejected (someone pushed first) or a transient failure: start over
			case <-ctx.Done():
				return "", false, ctx.Err()
			case <-time.After(time.Duration(i) * 200 * time.Millisecond):
			}
			continue
		}
		return sha, true, nil
	}
}

// reset makes the clone the remote branch, or an empty unborn branch when the
// remote does not have it yet.
func (g *GitCLIGitOps) reset(ctx context.Context) (bool, error) {
	if err := g.init(ctx); err != nil {
		return false, err
	}
	ref := "refs/remotes/origin/" + g.branch()
	heads, err := g.git(ctx, "ls-remote", "--heads", "origin", "refs/heads/"+g.branch())
	if err != nil {
		return false, err
	}
	if heads == "" {
		_, _ = g.git(ctx, "update-ref", "-d", "refs/heads/"+g.branch())
		if _, err := g.git(ctx, "symbolic-ref", "HEAD", "refs/heads/"+g.branch()); err != nil {
			return false, err
		}
		if _, err := g.git(ctx, "read-tree", "--empty"); err != nil {
			return false, err
		}
		_, err := g.git(ctx, "clean", "-q", "-f", "-d", "-x")
		return false, err
	}
	if _, err := g.git(ctx, "fetch", "-q", "origin", "+refs/heads/"+g.branch()+":"+ref); err != nil {
		return false, err
	}
	if _, err := g.git(ctx, "checkout", "-q", "-f", "-B", g.branch(), ref); err != nil {
		return false, err
	}
	_, err = g.git(ctx, "clean", "-q", "-f", "-d", "-x")
	return true, err
}

func (g *GitCLIGitOps) init(ctx context.Context) error {
	if g.ready {
		return nil
	}
	if g.Dir == "" {
		dir, err := os.MkdirTemp("", "hopcharlie-gitops-")
		if err != nil {
			return err
		}
		g.Dir = dir
	} else if err := os.MkdirAll(g.Dir, 0o755); err != nil {
		return err
	}
	if _, err := g.git(ctx, "init", "-q"); err != nil {
		return err
	}
	if _, err := g.git(ctx, "remote", "add", "origin", g.URL); err != nil {
		if _, err := g.git(ctx, "remote", "set-url", "origin", g.URL); err != nil {
			return err
		}
	}
	g.ready = true
	return nil
}

// Contains fetches the branch and asks git whether commit is an ancestor of
// revision.
func (g *GitCLIGitOps) Contains(ctx context.Context, revision, commit string) (bool, error) {
	if revision == commit {
		return true, nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.init(ctx); err != nil {
		return false, err
	}
	if _, err := g.git(ctx, "fetch", "-q", "origin", "+refs/heads/"+g.branch()+":refs/remotes/origin/"+g.branch()); err != nil {
		return false, err
	}
	for _, c := range []string{revision, commit} {
		if _, err := g.git(ctx, "cat-file", "-e", c+"^{commit}"); err != nil {
			return false, nil // not in the branch's history
		}
	}
	_, err := g.git(ctx, "merge-base", "--is-ancestor", commit, revision)
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return false, nil
	}
	return err == nil, err
}

func (g *GitCLIGitOps) branch() string {
	if g.Branch == "" {
		return DefaultGitOpsBranch
	}
	return g.Branch
}

// git runs a git command in the clone and returns its trimmed output.
func (g *GitCLIGitOps) git(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, cmdOr(g.Git, "git"), args...)
	cmd.Dir = g.Dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && len(args) > 0 && (args[0] == "diff" || args[0] == "merge-base") {
			return "", err // the exit code is the answer
		}
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}
