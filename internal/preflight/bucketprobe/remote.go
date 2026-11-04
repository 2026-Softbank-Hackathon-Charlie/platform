// Package bucketprobe measures a bucket through the rclone remote a plan's
// files copy will use: where the store says the bucket is (or, for a store
// the operator runs, which network its endpoint is on), its first-level
// prefixes, the buckets it replicates into and whether its lifecycle tiers
// objects elsewhere. It reads the remote's settings from the same
// environment variables rclone reads (RCLONE_CONFIG_<REMOTE>_*), so the
// measured store is the one the copy reaches.
//
// It reads metadata only, never object contents. Failures are fixed codes:
// endpoints, credentials and store error messages never appear in results.
package bucketprobe

import (
	"net/url"
	"regexp"
	"strings"
)

// Codes explaining an incomplete observation. An empty Code means complete.
const (
	CodeInvalidBucket       = "invalid_bucket"
	CodeRemoteNotConfigured = "remote_not_configured"
	CodeUnsupportedBackend  = "unsupported_backend"
	CodeCredentials         = "credentials_unavailable"
	CodeResolveFailed       = "endpoint_resolve_failed"
	CodeLocationFailed      = "location_failed"
	CodeListFailed          = "list_failed"
	CodeTooManyPrefixes     = "too_many_prefixes"
	CodeUnsupportedPrefix   = "unsupported_prefix"
	CodeReplicationFailed   = "replication_failed"
	CodeLifecycleFailed     = "lifecycle_failed"
	CodeTimeout             = "timeout"
)

// rclone backend types the probe understands.
const (
	typeS3  = "s3"
	typeGCS = "google cloud storage"
)

// remote is one rclone remote as rclone would configure it from the
// environment, plus the bucket and path a plan names on it.
type remote struct {
	name, typ, provider, endpoint, region string
	accessKey, secretKey, sessionToken    string
	envAuth                               bool
	saCredentials, saFile                 string // gcs
	bucket, path                          string
}

var specPattern = regexp.MustCompile(`^([A-Za-z0-9_-]+):([A-Za-z0-9][A-Za-z0-9._-]*)((?:/[A-Za-z0-9._-]+)*)$`)

// readRemote parses "remote:bucket[/path]" and reads the remote's settings.
// A remote whose type is not in the environment is not configured here:
// rclone could still find it in a config file, which the probe cannot see,
// so the probe refuses rather than guess.
func readRemote(getenv func(string) string, spec string) (remote, string) {
	m := specPattern.FindStringSubmatch(spec)
	if m == nil {
		return remote{}, CodeInvalidBucket
	}
	r := remote{name: m[1], bucket: m[2], path: strings.TrimPrefix(m[3], "/")}
	for _, seg := range strings.Split(r.path, "/") {
		if seg == "." || seg == ".." {
			return remote{}, CodeInvalidBucket
		}
	}
	key := "RCLONE_CONFIG_" + strings.ToUpper(strings.ReplaceAll(r.name, "-", "_")) + "_"
	r.typ = getenv(key + "TYPE")
	if r.typ == "" {
		return remote{}, CodeRemoteNotConfigured
	}
	// rclone falls back to the backend-wide RCLONE_<BACKEND>_<OPTION> when the
	// remote leaves an option unset.
	backend := "RCLONE_S3_"
	if r.typ == typeGCS {
		backend = "RCLONE_GCS_"
	}
	opt := func(name string) string {
		if v := getenv(key + name); v != "" {
			return v
		}
		return getenv(backend + name)
	}
	r.provider, r.endpoint, r.region = opt("PROVIDER"), opt("ENDPOINT"), opt("REGION")
	r.accessKey, r.secretKey, r.sessionToken = opt("ACCESS_KEY_ID"), opt("SECRET_ACCESS_KEY"), opt("SESSION_TOKEN")
	r.envAuth = strings.EqualFold(opt("ENV_AUTH"), "true")
	r.saCredentials, r.saFile = opt("SERVICE_ACCOUNT_CREDENTIALS"), opt("SERVICE_ACCOUNT_FILE")
	return r, ""
}

var (
	awsEndpoint = regexp.MustCompile(`^s3([.-][a-z0-9-]+)*\.amazonaws\.com(\.cn)?$`)
	gcsHost     = "storage.googleapis.com"
)

// endpointHost is the host of an rclone endpoint setting, which may omit the
// scheme ("minio.lan:9000"). rclone defaults to https.
func endpointHost(endpoint string) (host, base string, ok bool) {
	if endpoint == "" {
		return "", "", false
	}
	if !strings.Contains(endpoint, "://") {
		endpoint = "https://" + endpoint
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", "", false
	}
	return strings.ToLower(u.Hostname()), u.Scheme + "://" + u.Host, true
}

// providerHosted reports whether an s3 remote reaches the provider itself.
// "provider = AWS" with a custom endpoint is any S3-compatible server that
// would report whatever region it is configured with, so it is self-hosted.
func (r remote) providerHosted() bool {
	switch {
	case r.typ == typeGCS:
		return true
	case strings.EqualFold(r.provider, "AWS"):
		if r.endpoint == "" {
			return true
		}
		host, _, ok := endpointHost(r.endpoint)
		return ok && awsEndpoint.MatchString(host)
	case strings.EqualFold(r.provider, "GCS"):
		host, _, ok := endpointHost(r.endpoint)
		return ok && host == gcsHost
	}
	return false
}
