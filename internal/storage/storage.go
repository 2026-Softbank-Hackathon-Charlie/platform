// Package storage makes each app instance's bucket and a key that can use
// only that bucket (ADR 0016). An environment's driver decides how: aws
// makes an S3 bucket and an IAM user with an inline policy and an access
// key; minio makes a MinIO bucket and a user with a canned policy and a
// service account as the key. The platform seals the key for the cluster
// and keeps only the sealed key and the key id.
//
// The minio driver links the MinIO admin client (madmin-go, AGPL-3.0), so it
// is only in binaries built with -tags minio (the kind E2E and on-prem
// builds). The default build, which the published image uses, refuses a
// minio storage block (MinIOBuilt).
//
// Every operation is idempotent: what already exists and belongs to the
// same app is kept, and what is already gone is skipped, so a caller may
// retry after any failure.
package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// Drivers.
const (
	DriverAWS   = "aws"
	DriverMinIO = "minio"
	// DriverGCS is planned (ADR 0016) and not implemented.
	DriverGCS = "gcs"
)

// MaxBucketPrefix is the longest bucket prefix: with the longest instance
// name (48 characters, ADR 0016) the bucket name stays within S3's 63.
const MaxBucketPrefix = 15

// UserPrefix starts the name of every user the drivers make (and, for
// minio, of the policy): "hopcharlie-app-<instance>".
const UserPrefix = "hopcharlie-app-"

// Tag keys on buckets (and on IAM users), naming the app and the instance
// they were made for. A driver never takes over or deletes a bucket or user
// whose app tag names another app.
const (
	TagApp      = "hopcharlie:app"
	TagInstance = "hopcharlie:instance"
)

// Config is an environment's storage driver, the storage block of
// PLATFORM_ENVS_FILE.
type Config struct {
	// Driver is aws or minio.
	Driver string `json:"driver"`
	// BucketPrefix starts every bucket name: the bucket of an instance is
	// <bucketPrefix><instance>. At most MaxBucketPrefix characters.
	BucketPrefix string `json:"bucketPrefix"`
	// Region is the buckets' region; required for aws, optional for minio
	// (the server's default region).
	Region string `json:"region,omitempty"`

	// IAMPath is the path of the IAM users (aws), e.g. /hopcharlie-apps/.
	IAMPath string `json:"iamPath,omitempty"`
	// PermissionsBoundary is the ARN of the managed policy every IAM user
	// gets as its permissions boundary (aws).
	PermissionsBoundary string `json:"permissionsBoundary,omitempty"`

	// Endpoint is the MinIO server the platform calls, http(s)://host:port
	// (minio).
	Endpoint string `json:"endpoint,omitempty"`
	// AdminAccessKeyEnv and AdminSecretKeyEnv name the environment
	// variables of the platform API that hold MinIO's admin key (minio).
	AdminAccessKeyEnv string `json:"adminAccessKeyEnv,omitempty"`
	AdminSecretKeyEnv string `json:"adminSecretKeyEnv,omitempty"`
}

var (
	bucketPrefixPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	iamPathPattern      = regexp.MustCompile(`^/([\x21-\x7e]*/)?$`)
	envNamePattern      = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	regionPattern       = regexp.MustCompile(`^[a-z0-9-]+$`)
)

// Validate checks c as PLATFORM_ENVS_FILE gives it.
func (c Config) Validate() error {
	switch c.Driver {
	case DriverAWS, DriverMinIO:
	case DriverGCS:
		return errors.New("storage driver gcs is not implemented yet (ADR 0016); use aws or minio, or leave storage out")
	default:
		return fmt.Errorf("storage driver %q must be aws or minio", c.Driver)
	}
	switch p := c.BucketPrefix; {
	case p == "":
		return errors.New("storage bucketPrefix is required")
	case len(p) > MaxBucketPrefix:
		return fmt.Errorf("storage bucketPrefix %q is longer than %d characters", p, MaxBucketPrefix)
	case !bucketPrefixPattern.MatchString(p):
		return fmt.Errorf("storage bucketPrefix %q must be lowercase letters, digits and hyphens, starting with a letter or digit", p)
	case strings.HasPrefix(p, "xn--") || strings.HasPrefix(p, "sthree-") || strings.HasPrefix(p, "amzn-s3-demo-"):
		return fmt.Errorf("storage bucketPrefix %q starts with a prefix S3 reserves", p)
	}
	if c.Region != "" && !regionPattern.MatchString(c.Region) {
		return fmt.Errorf("storage region %q is not a region name", c.Region)
	}
	only := func(driver string, fields map[string]string) error {
		for name, v := range fields {
			if v != "" {
				return fmt.Errorf("storage %s is only for the %s driver", name, driver)
			}
		}
		return nil
	}
	switch c.Driver {
	case DriverAWS:
		switch {
		case c.Region == "":
			return errors.New("storage region is required for the aws driver")
		case !iamPathPattern.MatchString(c.IAMPath) || len(c.IAMPath) > 512:
			return fmt.Errorf("storage iamPath %q must start and end with / (e.g. /hopcharlie-apps/)", c.IAMPath)
		case !strings.HasPrefix(c.PermissionsBoundary, "arn:") || !strings.Contains(c.PermissionsBoundary, ":policy/"):
			return fmt.Errorf("storage permissionsBoundary %q must be the ARN of an IAM policy", c.PermissionsBoundary)
		}
		return only(DriverMinIO, map[string]string{"endpoint": c.Endpoint, "adminAccessKeyEnv": c.AdminAccessKeyEnv, "adminSecretKeyEnv": c.AdminSecretKeyEnv})
	default: // minio
		if _, _, err := c.minioEndpoint(); err != nil {
			return err
		}
		for name, v := range map[string]string{"adminAccessKeyEnv": c.AdminAccessKeyEnv, "adminSecretKeyEnv": c.AdminSecretKeyEnv} {
			if !envNamePattern.MatchString(v) {
				return fmt.Errorf("storage %s %q must name an environment variable", name, v)
			}
		}
		if err := only(DriverAWS, map[string]string{"iamPath": c.IAMPath, "permissionsBoundary": c.PermissionsBoundary}); err != nil {
			return err
		}
		if !MinIOBuilt {
			return errMinIONotBuilt
		}
		return nil
	}
}

// errMinIONotBuilt is the error for a minio storage block in a binary built
// without the minio driver.
var errMinIONotBuilt = errors.New("storage driver minio is not in this build: build the platform API with -tags minio (the MinIO admin client is AGPL-3.0, so the default build and the published image leave it out)")

// minioEndpoint returns the host:port of Endpoint and whether it is https.
// Users names where the driver makes its users: the AWS account of the
// server's credentials for every aws environment, the MinIO server for
// minio. A user's name depends only on the instance, so two environments
// with the same Users would share their instances' users. Call it on a
// valid Config.
func (c Config) Users() string {
	if c.Driver == DriverMinIO {
		host, _, _ := c.minioEndpoint()
		return "the MinIO server " + strings.ToLower(host)
	}
	return "the AWS account"
}

func (c Config) minioEndpoint() (host string, secure bool, err error) {
	u, err := url.Parse(c.Endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.User != nil {
		return "", false, fmt.Errorf("storage endpoint %q must be http(s)://host[:port] for the minio driver", c.Endpoint)
	}
	return u.Host, u.Scheme == "https", nil
}

// Instance is what a driver makes storage for: an instance of an app.
type Instance struct {
	App  string // the app's name; it tags the bucket and the user
	Name string // the instance's name: the namespace, and the bucket's suffix
}

// Key is an access key. Secret is only known when the key is made.
type Key struct {
	ID     string
	Secret string
}

// Result is what Ensure made sure of.
type Result struct {
	Bucket string
	// Key is set only when Ensure made a new key; the caller seals it and
	// stores its id. Nil means the existing key stays.
	Key *Key
}

// Driver makes and deletes the buckets and keys of one environment.
// Implementations are safe for concurrent use with different instances;
// calls for one instance must not run concurrently.
type Driver interface {
	// Name is the driver: aws or minio.
	Name() string
	// Bucket is the name of the instance's bucket.
	Bucket(instance string) string
	// Ensure makes the instance's bucket (private, tagged with the app and
	// the instance) and its user with access to that bucket only, keeping
	// what already exists. It makes a new key when newKey is set, when the
	// user is new, or when current, the key in use, is set but the user has
	// no such key (it was deleted outside the platform). Before that it
	// deletes the user's keys other than current and old, a key the pods
	// may still use (all of them when both are empty), so a retry after a
	// crash leaves no key nobody knows of. A user has at most two keys:
	// ErrKeyLimit, with nothing changed, when current and old both exist.
	// Without a new key it leaves the keys as they are. ErrInUse when the
	// bucket or the user belongs to another app.
	Ensure(ctx context.Context, in Instance, newKey bool, current, old string) (Result, error)
	// DeleteKey deletes one key of the instance's user, e.g. the old one
	// after a new key is in use. A key that is gone is not an error.
	DeleteKey(ctx context.Context, in Instance, keyID string) error
	// Delete deletes the instance's user with its keys and policy, then
	// empties the bucket and deletes it. What is already gone is skipped.
	// ErrInUse, with nothing deleted, when the bucket or the user belongs to
	// another app.
	Delete(ctx context.Context, in Instance) error
}

// ErrInUse means a bucket or user with the instance's name belongs to
// another app.
var ErrInUse = errors.New("storage: belongs to another app")

// ErrKeyLimit means a new key is needed while the user has two keys that
// may be in use, the current one and an old one. AWS allows two access
// keys per user, and both drivers keep to that.
var ErrKeyLimit = errors.New("storage: the user already has two keys that may be in use")

// keysToDelete returns the ids among ids to delete before a new key is
// made: all but current and old. ErrKeyLimit when current and old both
// exist.
func keysToDelete(user string, ids []string, current, old string) ([]string, error) {
	var del []string
	kept := 0
	for _, id := range ids {
		if id != "" && (id == current || id == old) {
			kept++
			continue
		}
		del = append(del, id)
	}
	if kept >= 2 {
		return nil, fmt.Errorf("%w: user %s has the key in use %s and the old key %s", ErrKeyLimit, user, MaskKey(current), MaskKey(old))
	}
	return del, nil
}

// MaskKey shows only the last four characters of a key id, for messages
// users see.
func MaskKey(id string) string {
	if len(id) <= 4 {
		return id
	}
	return "..." + id[len(id)-4:]
}

// New returns the driver c configures. It reads the minio admin key from
// the variables c names with getenv. It does not contact the storage.
func New(ctx context.Context, c Config, getenv func(string) string) (Driver, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	switch c.Driver {
	case DriverAWS:
		return newAWS(ctx, c)
	default:
		return newMinIO(c, getenv)
	}
}

// names are the names of an instance's bucket and user.
type names struct{ bucket, user string }

// maxBucket is S3's (and MinIO's) longest bucket name, maxUser IAM's
// longest user name.
const (
	maxBucket = 63
	maxUser   = 64
)

func instanceNames(prefix string, in Instance) (names, error) {
	if in.App == "" || in.Name == "" {
		return names{}, errors.New("storage: app and instance are required")
	}
	n := names{bucket: prefix + in.Name, user: UserPrefix + in.Name}
	switch b := n.bucket; {
	case len(b) > maxBucket:
		return names{}, fmt.Errorf("storage: bucket name %s is longer than %d characters", b, maxBucket)
	case !bucketPrefixPattern.MatchString(b) || strings.HasSuffix(b, "-") || len(b) < 3:
		return names{}, fmt.Errorf("storage: %s is not a valid bucket name", b)
	case strings.HasSuffix(b, "-s3alias") || strings.HasSuffix(b, "--ol-s3") || strings.HasSuffix(b, "--x-s3") || strings.HasSuffix(b, "--table-s3"):
		return names{}, fmt.Errorf("storage: bucket name %s ends with a suffix S3 reserves", b)
	case len(n.user) > maxUser:
		return names{}, fmt.Errorf("storage: user name %s is longer than %d characters", n.user, maxUser)
	}
	return n, nil
}

// bucketPolicy is the policy of an instance's user: list the bucket, and
// read, write and delete its objects, nothing else. arn is the bucket's
// ARN, e.g. arn:aws:s3:::hopcharlie-eks-moimroom.
func bucketPolicy(arn string) string {
	type statement struct {
		Sid      string   `json:"Sid"`
		Effect   string   `json:"Effect"`
		Action   []string `json:"Action"`
		Resource string   `json:"Resource"`
	}
	doc := struct {
		Version   string      `json:"Version"`
		Statement []statement `json:"Statement"`
	}{
		Version: "2012-10-17",
		Statement: []statement{
			{Sid: "Bucket", Effect: "Allow", Action: []string{"s3:ListBucket", "s3:GetBucketLocation", "s3:ListBucketMultipartUploads"}, Resource: arn},
			{Sid: "Objects", Effect: "Allow", Action: []string{"s3:GetObject", "s3:PutObject", "s3:DeleteObject", "s3:AbortMultipartUpload", "s3:ListMultipartUploadParts"}, Resource: arn + "/*"},
		},
	}
	b, _ := json.Marshal(doc)
	return string(b)
}

// checkApp returns ErrInUse when tags name an app other than app, or have
// no app tag unless untagged is allowed. Only a bucket the driver is about
// to make or has just made may be untagged: a crash can leave one between
// its creation and its tagging. An IAM user gets its tags in the same call
// that creates it, so an untagged one is not the platform's.
func checkApp(what string, tags map[string]string, app string, untagged bool) error {
	owner, ok := tags[TagApp]
	switch {
	case !ok && untagged:
		return nil
	case !ok:
		return fmt.Errorf("%w: %s has no %s tag, so the platform did not make it", ErrInUse, what, TagApp)
	case owner != app:
		return fmt.Errorf("%w: %s is app %s's", ErrInUse, what, owner)
	}
	return nil
}

// errNoBucket means the bucket does not exist.
var errNoBucket = errors.New("no such bucket")

// noBucket is errNoBucket naming the bucket.
func noBucket(bucket string) error { return fmt.Errorf("storage: bucket %s: %w", bucket, errNoBucket) }
