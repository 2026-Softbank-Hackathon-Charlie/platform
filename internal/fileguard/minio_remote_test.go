package fileguard

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestMinIOFixtureEndpointRejectsNonLocalTargets(t *testing.T) {
	for _, input := range []string{"https://example.com:9000", "http://169.254.169.254:9000", "http://localhost:9000", "http://127.0.0.1", "http://127.0.0.1:0", "http://127.0.0.1:9000/path", "http://user:secret@127.0.0.1:9000", "http://127.0.0.1:9000?x=1"} {
		if _, err := minioFixtureEndpoint(input); err == nil {
			t.Fatal("non-fixture endpoint accepted")
		}
	}
	for _, input := range []string{"http://127.0.0.1:19000", "http://[::1]:19001"} {
		if _, err := minioFixtureEndpoint(input); err != nil {
			t.Fatal("literal loopback fixture rejected")
		}
	}
}

func TestMinIOFixtureCannotInheritCloudOrRcloneOverrides(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "fixture-should-not-propagate")
	t.Setenv("RCLONE_S3_ENDPOINT", "https://example.invalid")
	t.Setenv("RCLONE_CONFIG_HOPMINIOA_SESSION_TOKEN", "fixture-should-not-propagate")
	a, _ := url.Parse("http://127.0.0.1:19000")
	b, _ := url.Parse("http://127.0.0.1:19001")
	for _, entry := range minioFixtureEnvironment(a, b) {
		if strings.HasPrefix(entry, "AWS_") || strings.HasPrefix(entry, "RCLONE_S3_") || strings.Contains(entry, "SESSION_TOKEN") || strings.Contains(entry, "example.invalid") {
			t.Fatal("fixture inherited an external credential or routing override")
		}
	}
}

func TestMinIOFixtureSanitizedRcloneCanRun(t *testing.T) {
	binary, err := exec.LookPath("rclone")
	if err != nil {
		t.Skip("actual rclone is not installed")
	}
	a, _ := url.Parse("http://127.0.0.1:19000")
	b, _ := url.Parse("http://127.0.0.1:19001")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "version", "--config", os.DevNull)
	cmd.Env = minioFixtureEnvironment(a, b)
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if cmd.Run() != nil {
		t.Fatal("actual rclone could not start with isolated fixture environment")
	}
}

// Opt in only on two disposable local MinIO servers. This never accepts a
// production endpoint or credential: the synthetic root pair is fixed here.
func TestDisposableMinIOFileguard(t *testing.T) {
	if os.Getenv("FILEGUARD_MINIO_FIXTURE") != "disposable-loopback-v1" {
		t.Skip("requires explicit disposable loopback MinIO fixture")
	}
	binary, err := exec.LookPath("rclone")
	if err != nil {
		t.Fatal("actual rclone is required for the opted-in fixture")
	}
	a, err := minioFixtureEndpoint(os.Getenv("FILEGUARD_MINIO_A_ENDPOINT"))
	if err != nil {
		t.Fatal("invalid local MinIO A endpoint")
	}
	b, err := minioFixtureEndpoint(os.Getenv("FILEGUARD_MINIO_B_ENDPOINT"))
	if err != nil || a.Port() == b.Port() {
		t.Fatal("two distinct local MinIO ports are required")
	}
	fixtureEnv := minioFixtureEnvironment(a, b)
	execute := func(ctx context.Context, out io.Writer, args ...string) error {
		args = append(args, "--config", os.DevNull, "--log-level", "ERROR", "--log-file", "")
		cmd := exec.CommandContext(ctx, binary, args...)
		cmd.Env = fixtureEnv
		cmd.Stdout, cmd.Stderr, cmd.WaitDelay = out, io.Discard, 5*time.Second
		if cmd.Run() != nil {
			return ErrTransfer
		}
		return nil
	}
	fixtureCommand := func(ctx context.Context, args ...string) ([]byte, error) {
		return minioFixtureCommand(ctx, execute, args...)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	bucket, err := minioFixtureBucket()
	if err != nil {
		t.Fatal("could not name disposable bucket")
	}
	source, target := "hopminioa:"+bucket, "hopminiob:"+bucket
	cleaned := false
	cleanup := func() error {
		cleanupCtx, done := context.WithTimeout(context.Background(), 20*time.Second)
		defer done()
		for _, remote := range []string{source, target} {
			if _, err := fixtureCommand(cleanupCtx, "purge", remote); err != nil {
				return err
			}
			root, _, _ := strings.Cut(remote, ":")
			listed, err := fixtureCommand(cleanupCtx, "lsf", root+":", "--dirs-only")
			if err != nil || slices.Contains(strings.Fields(string(listed)), bucket+"/") {
				return errors.New("fixture cleanup not verified")
			}
		}
		cleaned = true
		return nil
	}
	// The workflow independently removes its exact containers even after a
	// test failure. No arbitrary bucket or user config is ever purged here.
	t.Cleanup(func() {
		if !cleaned {
			if err := cleanup(); err != nil {
				t.Error("fixture bucket cleanup failed")
			}
		}
	})
	for _, remote := range []string{source, target} {
		if _, err := fixtureCommand(ctx, "mkdir", remote); err != nil {
			t.Fatal("could not create disposable bucket")
		}
	}
	put := func(remote, key, body string) {
		t.Helper()
		path := filepath.Join(t.TempDir(), "object.txt")
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal("could not seed fixture object")
		}
		if _, err := fixtureCommand(ctx, "copyto", path, remote+"/"+key, "--ignore-times"); err != nil {
			t.Fatal("fixture object upload failed")
		}
	}
	read := func(remote, key, want string) {
		t.Helper()
		body, err := fixtureCommand(ctx, "cat", remote+"/"+key)
		if err != nil || string(body) != want {
			t.Fatal("actual remote object bytes differ")
		}
	}
	list := func(remote string) []string {
		t.Helper()
		out, err := fixtureCommand(ctx, "lsf", remote, "--recursive", "--files-only")
		if err != nil {
			t.Fatal("actual remote listing failed")
		}
		return strings.Fields(string(out))
	}
	passed := func(name string) { fmt.Println("FILEGUARD_MINIO_SCENARIO " + name + " passed") }
	put(source, "body.txt", "hello")
	if len(list(target)) != 0 {
		t.Fatal("fixture endpoints share storage and are not independent")
	}
	cache := NewClassificationCache(16)
	metadataChecks, authorizations := 0, 0
	metadataDenied, authorizationDenied := false, false
	before := func(context.Context) error {
		metadataChecks++
		if metadataDenied {
			return ErrDenied
		}
		return nil
	}
	authorize := func(context.Context, Report) error {
		authorizations++
		if authorizationDenied {
			return ErrDenied
		}
		return nil
	}
	forward := New(source, target, nil, "", before, authorize).WithClassificationCache(cache, strings.Repeat("a", 64))
	forward.execute = execute // Actual rclone, with only the validated fixture environment.
	first, err := forward.Sync(ctx, source, target)
	if err != nil {
		t.Fatal("actual forward sync failed")
	}
	if hits, misses := cacheCounts(t, first.Output); hits != 0 || misses != 1 {
		t.Fatal("initial remote bytes were not inspected")
	}
	read(target, "body.txt", "hello")
	passed("forward_copy")
	back := New(target, source, nil, "", before, authorize).WithClassificationCache(cache, strings.Repeat("a", 64))
	back.execute = execute
	second, err := back.Sync(ctx, target, source)
	if err != nil {
		t.Fatal("actual reverse sync failed")
	}
	if hits, misses := cacheCounts(t, second.Output); hits != 1 || misses != 0 {
		t.Fatal("exact return bytes did not reuse classification")
	}
	if metadataChecks != 2 || authorizations != 2 {
		t.Fatal("current metadata and authorization hooks were not re-evaluated")
	}
	passed("reverse_copy_cache_reuse")
	put(target, "body.txt", "other") // Same length, different source bytes.
	third, err := back.Sync(ctx, target, source)
	if err != nil {
		t.Fatal("same-size source change failed")
	}
	if hits, misses := cacheCounts(t, third.Output); hits != 0 || misses != 1 {
		t.Fatal("same-size source change reused old classification")
	}
	read(source, "body.txt", "other")
	passed("same_size_source_change")
	put(source, "body.txt", "wrong") // Same-size target corruption.
	if _, err := back.Check(ctx, target, source); !errors.Is(err, ErrTransfer) {
		t.Fatal("same-size target corruption passed full-byte proof")
	}
	read(source, "body.txt", "wrong")
	passed("target_corruption_detected")
	if _, err := back.Sync(ctx, target, source); err != nil {
		t.Fatal("actual target repair failed")
	}
	read(source, "body.txt", "other")
	if _, err := back.Check(ctx, target, source); err != nil {
		t.Fatal("repaired target failed full-byte proof")
	}
	passed("target_corruption_repaired")
	if _, err := fixtureCommand(ctx, "deletefile", target+"/body.txt"); err != nil {
		t.Fatal("fixture deletion failed")
	}
	put(target, "nested/[literal].txt", "added")
	put(target, "empty.txt", "")
	put(source, "unexpected.txt", "stale")
	if _, err := back.Sync(ctx, target, source); err != nil {
		t.Fatal("actual object add/delete sync failed")
	}
	keys := list(source)
	slices.Sort(keys)
	if !slices.Equal(keys, []string{"empty.txt", "nested/[literal].txt"}) {
		t.Fatal("current remote inventory was not mirrored")
	}
	read(source, "nested/[literal].txt", "added")
	read(source, "empty.txt", "")
	passed("object_add_delete")
	uploads := 0
	actual := back.execute
	back.execute = func(ctx context.Context, out io.Writer, args ...string) error {
		// Snapshot download is also "copy". Count only mutations whose
		// destination is the registered remote target, never local inspection.
		if len(args) >= 3 && (args[0] == "sync" || args[0] == "copy") && args[2] == source {
			uploads++
		}
		return actual(ctx, out, args...)
	}
	previousMetadata, previousAuthorization := metadataChecks, authorizations
	authorizationDenied = true
	denied, err := back.Sync(ctx, target, source)
	if !errors.Is(err, ErrDenied) || uploads != 0 || metadataChecks != previousMetadata+1 || authorizations != previousAuthorization+1 {
		t.Fatal("warm classification bypassed current authorization")
	}
	if hits, _ := cacheCounts(t, denied.Output); hits < 1 {
		t.Fatal("authorization denial did not exercise a warm cache")
	}
	read(source, "nested/[literal].txt", "added")
	passed("authorization_rechecked")
	authorizationDenied = false
	metadataDenied = true
	previousMetadata, previousAuthorization = metadataChecks, authorizations
	if _, err := back.Sync(ctx, target, source); !errors.Is(err, ErrDenied) || uploads != 0 || metadataChecks != previousMetadata+1 || authorizations != previousAuthorization {
		t.Fatal("current metadata gate did not block before mutation")
	}
	read(source, "nested/[literal].txt", "added")
	passed("metadata_rechecked")
	metadataDenied = false
	cancelled, stop := context.WithCancel(ctx)
	stop()
	if _, err := back.Sync(cancelled, target, source); err == nil || uploads != 0 {
		t.Fatal("cancelled transfer dispatched an upload")
	}
	read(source, "nested/[literal].txt", "added")
	passed("cancelled_no_upload")
	if err := cleanup(); err != nil {
		t.Fatal("fixture bucket cleanup failed")
	}
	passed("fixture_bucket_cleanup")
	fmt.Println("FILEGUARD_MINIO_RESULT passed")
}

func minioFixtureEndpoint(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.ForceQuery || strings.Contains(raw, "#") {
		return nil, errors.New("not a disposable loopback endpoint")
	}
	address, err := netip.ParseAddr(u.Hostname())
	if err != nil || !address.IsLoopback() || address.Zone() != "" {
		return nil, errors.New("not a disposable loopback endpoint")
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 1 || port > 65535 {
		return nil, errors.New("not a disposable loopback endpoint")
	}
	return u, nil
}

func minioFixtureBucket() (string, error) {
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return "hopcharlie-e2e-" + hex.EncodeToString(random[:]), nil
}

func minioFixtureCommand(ctx context.Context, execute command, args ...string) ([]byte, error) {
	var result bytes.Buffer
	// No raw output, command arguments, endpoints or credential values enter
	// test logs. Controlled fixture inventories fit in this finite buffer.
	out := &boundedWriter{w: &result, remaining: 64 << 10}
	if err := execute(ctx, out, args...); err != nil {
		return nil, ErrTransfer
	}
	return result.Bytes(), nil
}

func minioFixtureEnvironment(a, b *url.URL) []string {
	var env []string
	// Do not propagate AWS credentials, global rclone flags, proxy settings,
	// home/config locations or inherited per-remote session tokens. A global
	// RCLONE_S3_ENDPOINT must not override the validated loopback endpoints.
	for _, key := range []string{"PATH", "SystemRoot", "WINDIR", "TEMP", "TMP", "TMPDIR"} {
		if value, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+value)
		}
	}
	for name, endpoint := range map[string]*url.URL{"HOPMINIOA": a, "HOPMINIOB": b} {
		for key, value := range map[string]string{"TYPE": "s3", "PROVIDER": "Minio", "ENV_AUTH": "false", "ACCESS_KEY_ID": "fileguard-fixture", "SECRET_ACCESS_KEY": "fileguard-fixture-password-v1", "ENDPOINT": endpoint.String(), "REGION": "us-east-1", "FORCE_PATH_STYLE": "true"} {
			env = append(env, "RCLONE_CONFIG_"+name+"_"+key+"="+value)
		}
	}
	return env
}
