package residency

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/preflight"
)

// bucketConfig registers a Seoul (aws), a Tokyo and an on-prem MinIO bucket,
// each with how its location is confirmed.
func bucketConfig(sourceLabel string) Config {
	c := fixtureConfig()
	for i := range c.Catalog[0].Datasets {
		c.Catalog[0].Datasets[i].Label = "global" // only the bucket decides below
	}
	c.Buckets = []Bucket{
		{Bucket: "s3:shop-photos", Environment: "aws", Label: sourceLabel, Expect: &BucketExpect{Locations: []string{"ap-northeast-2"}}},
		{Bucket: "gcs:shop-photos-tokyo", Environment: "tokyo", Label: "global", Expect: &BucketExpect{Locations: []string{"asia-northeast1"}}},
		{Bucket: "minio:shop-photos", Environment: "onprem", Label: sourceLabel, Expect: &BucketExpect{EndpointCIDRs: []string{"100.64.0.0/10"}}},
	}
	return c
}

func seoul() *preflight.BucketObservation {
	return &preflight.BucketObservation{Hosting: preflight.HostingProvider, Location: "ap-northeast-2", LocationType: "region",
		EndpointIPs: []string{"52.219.60.1"}, Prefixes: []string{""}, Replicas: []string{}}
}

func tokyo() *preflight.BucketObservation {
	return &preflight.BucketObservation{Hosting: preflight.HostingProvider, Location: "asia-northeast1", LocationType: "region",
		EndpointIPs: []string{"142.250.1.1"}, Prefixes: []string{}, Replicas: []string{}}
}

func onprem() *preflight.BucketObservation {
	return &preflight.BucketObservation{Hosting: preflight.HostingSelf, Location: "us-east-1", LocationType: "region",
		EndpointIPs: []string{"100.100.1.2"}, Prefixes: []string{""}, Replicas: []string{}}
}

// observedFor returns the observation the fixtures show for a bucket name.
func observedFor(name string) *preflight.BucketObservation {
	switch name {
	case "gcs:shop-photos-tokyo", "gcs:elsewhere":
		return tokyo()
	case "minio:shop-photos":
		return onprem()
	}
	return seoul()
}

func withFiles(source, target string) preflight.Request {
	r := fixtureRequest()
	r.Movements[0].Destination = "tokyo"
	r.Movements[0].Files = &preflight.BucketCopy{Source: source, Target: target, SourceObserved: observedFor(source), TargetObserved: observedFor(target)}
	return r
}

func hasReason(v preflight.ResidencyVerdict, code, bucket string) bool {
	return slices.ContainsFunc(v.Reasons, func(r preflight.Reason) bool { return r.Code == code && r.Bucket == bucket && r.Dataset == nil })
}

func evaluate(t *testing.T, c Config, r preflight.Request) preflight.ResidencyVerdict {
	t.Helper()
	v, err := mustEvaluator(t, c).Evaluate(context.Background(), r)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	return v
}

func TestBucketCopyFollowsSourceLabelAndTargetCountry(t *testing.T) {
	for _, tc := range []struct {
		label   string
		allowed bool
	}{{"global", true}, {"apac", true}, {"kr-only", false}} {
		t.Run(tc.label, func(t *testing.T) {
			v := evaluate(t, bucketConfig(tc.label), withFiles("s3:shop-photos", "gcs:shop-photos-tokyo"))
			if v.Allowed != tc.allowed {
				t.Fatalf("verdict %+v", v)
			}
			if !tc.allowed && !hasReason(v, "bucket_country_not_allowed", "s3:shop-photos") {
				t.Fatalf("reasons %+v", v.Reasons)
			}
		})
	}
}

func TestBucketCopyNeedsRegisteredBuckets(t *testing.T) {
	v := evaluate(t, bucketConfig("global"), withFiles("s3:unknown", "gcs:shop-photos-tokyo"))
	if v.Allowed || !hasReason(v, "unregistered_bucket", "s3:unknown") {
		t.Fatalf("unregistered source: %+v", v)
	}
	v = evaluate(t, bucketConfig("global"), withFiles("s3:shop-photos", "gcs:elsewhere"))
	if v.Allowed || !hasReason(v, "unregistered_bucket", "gcs:elsewhere") {
		t.Fatalf("unregistered target: %+v", v)
	}
	// A policy without buckets denies every files copy.
	v = evaluate(t, fixtureConfig(), withFiles("s3:shop-photos", "gcs:shop-photos-tokyo"))
	if v.Allowed || !hasReason(v, "unregistered_bucket", "s3:shop-photos") {
		t.Fatalf("no buckets configured: %+v", v)
	}
}

// The database still decides: an allowed bucket does not open a kr-only table.
func TestBucketDoesNotOverrideDatabase(t *testing.T) {
	c := bucketConfig("global")
	c.Catalog[0].Datasets[0].Label = "kr-only"
	v := evaluate(t, c, withFiles("s3:shop-photos", "gcs:shop-photos-tokyo"))
	if v.Allowed || !slices.ContainsFunc(v.Reasons, func(r preflight.Reason) bool { return r.Code == "country_not_allowed" }) {
		t.Fatalf("verdict %+v", v)
	}
}

// 1. Location: a declaration is not enough; the measurement must agree.
func TestBucketLocationMustBeConfirmed(t *testing.T) {
	for name, tc := range map[string]struct {
		change func(*Config, *preflight.Request)
		code   string
		bucket string
	}{
		"no expect": {func(c *Config, _ *preflight.Request) { c.Buckets[0].Expect = nil }, "bucket_location_unverified", "s3:shop-photos"},
		// The bucket says Seoul in the policy but the provider reports Virginia.
		"provider reports another region": {func(_ *Config, r *preflight.Request) {
			r.Movements[0].Files.SourceObserved.Location = "us-east-1"
		}, "bucket_location_mismatch", "s3:shop-photos"},
		"multi-region target": {func(_ *Config, r *preflight.Request) {
			o := r.Movements[0].Files.TargetObserved
			o.Location, o.LocationType = "asia", "multi-region"
		}, "bucket_multi_region", "gcs:shop-photos-tokyo"},
		// MinIO answers whatever region it is configured with: an expect of
		// locations alone cannot place a self-hosted store.
		"self-hosted with locations only": {func(c *Config, r *preflight.Request) {
			c.Buckets[2].Expect = &BucketExpect{Locations: []string{"us-east-1"}}
			r.Movements[0].Files.Source, r.Movements[0].Files.SourceObserved = "minio:shop-photos", onprem()
		}, "bucket_location_unverified", "minio:shop-photos"},
		// The remote's endpoint was changed to a cloud address.
		"self-hosted endpoint off the on-prem network": {func(_ *Config, r *preflight.Request) {
			o := onprem()
			o.EndpointIPs = []string{"100.100.1.2", "13.124.0.10"}
			r.Movements[0].Files.Source, r.Movements[0].Files.SourceObserved = "minio:shop-photos", o
		}, "bucket_endpoint_outside_network", "minio:shop-photos"},
		"self-hosted tiering": {func(_ *Config, r *preflight.Request) {
			o := onprem()
			o.Tiered = true
			r.Movements[0].Files.Source, r.Movements[0].Files.SourceObserved = "minio:shop-photos", o
		}, "bucket_tiering_not_allowed", "minio:shop-photos"},
	} {
		t.Run(name, func(t *testing.T) {
			c := bucketConfig("global")
			r := withFiles("s3:shop-photos", "gcs:shop-photos-tokyo")
			tc.change(&c, &r)
			v := evaluate(t, c, r)
			if v.Allowed || !hasReason(v, tc.code, tc.bucket) {
				t.Fatalf("want %s on %s, got %+v", tc.code, tc.bucket, v.Reasons)
			}
		})
	}
}

func TestOnPremBucketConfirmedByNetwork(t *testing.T) {
	c := bucketConfig("kr-only")
	r := withFiles("minio:shop-photos", "s3:shop-photos")
	r.Movements[0].Destination = "onprem" // the database moves aws -> onprem (both KR)
	// On-prem (KR) to Seoul (KR) with kr-only photos: allowed, and the
	// self-reported "us-east-1" region of MinIO decides nothing.
	if v := evaluate(t, c, r); !v.Allowed {
		t.Fatalf("verdict %+v", v)
	}
}

// 2. Contents: a prefix catalog works like a cluster's table catalog.
func TestBucketPrefixCatalog(t *testing.T) {
	withPrefixes := func() Config {
		c := bucketConfig("global")
		c.Buckets[0].Prefixes = []ClassifiedPrefix{{Prefix: "rooms/", Label: "global"}, {Prefix: "kyc/", Label: "kr-only"}}
		return c
	}
	request := func(prefixes ...string) preflight.Request {
		r := withFiles("s3:shop-photos", "gcs:shop-photos-tokyo")
		r.Movements[0].Files.SourceObserved.Prefixes = prefixes
		return r
	}
	t.Run("kr-only prefix blocks Tokyo", func(t *testing.T) {
		v := evaluate(t, withPrefixes(), request("", "kyc/", "rooms/"))
		if v.Allowed || len(v.Reasons) != 1 || v.Reasons[0].Code != "prefix_country_not_allowed" || v.Reasons[0].Prefix != "kyc/" {
			t.Fatalf("reasons %+v", v.Reasons)
		}
	})
	t.Run("new prefix is unclassified", func(t *testing.T) {
		c := withPrefixes()
		c.Buckets[0].Prefixes[1].Label = "global"
		v := evaluate(t, c, request("kyc/", "passports/", "rooms/"))
		if v.Allowed || len(v.Reasons) != 1 || v.Reasons[0].Code != "unclassified_prefix" || v.Reasons[0].Prefix != "passports/" {
			t.Fatalf("reasons %+v", v.Reasons)
		}
	})
	t.Run("cataloged prefix missing", func(t *testing.T) {
		c := withPrefixes()
		c.Buckets[0].Prefixes[1].Label = "global"
		v := evaluate(t, c, request("rooms/"))
		if v.Allowed || len(v.Reasons) != 1 || v.Reasons[0].Code != "prefix_catalog_mismatch" || v.Reasons[0].Prefix != "kyc/" {
			t.Fatalf("reasons %+v", v.Reasons)
		}
	})
	t.Run("bucket label covers root objects only", func(t *testing.T) {
		c := withPrefixes()
		c.Buckets[0].Label = "kr-only"
		c.Buckets[0].Prefixes[1].Label = "global"
		if v := evaluate(t, c, request("kyc/", "rooms/")); !v.Allowed {
			t.Fatalf("no root objects: %+v", v.Reasons)
		}
		v := evaluate(t, c, request("", "kyc/", "rooms/"))
		if v.Allowed || !hasReason(v, "bucket_country_not_allowed", "s3:shop-photos") {
			t.Fatalf("root objects: %+v", v.Reasons)
		}
	})
}

// 3. Onward copies: the target's replication must stay where the data may go.
func TestBucketTargetReplication(t *testing.T) {
	c := bucketConfig("apac")
	c.Buckets = append(c.Buckets,
		Bucket{Bucket: "s3dr:shop-photos-dr", Environment: "us", Label: "global", Expect: &BucketExpect{Locations: []string{"us-east-1"}}},
		Bucket{Bucket: "s3jp:shop-photos-osaka", Environment: "tokyo", Label: "global", Expect: &BucketExpect{Locations: []string{"ap-northeast-3"}}})
	for _, tc := range []struct {
		replica string
		code    string
	}{
		{"shop-photos-osaka", ""},
		{"shop-photos-dr", "bucket_replica_country_not_allowed"},
		{"somewhere-else", "bucket_replication_unregistered"},
	} {
		t.Run(tc.replica, func(t *testing.T) {
			r := withFiles("s3:shop-photos", "gcs:shop-photos-tokyo")
			r.Movements[0].Files.TargetObserved.Replicas = []string{tc.replica}
			v := evaluate(t, c, r)
			if tc.code == "" {
				if !v.Allowed {
					t.Fatalf("verdict %+v", v)
				}
				return
			}
			if v.Allowed || len(v.Reasons) != 1 || v.Reasons[0].Code != tc.code || v.Reasons[0].Replica != tc.replica || v.Reasons[0].Bucket != "gcs:shop-photos-tokyo" {
				t.Fatalf("reasons %+v", v.Reasons)
			}
		})
	}
}

func TestBucketInputAndConfigValidation(t *testing.T) {
	e := mustEvaluator(t, bucketConfig("global"))
	for name, change := range map[string]func(*preflight.Request){
		"same bucket":   func(r *preflight.Request) { *r = withFiles("s3:shop-photos", "s3:shop-photos") },
		"inline remote": func(r *preflight.Request) { *r = withFiles(":s3,access_key_id=TOPSECRET:b", "gcs:shop-photos-tokyo") },
		"empty target":  func(r *preflight.Request) { *r = withFiles("s3:shop-photos", "") },
		"no source observation": func(r *preflight.Request) {
			r.Movements[0].Files.SourceObserved = nil
		},
		"no target observation": func(r *preflight.Request) { r.Movements[0].Files.TargetObserved = nil },
		"unknown hosting":       func(r *preflight.Request) { r.Movements[0].Files.SourceObserved.Hosting = "trusted" },
		"provider without location": func(r *preflight.Request) {
			r.Movements[0].Files.SourceObserved.Location = ""
		},
		"bad ip":      func(r *preflight.Request) { r.Movements[0].Files.SourceObserved.EndpointIPs = []string{"TOPSECRET"} },
		"bad prefix":  func(r *preflight.Request) { r.Movements[0].Files.SourceObserved.Prefixes = []string{"a/b/"} },
		"dup prefix":  func(r *preflight.Request) { r.Movements[0].Files.SourceObserved.Prefixes = []string{"a/", "a/"} },
		"bad replica": func(r *preflight.Request) { r.Movements[0].Files.TargetObserved.Replicas = []string{"arn:aws:s3:::x"} },
		"self multi-reg": func(r *preflight.Request) {
			*r.Movements[0].Files.SourceObserved = *onprem()
			r.Movements[0].Files.SourceObserved.LocationType = "multi-region"
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := withFiles("s3:shop-photos", "gcs:shop-photos-tokyo")
			change(&r)
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
		"empty expect":     func(c *Config) { c.Buckets[0].Expect = &BucketExpect{} },
		"upper location":   func(c *Config) { c.Buckets[0].Expect.Locations = []string{"AP-NORTHEAST-2"} },
		"bad cidr":         func(c *Config) { c.Buckets[2].Expect.EndpointCIDRs = []string{"10.0.0.0/33"} },
		"any network":      func(c *Config) { c.Buckets[2].Expect.EndpointCIDRs = []string{"0.0.0.0/0"} },
		"nested prefix":    func(c *Config) { c.Buckets[0].Prefixes = []ClassifiedPrefix{{"a/b/", "global"}} },
		"prefix no slash":  func(c *Config) { c.Buckets[0].Prefixes = []ClassifiedPrefix{{"a", "global"}} },
		"dup prefix":       func(c *Config) { c.Buckets[0].Prefixes = []ClassifiedPrefix{{"a/", "global"}, {"a/", "apac"}} },
		"prefix label":     func(c *Config) { c.Buckets[0].Prefixes = []ClassifiedPrefix{{"a/", "secret"}} },
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
	c.Buckets[0].Expect.EndpointCIDRs = []string{"100.127.255.255/10"} // same network, unmasked
	if with.PolicyDigest() == plain || mustEvaluator(t, c).PolicyDigest() != with.PolicyDigest() {
		t.Fatal("bucket entries must change the digest, their order and CIDR spelling must not")
	}
	c.Buckets[0].Expect.EndpointCIDRs = []string{"10.0.0.0/8"}
	if mustEvaluator(t, c).PolicyDigest() == with.PolicyDigest() {
		t.Fatal("a different network must change the digest")
	}
}
