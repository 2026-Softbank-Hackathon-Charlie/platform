package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/preflight"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/preflight/bucketprobe"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/preflight/gate"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/runs"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

type fakeInspector struct{ bucket, prefix string }

func (f *fakeInspector) Inspect(_ context.Context, bucket, prefix string) (gate.Inspection, error) {
	f.bucket, f.prefix = bucket, prefix
	switch {
	case bucket != "minio:photos":
		return gate.Inspection{}, gate.ErrUnknownBucket
	case prefix == "gone/":
		return gate.Inspection{}, gate.ErrUnknownPrefix
	case prefix == "down/":
		return gate.Inspection{}, &gate.ProbeError{Code: bucketprobe.CodeTimeout}
	}
	in := gate.Inspection{
		Bucket: bucket, PolicyDigest: strings.Repeat("a", 64), Environment: "onprem", Label: "apac",
		Observation: preflight.BucketObservation{Hosting: preflight.HostingSelf, EndpointIPs: []string{"100.64.0.7"},
			Prefixes: []string{"", "hosts/", "rooms/"}, Replicas: []string{}},
		Prefixes: []gate.PrefixStatus{
			{Prefix: "", Status: gate.PrefixClassified, Label: "apac"},
			{Prefix: "hosts/", Status: gate.PrefixUnclassified},
			{Prefix: "rooms/", Status: gate.PrefixClassified, Label: "apac"},
		},
	}
	if prefix != "" {
		in.Sample = &bucketprobe.Sample{Prefix: prefix, Objects: 2, ContentTypes: map[string]int{"image/jpeg": 2},
			Extensions: map[string]int{".jpg": 2}, MetadataKeys: map[string]int{}, KeyPatterns: map[string]int{"kr_resident_number": 2},
			MinSize: 10, MaxSize: 20, MaxDepth: 1}
	}
	return in, nil
}

func withBuckets(b BucketInspector) option {
	return func(_ *harness, _ *runs.Config, d *Deps) { d.Buckets = b }
}

func TestInspectBucket(t *testing.T) {
	f := &fakeInspector{}
	h := newHarness(t, withBuckets(f))

	code, body := h.do("POST", "/v1/residency/buckets/inspect", agentToken, map[string]any{"bucket": "minio:photos", "prefix": "hosts/"})
	if code != http.StatusOK {
		t.Fatalf("%d %s", code, body)
	}
	var in apiv1.BucketInspection
	if err := json.Unmarshal(body, &in); err != nil {
		t.Fatal(err)
	}
	if f.prefix != "hosts/" || in.Prefixes[1].Status != apiv1.BucketPrefixStatusStatusUnclassified || in.Sample.KeyPatterns["kr_resident_number"] != 2 {
		t.Fatalf("%s", body)
	}

	for name, tc := range map[string]struct {
		token string
		body  map[string]any
		code  int
		err   string
	}{
		"viewer may not":    {viewerToken, map[string]any{"bucket": "minio:photos"}, http.StatusForbidden, "forbidden"},
		"operator may":      {opToken, map[string]any{"bucket": "minio:photos"}, http.StatusOK, ""},
		"unknown bucket":    {agentToken, map[string]any{"bucket": "s3:other"}, http.StatusNotFound, "bucket_not_registered"},
		"prefix not listed": {agentToken, map[string]any{"bucket": "minio:photos", "prefix": "gone/"}, http.StatusNotFound, "prefix_not_listed"},
		"probe failed":      {agentToken, map[string]any{"bucket": "minio:photos", "prefix": "down/"}, http.StatusBadGateway, "bucket_probe_failed"},
		"no bucket":         {agentToken, map[string]any{}, http.StatusBadRequest, "invalid_request"},
	} {
		t.Run(name, func(t *testing.T) {
			code, body := h.do("POST", "/v1/residency/buckets/inspect", tc.token, tc.body)
			if code != tc.code || (tc.err != "" && !strings.Contains(string(body), tc.err)) {
				t.Fatalf("%d %s", code, body)
			}
		})
	}
}

func TestInspectBucketWithoutPolicy(t *testing.T) {
	h := newHarness(t)
	code, body := h.do("POST", "/v1/residency/buckets/inspect", agentToken, map[string]any{"bucket": "minio:photos"})
	if code != http.StatusServiceUnavailable || !strings.Contains(string(body), "residency_not_configured") {
		t.Fatalf("%d %s", code, body)
	}
}
