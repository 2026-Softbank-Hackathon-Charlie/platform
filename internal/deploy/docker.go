package deploy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
)

// DefaultCloneURL clones from GitHub with the installation token.
const DefaultCloneURL = "https://x-access-token:{token}@github.com/{repo}.git"

// DockerBuilder builds with the local git and docker CLIs, for E2E tests
// and local development: it clones the repository at the commit into a
// temporary directory, then builds, pushes and inspects
// <Registry>/hopcharlie/apps/<app>/<service>:<commit> per service.
type DockerBuilder struct {
	// Registry is where images are pushed, e.g. localhost:5001.
	Registry string
	// CloneURL is the repository to clone: {repo} becomes owner/name and
	// {token} the GitHub token. Empty means DefaultCloneURL; tests use a
	// local path such as /srv/git/{repo}.
	CloneURL string
	// WorkDir holds the temporary clones; empty means the system default.
	WorkDir string
	// Docker and Git are the commands to run (default docker and git).
	Docker, Git string
}

// Build clones, builds and pushes every service in turn.
func (b *DockerBuilder) Build(ctx context.Context, req BuildRequest, hooks BuildHooks) (BuildResult, error) {
	id := "docker-" + randomHex(6)
	if err := hooks.started(id); err != nil {
		return BuildResult{}, err
	}
	r := runner{token: req.Token, log: hooks.log}
	dir, err := os.MkdirTemp(b.WorkDir, "hopcharlie-build-")
	if err != nil {
		return BuildResult{}, err
	}
	defer os.RemoveAll(dir)

	tmpl := b.CloneURL
	if tmpl == "" {
		tmpl = DefaultCloneURL
	}
	url := strings.NewReplacer("{repo}", req.Repo, "{token}", req.Token).Replace(tmpl)
	src := filepath.Join(dir, "src")
	git := cmdOr(b.Git, "git")
	hooks.log("cloning " + req.Repo + " at " + req.Commit)
	if err := r.run(ctx, "", git, "clone", "--quiet", "--no-checkout", url, src); err != nil {
		return BuildResult{}, fmt.Errorf("clone %s: %w", req.Repo, err)
	}
	if err := r.run(ctx, src, git, "checkout", "--quiet", "--detach", req.Commit); err != nil {
		return BuildResult{}, fmt.Errorf("check out %s: %w", req.Commit, err)
	}

	docker := cmdOr(b.Docker, "docker")
	res := BuildResult{BuildID: id}
	for _, sv := range req.Services {
		repo := ImageRepository(b.Registry, req.App, sv.Name)
		tag := repo + ":" + req.Commit
		buildDir := filepath.Join(src, filepath.FromSlash(sv.Path))
		hooks.log("building " + sv.Name + " from " + sv.DockerfilePath())
		if err := r.run(ctx, src, docker, "build", "--progress=plain", "-t", tag, "-f", filepath.Join(buildDir, filepath.FromSlash(sv.Dockerfile)), buildDir); err != nil {
			return BuildResult{}, fmt.Errorf("docker build %s: %w", sv.Name, err)
		}
		if err := r.run(ctx, src, docker, "push", tag); err != nil {
			return BuildResult{}, fmt.Errorf("docker push %s: %w", tag, err)
		}
		out, err := r.output(ctx, docker, "image", "inspect", "--format", "{{json .RepoDigests}}", tag)
		if err != nil {
			return BuildResult{}, fmt.Errorf("docker image inspect %s: %w", tag, err)
		}
		image, err := pushedDigest(repo, out)
		if err != nil {
			return BuildResult{}, err
		}
		hooks.log("pushed " + image)
		res.Images = append(res.Images, store.DeploymentImage{Service: sv.Name, Image: image})
	}
	return res, nil
}

// pushedDigest finds repo@sha256:... in docker's RepoDigests JSON.
func pushedDigest(repo string, inspect []byte) (string, error) {
	var digests []string
	if err := json.Unmarshal(bytes.TrimSpace(inspect), &digests); err != nil {
		return "", fmt.Errorf("read the digest of %s: %w", repo, err)
	}
	for _, d := range digests {
		if name, digest, ok := strings.Cut(d, "@"); ok && name == repo && digestPattern.MatchString(digest) {
			return d, nil
		}
	}
	return "", fmt.Errorf("docker reports no digest of %s after the push (RepoDigests %v)", repo, digests)
}

// runner runs commands with their output going to the deployment's log and
// the token replaced wherever it would show.
type runner struct {
	token string
	log   func(string)
}

func (r runner) redact(s string) string {
	if r.token == "" {
		return s
	}
	return strings.ReplaceAll(s, r.token, "***")
}

// run streams the command's output line by line. Its error ends with the
// last lines of output.
func (r runner) run(ctx context.Context, dir, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "DOCKER_BUILDKIT=1")
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	var tail []string
	var wg sync.WaitGroup
	wg.Go(func() {
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			line := r.redact(sc.Text())
			r.log(line)
			if tail = append(tail, line); len(tail) > 5 {
				tail = tail[1:]
			}
		}
		_, _ = io.Copy(io.Discard, pr)
	})
	err := cmd.Run()
	pw.Close()
	wg.Wait()
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		msg := r.redact(err.Error())
		if len(tail) > 0 {
			msg += ": " + strings.Join(tail, " | ")
		}
		return errors.New(msg)
	}
	return nil
}

// output returns the command's standard output.
func (r runner) output(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, errors.New(r.redact(strings.TrimSpace(err.Error() + ": " + stderr.String())))
	}
	return out, nil
}

func cmdOr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
