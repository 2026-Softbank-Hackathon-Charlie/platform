package residency

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/preflight"
)

func TestContentLabelsOnlyAddRestrictions(t *testing.T) {
	r := withFiles("s3:shop-photos", "gcs:shop-photos-tokyo")
	r.Movements[0].Files.ContentLabels = []string{"global", "kr-only"}
	v := evaluate(t, bucketConfig("global"), r)
	if v.Allowed || !hasReason(v, "bucket_country_not_allowed", "s3:shop-photos") {
		t.Fatalf("content-derived KR restriction ignored: %+v", v)
	}
	r.Movements[0].Files.ContentLabels = []string{"global"}
	v = evaluate(t, bucketConfig("kr-only"), r)
	if v.Allowed {
		t.Fatal("global scan overrode bucket label")
	}
	c := bucketConfig("global")
	c.Buckets[0].Prefixes = []ClassifiedPrefix{{Prefix: "private/", Label: "kr-only"}}
	r.Movements[0].Files.SourceObserved.Prefixes = []string{"private/"}
	v = evaluate(t, c, r)
	if v.Allowed || !hasReason(v, "prefix_country_not_allowed", "s3:shop-photos") {
		t.Fatalf("scan overrode prefix: %+v", v)
	}
}

func TestContentRestrictionAppliesToOnwardReplicas(t *testing.T) {
	c := bucketConfig("global")
	c.Buckets = append(c.Buckets, Bucket{Bucket: "archive:backup", Environment: "us", Label: "global"})
	r := withFiles("s3:shop-photos", "minio:shop-photos")
	r.Movements[0].Files.ContentLabels = []string{"kr-only"}
	r.Movements[0].Files.TargetObserved.Replicas = []string{"backup"}
	v := evaluate(t, c, r)
	if v.Allowed || !hasReason(v, "bucket_replica_country_not_allowed", "minio:shop-photos") {
		t.Fatalf("content escaped through replication: %+v", v)
	}
}

func TestContentLabelsRejectUntrustedVocabulary(t *testing.T) {
	for _, labels := range [][]string{{"unknown"}, {"global", "global"}, {"global", "apac", "kr-only", "global"}} {
		r := withFiles("s3:shop-photos", "gcs:shop-photos-tokyo")
		r.Movements[0].Files.ContentLabels = labels
		if v, err := mustEvaluator(t, bucketConfig("global")).Evaluate(context.Background(), r); !errors.Is(err, ErrInvalidInput) || v.Allowed {
			t.Fatalf("invalid content labels accepted: %+v %v", v, err)
		}
	}
}

func TestReviewedFilesValidationAndPolicyDigest(t *testing.T) {
	c := bucketConfig("global")
	c.InspectionEnvironment = "aws"
	c.Buckets[0].ReviewedFiles = []preflight.ContentReview{{SHA256: strings.Repeat("a", 64), Label: "global"}, {SHA256: strings.Repeat("b", 64), Label: "kr-only"}}
	a := mustEvaluator(t, c).PolicyDigest()
	slices.Reverse(c.Buckets[0].ReviewedFiles)
	if b := mustEvaluator(t, c).PolicyDigest(); a != b {
		t.Fatal("review ordering changed policy digest")
	}
	c.Buckets[0].ReviewedFiles[0].Label = "apac"
	if b := mustEvaluator(t, c).PolicyDigest(); a == b {
		t.Fatal("review classification not bound to policy digest")
	}
	for _, review := range []preflight.ContentReview{{SHA256: "short", Label: "global"}, {SHA256: strings.Repeat("A", 64), Label: "global"}, {SHA256: strings.Repeat("a", 64), Label: "unknown"}} {
		c.Buckets[0].ReviewedFiles = []preflight.ContentReview{review}
		if _, err := c.normalized(); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("bad review accepted: %v", err)
		}
	}
	c.Buckets[0].ReviewedFiles = []preflight.ContentReview{{SHA256: strings.Repeat("a", 64), Label: "global"}, {SHA256: strings.Repeat("a", 64), Label: "kr-only"}}
	if _, err := c.normalized(); !errors.Is(err, ErrInvalidConfig) {
		t.Fatal("conflicting duplicate review accepted")
	}
	c.Buckets[0].ReviewedFiles = nil
	for _, env := range []string{"tokyo", "missing"} {
		c.InspectionEnvironment = env
		if _, err := c.normalized(); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("unsafe inspection location accepted: %s", env)
		}
	}
}
