//go:build minio

// Live test against two real MinIO servers (-tags minio links madmin-go,
// AGPL, as internal/storage does). Run with
//
//	BUCKETPROBE_MINIO_A=127.0.0.1:19000 BUCKETPROBE_MINIO_B=127.0.0.1:19001 \
//	BUCKETPROBE_MINIO_B_INTERNAL=bp-minio-b:9000 BUCKETPROBE_MINIO_KEY=... BUCKETPROBE_MINIO_SECRET=... \
//	go test -tags minio -run Live ./internal/preflight/bucketprobe/
//
// A is the on-prem source, B a DR server A replicates and tiers into.
package bucketprobe

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/minio/madmin-go/v3"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/minio/minio-go/v7/pkg/lifecycle"
	"github.com/minio/minio-go/v7/pkg/replication"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/preflight"
)

func TestLiveMinIO(t *testing.T) {
	a, b, bInternal := os.Getenv("BUCKETPROBE_MINIO_A"), os.Getenv("BUCKETPROBE_MINIO_B"), os.Getenv("BUCKETPROBE_MINIO_B_INTERNAL")
	key, secret := os.Getenv("BUCKETPROBE_MINIO_KEY"), os.Getenv("BUCKETPROBE_MINIO_SECRET")
	if a == "" || b == "" || bInternal == "" || key == "" {
		t.Skip("BUCKETPROBE_MINIO_* not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	client := func(ep string) *minio.Client {
		c, err := minio.New(ep, &minio.Options{Creds: credentials.NewStaticV4(key, secret, "")})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	ca, cb := client(a), client(b)
	suffix := time.Now().Format("150405")
	src, dr, tier := "photos-"+suffix, "photos-dr-"+suffix, "tier-"+suffix
	for _, x := range []struct {
		c    *minio.Client
		name string
	}{{ca, src}, {cb, dr}, {cb, tier}} {
		if err := x.c.MakeBucket(ctx, x.name, minio.MakeBucketOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	for _, k := range []string{"cover.jpg", "rooms/1/a.jpg", "rooms/2.jpg", "kyc/900101-1234567.jpg", "kyc/hong@example.com/id.png"} {
		opts := minio.PutObjectOptions{ContentType: "image/jpeg", UserMetadata: map[string]string{"owner-name": "TOPSECRET-VALUE"}}
		if _, err := ca.PutObject(ctx, src, k, bytes.NewReader([]byte("x")), 1, opts); err != nil {
			t.Fatal(err)
		}
	}

	vars := map[string]string{
		"RCLONE_CONFIG_ONPREM_TYPE": "s3", "RCLONE_CONFIG_ONPREM_PROVIDER": "Minio",
		"RCLONE_CONFIG_ONPREM_ENDPOINT": "http://" + a, "RCLONE_CONFIG_ONPREM_ACCESS_KEY_ID": key,
		"RCLONE_CONFIG_ONPREM_SECRET_ACCESS_KEY": secret,
	}
	p := Prober{Getenv: env(vars)}

	// Before: a plain on-prem bucket. A started without MINIO_REGION reports
	// an empty location, which is recorded as unknown.
	obs, res := p.Probe(ctx, "onprem:"+src, Options{Prefixes: true, Replication: true})
	want := preflight.BucketObservation{Hosting: preflight.HostingSelf,
		EndpointIPs: []string{"127.0.0.1"}, Prefixes: []string{"", "kyc/", "rooms/"}, Replicas: []string{}}
	if res.Code != "" || !reflect.DeepEqual(obs, want) {
		t.Fatalf("plain bucket: %q\n got %+v\nwant %+v", res.Code, obs, want)
	}
	t.Logf("plain: %+v", obs)

	// The sample of kyc/ shows the shapes of its keys, never the keys.
	sample, res := p.Sample(ctx, "onprem:"+src, "kyc/", 0)
	raw, _ := json.Marshal(sample)
	if res.Code != "" || sample.Objects != 2 || sample.KeyPatterns["kr_resident_number"] != 1 || sample.KeyPatterns["email"] != 1 ||
		sample.MetadataKeys["owner-name"] != 2 || sample.ContentTypes["image/jpeg"] != 2 || sample.MaxDepth != 2 ||
		strings.Contains(string(raw), "1234567") || strings.Contains(string(raw), "hong") || strings.Contains(string(raw), "TOPSECRET") {
		t.Fatalf("sample: %q %s", res.Code, raw)
	}
	t.Logf("sample: %s", raw)

	// Replication from A to B (needs versioning on both and a remote target).
	adm, err := madmin.New(a, key, secret, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range []struct {
		c    *minio.Client
		name string
	}{{ca, src}, {cb, dr}} {
		if err := x.c.EnableVersioning(ctx, x.name); err != nil {
			t.Fatal(err)
		}
	}
	arn, err := adm.SetRemoteTarget(ctx, src, &madmin.BucketTarget{
		SourceBucket: src, Endpoint: bInternal, TargetBucket: dr, Type: madmin.ReplicationService,
		Credentials: &madmin.Credentials{AccessKey: key, SecretKey: secret}, API: "s3v4",
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg := replication.Config{Rules: []replication.Rule{{
		ID: "dr", Status: replication.Enabled, Priority: 1,
		DeleteMarkerReplication: replication.DeleteMarkerReplication{Status: replication.Disabled},
		DeleteReplication:       replication.DeleteReplication{Status: replication.Disabled},
		Destination:             replication.Destination{Bucket: arn},
	}}}
	if err := ca.SetBucketReplication(ctx, src, cfg); err != nil {
		t.Fatal(err)
	}
	t.Logf("remote target arn %s", arn)

	// ILM tiering from A into a bucket on B.
	if err := adm.AddTier(ctx, &madmin.TierConfig{Version: madmin.TierConfigVer, Type: madmin.MinIO, Name: "WARM" + suffix,
		MinIO: &madmin.TierMinIO{Endpoint: "http://" + bInternal, AccessKey: key, SecretKey: secret, Bucket: tier}}); err != nil {
		t.Fatal(err)
	}
	lc := lifecycle.NewConfiguration()
	lc.Rules = []lifecycle.Rule{{ID: "tier", Status: "Enabled", RuleFilter: lifecycle.Filter{Prefix: ""},
		Transition: lifecycle.Transition{Days: 30, StorageClass: "WARM" + suffix}}}
	if err := ca.SetBucketLifecycle(ctx, src, lc); err != nil {
		t.Fatal(err)
	}

	obs, res = p.Probe(ctx, "onprem:"+src, Options{Prefixes: true, Replication: true})
	if res.Code != "" || !reflect.DeepEqual(obs.Replicas, []string{dr}) || !obs.Tiered {
		t.Fatalf("replicated and tiered: %q %+v", res.Code, obs)
	}
	t.Logf("replicated+tiered: %+v", obs)

	// B was started with MINIO_REGION: its "location" is just that setting.
	vars["RCLONE_CONFIG_DR_TYPE"], vars["RCLONE_CONFIG_DR_PROVIDER"] = "s3", "Minio"
	vars["RCLONE_CONFIG_DR_ENDPOINT"], vars["RCLONE_CONFIG_DR_REGION"] = "http://"+b, "kr-onprem-dr"
	vars["RCLONE_CONFIG_DR_ACCESS_KEY_ID"], vars["RCLONE_CONFIG_DR_SECRET_ACCESS_KEY"] = key, secret
	obs, res = p.Probe(ctx, "dr:"+dr, Options{})
	if res.Code != "" || obs.Location != "kr-onprem-dr" || obs.Hosting != preflight.HostingSelf {
		t.Fatalf("dr: %q %+v", res.Code, obs)
	}
	t.Logf("dr: %+v", obs)
}
