package residency

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/preflight"
)

// bucketConfig registers a Seoul (aws) and a Tokyo bucket.
func bucketConfig(sourceLabel string) Config {
	c := fixtureConfig()
	for i := range c.Catalog[0].Datasets {
		c.Catalog[0].Datasets[i].Label = "global" // only the bucket decides below
	}
	c.Buckets = []Bucket{
		{Bucket: "s3:shop-photos", Environment: "aws", Label: sourceLabel},
		{Bucket: "gcs:shop-photos-tokyo", Environment: "tokyo", Label: "global"},
	}
	return c
}

func withFiles(source, target string) preflight.Request {
	r := fixtureRequest()
	r.Movements[0].Destination = "tokyo"
	r.Movements[0].Files = &preflight.BucketCopy{Source: source, Target: target}
	return r
}

func hasReason(v preflight.ResidencyVerdict, code, bucket string) bool {
	return slices.ContainsFunc(v.Reasons, func(r preflight.Reason) bool { return r.Code == code && r.Bucket == bucket && r.Dataset == nil })
}

func TestBucketCopyFollowsSourceLabelAndTargetCountry(t *testing.T) {
	for _, tc := range []struct {
		label   string
		allowed bool
	}{{"global", true}, {"apac", true}, {"kr-only", false}} {
		t.Run(tc.label, func(t *testing.T) {
			v, err := mustEvaluator(t, bucketConfig(tc.label)).Evaluate(context.Background(), withFiles("s3:shop-photos", "gcs:shop-photos-tokyo"))
			if err != nil || v.Allowed != tc.allowed {
				t.Fatalf("verdict %+v err %v", v, err)
			}
			if !tc.allowed && !hasReason(v, "bucket_country_not_allowed", "s3:shop-photos") {
				t.Fatalf("reasons %+v", v.Reasons)
			}
		})
	}
}

func TestBucketCopyNeedsRegisteredBuckets(t *testing.T) {
	e := mustEvaluator(t, bucketConfig("global"))
	v, err := e.Evaluate(context.Background(), withFiles("s3:unknown", "gcs:shop-photos-tokyo"))
	if err != nil || v.Allowed || !hasReason(v, "unregistered_bucket", "s3:unknown") {
		t.Fatalf("unregistered source: %+v %v", v, err)
	}
	v, err = e.Evaluate(context.Background(), withFiles("s3:shop-photos", "gcs:elsewhere"))
	if err != nil || v.Allowed || !hasReason(v, "unregistered_bucket", "gcs:elsewhere") {
		t.Fatalf("unregistered target: %+v %v", v, err)
	}
	// A policy without buckets denies every files copy.
	v, err = mustEvaluator(t, fixtureConfig()).Evaluate(context.Background(), withFiles("s3:shop-photos", "gcs:shop-photos-tokyo"))
	if err != nil || v.Allowed || !hasReason(v, "unregistered_bucket", "s3:shop-photos") {
		t.Fatalf("no buckets configured: %+v %v", v, err)
	}
}

// The database still decides: an allowed bucket does not open a kr-only table.
func TestBucketDoesNotOverrideDatabase(t *testing.T) {
	c := bucketConfig("global")
	c.Catalog[0].Datasets[0].Label = "kr-only"
	v, err := mustEvaluator(t, c).Evaluate(context.Background(), withFiles("s3:shop-photos", "gcs:shop-photos-tokyo"))
	if err != nil || v.Allowed || !slices.ContainsFunc(v.Reasons, func(r preflight.Reason) bool { return r.Code == "country_not_allowed" }) {
		t.Fatalf("verdict %+v err %v", v, err)
	}
}

func TestBucketInputAndConfigValidation(t *testing.T) {
	e := mustEvaluator(t, bucketConfig("global"))
	for name, r := range map[string]preflight.Request{
		"same bucket":   withFiles("s3:shop-photos", "s3:shop-photos"),
		"inline remote": withFiles(":s3,access_key_id=TOPSECRET:b", "gcs:shop-photos-tokyo"),
		"empty target":  withFiles("s3:shop-photos", ""),
	} {
		t.Run(name, func(t *testing.T) {
			if v, err := e.Evaluate(context.Background(), r); !errors.Is(err, ErrInvalidInput) || v.Allowed {
				t.Fatalf("verdict %+v err %v", v, err)
			}
		})
	}
	for name, change := range map[string]func(*Config){
		"bad name":         func(c *Config) { c.Buckets[0].Bucket = ":local:/etc" },
		"duplicate":        func(c *Config) { c.Buckets[1].Bucket = c.Buckets[0].Bucket },
		"unregistered env": func(c *Config) { c.Buckets[0].Environment = "mars" },
		"bad label":        func(c *Config) { c.Buckets[0].Label = "secret" },
	} {
		t.Run(name, func(t *testing.T) {
			c := bucketConfig("global")
			change(&c)
			if _, err := c.normalized(); !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("accepted: %v", err)
			}
		})
	}
}

func TestBucketsChangeDigestOnlyWhenPresent(t *testing.T) {
	plain := mustEvaluator(t, fixtureConfig()).PolicyDigest()
	with := mustEvaluator(t, bucketConfig("global"))
	c := bucketConfig("global")
	slices.Reverse(c.Buckets)
	if with.PolicyDigest() == plain || mustEvaluator(t, c).PolicyDigest() != with.PolicyDigest() {
		t.Fatal("bucket entries must change the digest, their order must not")
	}
}
