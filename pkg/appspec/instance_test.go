package appspec

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
	"testing"
)

func TestBranchSlug(t *testing.T) {
	for _, c := range []struct{ branch, want string }{
		{"dev", "dev"},
		{"feature/Login_Page", "feature-login-page"},
		{"release/1.2", "release-1-2"},
		{"--x--", "x"},
		{"feature//x", "feature-x"},
		{"a._/-b", "a-b"},
		{"café", "caf"},
		{"MAIN", "main"},
		{"///", ""},
		{"한글", ""},
		{"", ""},
	} {
		if got := BranchSlug(c.branch); got != c.want {
			t.Errorf("BranchSlug(%q) = %q, want %q", c.branch, got, c.want)
		}
	}
}

func TestInstanceName(t *testing.T) {
	hash := func(branch string) string {
		sum := sha256.Sum256([]byte(branch))
		return hex.EncodeToString(sum[:])[:6]
	}
	app32 := "abcdefghijklmnopqrstuvwxyz012345"
	long := "feature/a-very-long-branch-name"
	dashAtCut := "featur/abcdefghijklmnop" // the first 7 slug characters end with "-"
	xs := strings.Repeat("x", 60)
	for _, c := range []struct {
		app, branch, want string
	}{
		{"moimroom", "dev", "moimroom--dev"},
		{"moimroom", "feature/Login_Page", "moimroom--feature-login-page"},
		{"moimroom", "release/1.2", "moimroom--release-1-2"},
		{"moimroom", "--x--", "moimroom--x"},
		// The longest name that is not shortened: 48 characters.
		{app32, "abcdefghijklmn", app32 + "--abcdefghijklmn"},
		// Shortened: exactly 48 characters, ending with the branch's hash.
		{app32, "abcdefghijklmno", app32 + "--abcdefg-" + hash("abcdefghijklmno")},
		{app32, long, app32 + "--feature-" + hash(long)},
		{app32, dashAtCut, app32 + "--featur-" + hash(dashAtCut)},
		{"moimroom", xs, "moimroom--" + strings.Repeat("x", 31) + "-" + hash(xs)},
	} {
		got, err := InstanceName(c.app, c.branch)
		if err != nil {
			t.Errorf("InstanceName(%q, %q): %v", c.app, c.branch, err)
			continue
		}
		if got != c.want {
			t.Errorf("InstanceName(%q, %q) = %q, want %q", c.app, c.branch, got, c.want)
		}
		if again, _ := InstanceName(c.app, c.branch); again != got {
			t.Errorf("InstanceName(%q, %q) = %q, then %q", c.app, c.branch, got, again)
		}
		if !dnsLabel.MatchString(got) || len(got) > MaxInstanceName {
			t.Errorf("InstanceName(%q, %q) = %q (%d characters) is not a DNS label of at most %d", c.app, c.branch, got, len(got), MaxInstanceName)
		}
	}
	if got, _ := InstanceName(app32, long); len(got) != MaxInstanceName {
		t.Errorf("shortened name %q has %d characters, want %d", got, len(got), MaxInstanceName)
	}

	// Branches with the same slug get the same name unless it is shortened:
	// the hash is of the branch as given.
	a, _ := InstanceName("moimroom", "feature/x")
	b, _ := InstanceName("moimroom", "Feature-X")
	if a != b || a != "moimroom--feature-x" {
		t.Errorf("feature/x and Feature-X name %q and %q, want both moimroom--feature-x", a, b)
	}
	a, _ = InstanceName(app32, long)
	b, _ = InstanceName(app32, strings.ToUpper(long))
	if a == b || a[:len(a)-instanceHash] != b[:len(b)-instanceHash] {
		t.Errorf("%q and %q name %q and %q, want the same prefix and different hashes", long, strings.ToUpper(long), a, b)
	}

	for _, branch := range []string{"///", "", "한글"} {
		if got, err := InstanceName("moimroom", branch); err == nil {
			t.Errorf("InstanceName(moimroom, %q) = %q, want an error", branch, got)
		}
	}
	for _, app := range []string{"", "ab", "a--b", "Shop", "-shop", strings.Repeat("a", 33), "shop/x"} {
		if got, err := InstanceName(app, "dev"); err == nil {
			t.Errorf("InstanceName(%q, dev) = %q, want an error for the app name", app, got)
		}
	}
}

var dnsLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
