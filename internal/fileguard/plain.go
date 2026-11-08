package fileguard

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/files"
)

// Plain is an engine.FileSyncer that runs rclone with the server's remotes
// and Env, without the guard's snapshot and inspection: for an onboarding's
// plan (ADR 0042), whose source is the app's own bucket outside the
// platform, reached through the remote legacy (RCLONE_CONFIG_LEGACY_*)
// that exists only in Env, never in the server's environment. Like run, it
// reads no config file and drops rclone's global flags from the
// environment; it also drops every *_PGPASSWORD.
type Plain struct {
	Env []string
	// exec runs rclone with args and env and returns its output (tests).
	exec func(ctx context.Context, env []string, args ...string) (string, error)
}

// NewPlain returns a Plain with env added to the server's environment.
func NewPlain(env []string) *Plain { return &Plain{Env: env} }

// Environment is what rclone runs with: the server's remote settings
// (RCLONE_CONFIG_*, RCLONE_S3_*, RCLONE_GCS_*, AWS_*, GOOGLE_* and the rest
// of the environment but rclone's global flags) and Env, without any
// *_PGPASSWORD.
func (p *Plain) Environment() []string {
	var out []string
	for _, v := range remoteEnvironment(append(os.Environ(), p.Env...)) {
		key, _, _ := strings.Cut(v, "=")
		if strings.HasSuffix(strings.ToUpper(key), "_PGPASSWORD") {
			continue
		}
		out = append(out, v)
	}
	return out
}

func (p *Plain) rclone(ctx context.Context, args ...string) (string, error) {
	args = append(args, "--config", os.DevNull)
	if p.exec != nil {
		return p.exec(ctx, p.Environment(), args...)
	}
	cmd := exec.CommandContext(ctx, "rclone", args...)
	cmd.Env = p.Environment()
	cmd.WaitDelay = 5 * time.Second
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return out.String(), err
}

// Available implements engine.FileSyncer.
func (p *Plain) Available() error {
	if p.exec != nil {
		return nil
	}
	if _, err := exec.LookPath("rclone"); err != nil {
		return ErrTransfer
	}
	return nil
}

func (p *Plain) transfer(ctx context.Context, op, src, dst string) (files.SyncResult, error) {
	start := time.Now()
	out, err := p.rclone(ctx, op, src, dst, "--stats-one-line", "--stats-log-level", "NOTICE", "--stats", "0")
	res := files.SyncResult{Duration: time.Since(start), Output: tailOf(out, 2000)}
	if err != nil {
		return res, fmt.Errorf("rclone %s %s -> %s: %w: %s", op, src, dst, err, tailOf(out, 500))
	}
	return res, nil
}

// Sync implements engine.FileSyncer.
func (p *Plain) Sync(ctx context.Context, src, dst string) (files.SyncResult, error) {
	return p.transfer(ctx, "sync", src, dst)
}

// Copy implements engine.FileSyncer.
func (p *Plain) Copy(ctx context.Context, src, dst string) (files.SyncResult, error) {
	return p.transfer(ctx, "copy", src, dst)
}

var (
	differencesFound = regexp.MustCompile(`(\d+) differences found`)
	matchingFiles    = regexp.MustCompile(`(\d+) matching files`)
	missingFiles     = regexp.MustCompile(`(\d+) files missing`)
)

// Check implements engine.FileSyncer (rclone check --one-way).
func (p *Plain) Check(ctx context.Context, src, dst string) (files.CheckResult, error) {
	out, err := p.rclone(ctx, "check", src, dst, "--one-way")
	res := files.CheckResult{OK: err == nil, Output: tailOf(out, 2000)}
	num := func(re *regexp.Regexp) int {
		if m := re.FindStringSubmatch(out); m != nil {
			n, _ := strconv.Atoi(m[1])
			return n
		}
		return 0
	}
	res.Differences, res.Matching, res.Missing = num(differencesFound), num(matchingFiles), num(missingFiles)
	if err != nil && res.Differences == 0 && res.Missing == 0 {
		return res, fmt.Errorf("rclone check %s -> %s: %w: %s", src, dst, err, tailOf(out, 500))
	}
	return res, nil
}

// Readable implements engine.FileSyncer.
func (p *Plain) Readable(ctx context.Context, remote string) error {
	if out, err := p.rclone(ctx, "lsf", remote, "--max-depth", "1"); err != nil {
		return fmt.Errorf("rclone lsf %s: %w: %s", remote, err, tailOf(out, 500))
	}
	return nil
}

func tailOf(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
