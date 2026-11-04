// Package pgversion checks, before a migration starts, that the source and the
// target run the same PostgreSQL major version. The engine replicates
// physically (CNPG pg_basebackup and streaming), which works only within one
// major version; without this check a mismatch shows up minutes later as a
// target cluster that never becomes healthy.
package pgversion

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"sigs.k8s.io/yaml"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"
)

// Status of a check.
type Status string

const (
	Pass    Status = "pass"
	Fail    Status = "fail"
	Unknown Status = "unknown" // not enough information; never treated as a pass
)

// Codes of a Result.
const (
	CodeMatch         = "major_match"
	CodeMismatch      = "major_mismatch"
	CodeSourceUnknown = "source_unknown" // the source database could not be read
	CodeTargetUnknown = "target_unknown" // no reachable target and no manifest to read
	CodeImageDefault  = "image_default"  // the manifest sets no image: the operator's default decides
	CodeImageUnparsed = "image_unparsed" // the image tag carries no version, e.g. latest
)

// Version is one side's PostgreSQL version.
type Version struct {
	Major int `json:"major"`
	// Full is the version as read: "17.11" from a server, the image tag's
	// leading version from a manifest ("18.6"), or the catalog major ("17").
	Full string `json:"full"`
	// From says where it was read: "server", "manifest image <image>" or
	// "manifest image catalog".
	From string `json:"from"`
}

func (v *Version) String() string {
	if v == nil {
		return "unknown"
	}
	return fmt.Sprintf("%d (%s, from %s)", v.Major, v.Full, v.From)
}

// Result is the check's outcome. Message holds versions and an image name
// only, never a DSN or a server message.
type Result struct {
	Status  Status   `json:"status"`
	Code    string   `json:"code"`
	Message string   `json:"message"`
	Source  *Version `json:"source,omitempty"`
	Target  *Version `json:"target,omitempty"`
}

// Compare judges two versions. A missing side is Unknown with code, never a
// pass: the caller cannot tell a match from a mismatch.
func Compare(src, dst *Version, targetCode string) Result {
	r := Result{Source: src, Target: dst}
	switch {
	case src == nil:
		r.Status, r.Code = Unknown, CodeSourceUnknown
		r.Message = "source PostgreSQL version could not be read; the major versions were not compared"
	case dst == nil:
		r.Status, r.Code = Unknown, targetCode
		if r.Code == "" {
			r.Code = CodeTargetUnknown
		}
		r.Message = "source PostgreSQL " + src.String() + "; " + targetUnknownText(r.Code)
	case src.Major != dst.Major:
		r.Status, r.Code = Fail, CodeMismatch
		r.Message = fmt.Sprintf("source PostgreSQL %s and target %s differ in major version; physical replication needs the same major version (set the target's spec.imageName to a %d.x image)",
			src, dst, src.Major)
	default:
		r.Status, r.Code = Pass, CodeMatch
		r.Message = fmt.Sprintf("source PostgreSQL %s and target %s have the same major version", src, dst)
	}
	return r
}

func targetUnknownText(code string) string {
	switch code {
	case CodeImageDefault:
		return "the target manifest sets no spec.imageName or spec.imageCatalogRef, so the operator's default image decides the target version; set spec.imageName to the source's major version"
	case CodeImageUnparsed:
		return "the target manifest's image tag names no version; use a tag such as 17.11"
	default:
		return "the target's PostgreSQL version could not be read from its database or manifest"
	}
}

// Checker reads both versions of a plan and compares them.
type Checker struct {
	Timeout time.Duration // whole check; 0 means DefaultTimeout
}

// DefaultTimeout bounds a check.
const DefaultTimeout = 10 * time.Second

const appName = "hopcharlie-version-preflight"

// Check reads the source's version from its database, and the target's from
// its database when it is reachable (an existing replica) or else from the
// target manifest (a local file after plans.Service.Reserve). The error is
// only the context's.
func (c Checker) Check(ctx context.Context, p *plan.Plan) (Result, error) {
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	src := serverVersion(cctx, p.Source.DSN)
	dst := serverVersion(cctx, p.Target.DSN)
	code := ""
	if dst == nil {
		dst, code = ManifestVersion(p.Target.Manifest)
	}
	return Compare(src, dst, code), ctx.Err()
}

// serverVersion reads server_version_num (major*10000+minor since 10).
// Errors are dropped so DSNs and server messages stay out of the result.
func serverVersion(ctx context.Context, dsn string) *Version {
	if dsn == "" {
		return nil
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil
	}
	if cfg.ConnectTimeout == 0 || cfg.ConnectTimeout > 5*time.Second {
		cfg.ConnectTimeout = 5 * time.Second
	}
	if cfg.RuntimeParams == nil {
		cfg.RuntimeParams = map[string]string{}
	}
	cfg.RuntimeParams["default_transaction_read_only"] = "on"
	cfg.RuntimeParams["statement_timeout"] = "5000"
	cfg.RuntimeParams["application_name"] = appName
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil
	}
	defer func() {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		_ = conn.Close(cctx)
	}()
	var v string
	if conn.QueryRow(ctx, "SELECT current_setting('server_version_num')").Scan(&v) != nil {
		return nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 100000 {
		return nil
	}
	return &Version{Major: n / 10000, Full: fmt.Sprintf("%d.%d", n/10000, n%10000), From: "server"}
}

// tagVersion is the leading version of an image tag: 17, 17.11,
// 18.6-minimal-trixie, 17.11-202510010000.
var tagVersion = regexp.MustCompile(`^([0-9]{2})(\.[0-9]{1,4})?(?:[-_.].*)?$`)

// ManifestVersion reads the target's major version from a CNPG Cluster
// manifest file: spec.imageCatalogRef.major, or the tag of spec.imageName.
// Without either it returns nil and the code that says why; it never assumes
// the operator's default.
func ManifestVersion(path string) (*Version, string) {
	if path == "" {
		return nil, CodeTargetUnknown
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, CodeTargetUnknown
	}
	var doc struct {
		Spec struct {
			ImageName       string `json:"imageName"`
			ImageCatalogRef *struct {
				Major int `json:"major"`
			} `json:"imageCatalogRef"`
		} `json:"spec"`
	}
	if yaml.Unmarshal(raw, &doc) != nil {
		return nil, CodeTargetUnknown
	}
	if ref := doc.Spec.ImageCatalogRef; ref != nil {
		if ref.Major >= 10 {
			return &Version{Major: ref.Major, Full: strconv.Itoa(ref.Major), From: "manifest image catalog"}, ""
		}
		return nil, CodeImageUnparsed
	}
	if doc.Spec.ImageName == "" {
		return nil, CodeImageDefault
	}
	tag := imageTag(doc.Spec.ImageName)
	m := tagVersion.FindStringSubmatch(tag)
	if m == nil {
		return nil, CodeImageUnparsed
	}
	major, _ := strconv.Atoi(m[1])
	return &Version{Major: major, Full: m[1] + m[2], From: "manifest image " + doc.Spec.ImageName}, ""
}

// imageTag is the tag of an image reference, without a digest:
// registry:5000/repo:17.11@sha256:... -> 17.11.
func imageTag(image string) string {
	if i := strings.IndexByte(image, '@'); i >= 0 {
		image = image[:i]
	}
	slash := strings.LastIndexByte(image, '/')
	colon := strings.LastIndexByte(image, ':')
	if colon <= slash {
		return ""
	}
	return image[colon+1:]
}
