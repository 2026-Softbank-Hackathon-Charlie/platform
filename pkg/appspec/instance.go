package appspec

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// Instances (ADR 0015). An app deploys its own branch as the default
// instance, named after the app, and every branch the operator adds as
// another instance named by InstanceName.
const (
	// MaxInstances is how many instances an app may have, the default one
	// included.
	MaxInstances = 3
	// MaxInstanceName is the longest instance name. The instance name is the
	// Helm release name, which Helm limits to 53 characters.
	MaxInstanceName = 53
)

// instanceHash is how many hex digits of the branch's SHA-256 a shortened
// instance name ends with.
const instanceHash = 6

// BranchSlug turns a branch name into the part of an instance name after
// "--": ASCII letters are lowercased, every other character that is not a
// letter or digit becomes "-", runs of "-" become one, and leading and
// trailing "-" are removed. The slug is empty when the branch has no ASCII
// letter or digit.
func BranchSlug(branch string) string {
	var b strings.Builder
	dash := false
	for _, r := range branch {
		switch {
		case 'A' <= r && r <= 'Z':
			r += 'a' - 'A'
		case 'a' <= r && r <= 'z', '0' <= r && r <= '9':
		default:
			if !dash {
				b.WriteByte('-')
				dash = true
			}
			continue
		}
		b.WriteRune(r)
		dash = false
	}
	return strings.Trim(b.String(), "-")
}

// InstanceName returns the instance name of a branch added to app, which
// must be a valid app name: app + "--" + BranchSlug(branch). A name longer
// than MaxInstanceName keeps as much of the slug as fits, without a trailing
// "-", and ends with "-" and the first 6 hex digits of the SHA-256 of the
// branch as given, so it is at most MaxInstanceName characters and stays a
// DNS label. Branches with the same slug get the same name. It is an error
// when the slug is empty.
func InstanceName(app, branch string) (string, error) {
	slug := BranchSlug(branch)
	if slug == "" {
		return "", fmt.Errorf("branch %q has no letter or digit to name an instance after", branch)
	}
	name := app + "--" + slug
	if len(name) <= MaxInstanceName {
		return name, nil
	}
	keep := MaxInstanceName - len(app) - len("--") - 1 - instanceHash
	if keep < 1 {
		return "", fmt.Errorf("app name %q is too long to name an instance after", app)
	}
	sum := sha256.Sum256([]byte(branch))
	return app + "--" + strings.TrimRight(slug[:keep], "-") + "-" + hex.EncodeToString(sum[:])[:instanceHash], nil
}
