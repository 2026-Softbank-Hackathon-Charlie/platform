package deploy

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// isolateGit keeps the user's git configuration (signing, hooks, templates)
// out of the test's git commands.
func isolateGit(t *testing.T) {
	t.Helper()
	empty := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", empty)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
}

func gitCmd(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestGitCLIGitOps(t *testing.T) {
	isolateGit(t)
	ctx := t.Context()
	bare := filepath.Join(t.TempDir(), "gitops.git")
	gitCmd(t, "", "init", "-q", "--bare", bare)
	show := func(rev, path string) (string, bool) {
		cmd := exec.Command("git", "--git-dir", bare, "show", rev+":"+path)
		out, err := cmd.Output()
		return string(out), err == nil
	}

	g := &GitCLIGitOps{URL: bare, Branch: "main", Dir: filepath.Join(t.TempDir(), "clone")}
	// The first write creates the branch of an empty repository.
	sha1, changed, err := g.Write(ctx, map[string][]byte{"apps/shop/gke.yaml": []byte("a: 1\n"), "apps/shop/eks.yaml": []byte("a: 2\n")}, "deploy shop")
	if err != nil || !changed || len(sha1) != 40 {
		t.Fatalf("first write: %q %v %v", sha1, changed, err)
	}
	if got, _ := show("main", "apps/shop/gke.yaml"); got != "a: 1\n" {
		t.Fatalf("gke.yaml = %q", got)
	}
	if head := gitCmd(t, "", "--git-dir", bare, "rev-parse", "main"); head != sha1 {
		t.Fatalf("main is %s, want %s", head, sha1)
	}
	if msg := gitCmd(t, "", "--git-dir", bare, "log", "-1", "--format=%an <%ae> %s", "main"); msg != "HopCharlie platform <platform@hopcharlie.dev> deploy shop" {
		t.Fatalf("commit %q", msg)
	}

	// The same content again is no commit.
	again, changed, err := g.Write(ctx, map[string][]byte{"apps/shop/gke.yaml": []byte("a: 1\n")}, "deploy shop again")
	if err != nil || changed || again != sha1 {
		t.Fatalf("rewrite: %q %v %v", again, changed, err)
	}

	// Another writer's commit in between is kept.
	other := &GitCLIGitOps{URL: bare, Branch: "main", Dir: filepath.Join(t.TempDir(), "other")}
	sha2, _, err := other.Write(ctx, map[string][]byte{"apps/cart/gke.yaml": []byte("b: 1\n")}, "deploy cart")
	if err != nil {
		t.Fatal(err)
	}
	sha3, changed, err := g.Write(ctx, map[string][]byte{"apps/shop/gke.yaml": []byte("a: 3\n")}, "deploy shop 3")
	if err != nil || !changed {
		t.Fatalf("write after another writer: %v %v", changed, err)
	}
	if _, ok := show(sha3, "apps/cart/gke.yaml"); !ok {
		t.Fatal("the other writer's file is gone")
	}
	for _, tc := range []struct {
		rev, commit string
		want        bool
	}{
		{sha3, sha1, true}, {sha3, sha2, true}, {sha3, sha3, true}, {sha1, sha3, false},
		{strings.Repeat("0", 40), sha1, false},
	} {
		if got, err := g.Contains(ctx, tc.rev, tc.commit); err != nil || got != tc.want {
			t.Errorf("Contains(%s, %s) = %v, %v; want %v", short(tc.rev), short(tc.commit), got, err, tc.want)
		}
	}

	// A rejected push is retried.
	hook := filepath.Join(bare, "hooks", "pre-receive")
	script := "#!/bin/sh\nif [ ! -f rejected-once ]; then : > rejected-once; echo 'rejected once for the test' >&2; exit 1; fi\nexit 0\n"
	if err := os.WriteFile(hook, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	sha4, changed, err := g.Write(ctx, map[string][]byte{"apps/shop/gke.yaml": []byte("a: 4\n")}, "deploy shop 4")
	if err != nil || !changed {
		t.Fatalf("write through a rejection: %v %v", changed, err)
	}
	if _, err := os.Stat(filepath.Join(bare, "rejected-once")); err != nil {
		t.Fatal("the hook never rejected a push")
	}
	if got, _ := show("main", "apps/shop/gke.yaml"); got != "a: 4\n" || gitCmd(t, "", "--git-dir", bare, "rev-parse", "main") != sha4 {
		t.Fatalf("after retry: %q", got)
	}

	// Delete removes the app's directory only, once.
	sha5, changed, err := g.Delete(ctx, "apps/shop", "delete app shop")
	if err != nil || !changed {
		t.Fatalf("delete: %v %v", changed, err)
	}
	if _, ok := show(sha5, "apps/shop/gke.yaml"); ok {
		t.Fatal("apps/shop/gke.yaml is still there")
	}
	if _, ok := show(sha5, "apps/cart/gke.yaml"); !ok {
		t.Fatal("delete removed another app")
	}
	if again, changed, err := g.Delete(ctx, "apps/shop", "delete app shop"); err != nil || changed || again != sha5 {
		t.Fatalf("second delete: %q %v %v", again, changed, err)
	}
}
