package apps

import (
	"fmt"
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
	// BucketPrefixes maps each environment with an aws or gcs storage driver
	// to its bucket prefix, for Registry.BucketPrefixes.
	BucketPrefixes map[string]string
	// Set are the remotes whose variables were put into the environment;
	// Kept are the ones that were already there and were left alone.
	Set, Kept []string
}

// EnableFiles reads PLATFORM_APP_FILES (v): "" or "0" is off and returns
// nil. "1" puts an rclone remote named after every environment with an aws
// or gcs storage driver into the server's environment through setenv
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
	for _, e := range environments {
		vars := RemoteEnv(e)
		if vars == nil {
			continue
		}
		out.BucketPrefixes[e.Name] = e.Storage.BucketPrefix
		if _, ok := lookup(plans.RcloneTypeEnv(e.Name + ":")); ok {
			out.Kept = append(out.Kept, e.Name)
			continue
		}
		keys := make([]string, 0, len(vars))
		for k := range vars {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			if err := setenv(k, vars[k]); err != nil {
				return nil, fmt.Errorf("set %s: %w", k, err)
			}
		}
		out.Set = append(out.Set, e.Name)
	}
	if len(out.BucketPrefixes) == 0 {
		return nil, fmt.Errorf("no environment has an aws or gcs storage driver")
	}
	return out, nil
}

// RemoteEnv is the rclone remote named after environment e, as the
// variables rclone reads it from (RCLONE_CONFIG_<ENV>_*): S3 or Cloud
// Storage with the credentials of the server's own environment (env_auth:
// the pod's AWS Pod Identity, or GOOGLE_APPLICATION_CREDENTIALS), never a
// key. Buckets are neither checked nor made, which needs no bucket
// permission beyond listing and objects. nil for an environment without
// an aws or gcs storage driver, or whose name is not a remote name.
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
