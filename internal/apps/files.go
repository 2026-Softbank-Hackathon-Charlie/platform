package apps

import (
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/envs"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/plans"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/storage"
)

// Files is what PLATFORM_APP_FILES=1 turns on (ADR 0032): the registered
// apps' files move between their buckets through rclone remotes named after
// the environments, which use the platform's own cloud identities.
type Files struct {
	// BucketPrefixes maps each environment with an aws, gcs or minio storage driver
	// to its bucket prefix, for Registry.BucketPrefixes.
	BucketPrefixes map[string]string
	// Set are the remotes whose variables were put into the environment;
	// Kept are the ones that were already there and were left alone.
	Set, Kept []string
}

// EnableFiles reads PLATFORM_APP_FILES (v): "" or "0" is off and returns
// nil. "1" puts an rclone remote named after every environment with an aws,
// gcs or minio storage driver into the server's environment through setenv
// (RemoteEnv), unless lookup finds the remote's _TYPE variable already set,
// and returns those environments' bucket prefixes. The engine's rclone and
// the residency checks inherit the server's environment.
func EnableFiles(v string, environments []envs.EnvConfig, lookup func(string) (string, bool), setenv func(string, string) error) (*Files, error) {
	switch v {
	case "", "0":
		return nil, nil
	case "1":
	default:
		return nil, fmt.Errorf("must be 1 or 0, not %q", v)
	}
	out := &Files{BucketPrefixes: map[string]string{}}
	remotes := make([]map[string]string, 0, len(environments))
	for _, e := range environments {
		vars := RemoteEnv(e)
		if vars == nil {
			if e.Storage != nil && e.Storage.Driver == storage.DriverMinIO {
				return nil, fmt.Errorf("minio files remote name is invalid")
			}
			continue
		}
		if e.Storage.Driver == storage.DriverMinIO && (len(e.Storage.BucketPrefix) > storage.MaxBucketPrefix || !filesBucketPrefix.MatchString(e.Storage.BucketPrefix)) {
			return nil, fmt.Errorf("files remote %s: minio bucketPrefix is invalid", e.Name)
		}
		out.BucketPrefixes[e.Name] = e.Storage.BucketPrefix
		if _, ok := lookup(plans.RcloneTypeEnv(e.Name + ":")); ok {
			out.Kept = append(out.Kept, e.Name)
			continue
		}
		if e.Storage.Driver == storage.DriverMinIO {
			if err := minioRemoteCredentials(e, vars, lookup); err != nil {
				return nil, fmt.Errorf("files remote %s: %w", e.Name, err)
			}
		}
		remotes = append(remotes, vars)
		out.Set = append(out.Set, e.Name)
	}
	if len(out.BucketPrefixes) == 0 {
		return nil, fmt.Errorf("no environment has an aws, gcs or minio storage driver")
	}
	// Validate and resolve every remote before changing the process environment.
	for _, vars := range remotes {
		keys := make([]string, 0, len(vars))
		for k := range vars {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			if err := setenv(k, vars[k]); err != nil {
				// A setter may include its value in the error; never propagate it.
				return nil, fmt.Errorf("cannot set files remote variable %s", k)
			}
		}
	}
	return out, nil
}

// RemoteEnv is the rclone remote named after environment e, as the
// variables rclone reads it from (RCLONE_CONFIG_<ENV>_*): S3 or Cloud
// Storage with the credentials of the server's own environment (env_auth:
// the pod's AWS Pod Identity, or GOOGLE_APPLICATION_CREDENTIALS), never a
// key. MinIO returns connection settings only; EnableFiles resolves its
// configured credential references into per-remote variables in memory.
// Buckets are neither checked nor made, which needs no bucket
// permission beyond listing and objects. nil for an environment without
// an aws, gcs or minio storage driver, or whose name is not a remote name.
func RemoteEnv(e envs.EnvConfig) map[string]string {
	if e.Storage == nil || plans.CheckBucket(e.Name+":b") != nil {
		return nil
	}
	p := strings.TrimSuffix(plans.RcloneTypeEnv(e.Name+":"), "TYPE")
	switch e.Storage.Driver {
	case storage.DriverAWS:
		return map[string]string{
			p + "TYPE":            "s3",
			p + "PROVIDER":        "AWS",
			p + "ENV_AUTH":        "true",
			p + "REGION":          e.Storage.Region,
			p + "NO_CHECK_BUCKET": "true",
		}
	case storage.DriverGCS:
		return map[string]string{
			p + "TYPE":               "google cloud storage",
			p + "ENV_AUTH":           "true",
			p + "BUCKET_POLICY_ONLY": "true",
			p + "NO_CHECK_BUCKET":    "true",
		}
	case storage.DriverMinIO:
		return map[string]string{
			p + "TYPE":             "s3",
			p + "PROVIDER":         "Minio",
			p + "ENV_AUTH":         "false",
			p + "REGION":           e.Storage.Region,
			p + "ENDPOINT":         e.Storage.Endpoint,
			p + "FORCE_PATH_STYLE": "true",
			p + "NO_CHECK_BUCKET":  "true",
		}
	}
	return nil
}

var filesEnvName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
var filesBucketPrefix = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// The endpoint and credential references come only from operator-owned
// PLATFORM_ENVS_FILE, never from a migration request. Private addresses and
// HTTP are intentional for on-prem networks; network egress policy remains
// the operator's responsibility. Do not include parsed URLs or key values in errors.
func minioRemoteCredentials(e envs.EnvConfig, vars map[string]string, lookup func(string) (string, bool)) error {
	c := e.Storage
	u, err := url.Parse(c.Endpoint)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
		return fmt.Errorf("minio endpoint must be an http(s) origin without credentials, query or fragment")
	}
	p := strings.TrimSuffix(plans.RcloneTypeEnv(e.Name+":"), "TYPE")
	for _, ref := range []struct{ field, name, suffix string }{
		{"adminAccessKeyEnv", c.AdminAccessKeyEnv, "ACCESS_KEY_ID"},
		{"adminSecretKeyEnv", c.AdminSecretKeyEnv, "SECRET_ACCESS_KEY"},
	} {
		if !filesEnvName.MatchString(ref.name) {
			return fmt.Errorf("minio %s must name an environment variable", ref.field)
		}
		value, ok := lookup(ref.name)
		if !ok || strings.TrimSpace(value) == "" {
			return fmt.Errorf("minio %s credential is missing or blank", ref.field)
		}
		vars[p+ref.suffix] = value
	}
	return nil
}

// fileBucket is the bucket of instance in env as a plan names it,
// <env>:<bucketPrefix><instance>, or "" when the registry moves no files
// there.
func (r *Registry) fileBucket(env, instance string) string {
	prefix, ok := r.BucketPrefixes[env]
	if !ok {
		return ""
	}
	b := env + ":" + prefix + instance
	if plans.CheckBucket(b) != nil {
		return ""
	}
	return b
}
