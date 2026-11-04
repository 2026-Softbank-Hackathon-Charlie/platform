package gate

import (
	"context"
	"errors"
	"slices"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/preflight"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/preflight/bucketprobe"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/preflight/residency"
)

// Errors of Inspect.
var (
	ErrUnknownBucket = errors.New("bucket is not registered in the residency policy")
	ErrUnknownPrefix = errors.New("prefix is not a first-level prefix of the bucket")
)

// ProbeError is a bucket that could not be measured, with the probe's code.
type ProbeError struct{ Code string }

func (e *ProbeError) Error() string { return "bucket could not be measured: " + e.Code }

// Prefix statuses in an inspection.
const (
	PrefixClassified   = "classified"
	PrefixUnclassified = "unclassified" // listed, not in the policy
	PrefixMissing      = "missing"      // in the policy, not listed
)

// PrefixStatus compares one prefix of the listing with the policy.
type PrefixStatus struct {
	Prefix string `json:"prefix"`
	Status string `json:"status"`
	Label  string `json:"label,omitempty"` // the policy's label, when classified or missing
}

// Inspection is what the policy says about a registered bucket next to what
// the bucket shows now. It helps a person or an agent write the prefix
// catalog; it is never a verdict, and nothing reads it to allow a run.
type Inspection struct {
	Bucket       string                      `json:"bucket"`
	PolicyDigest string                      `json:"policyDigest"`
	Environment  string                      `json:"environment"`
	Label        string                      `json:"label"`
	Observation  preflight.BucketObservation `json:"observation"`
	// Prefixes is every listed or cataloged prefix, sorted. Objects directly
	// under the bucket path appear as "" with the bucket label.
	Prefixes []PrefixStatus      `json:"prefixes"`
	Sample   *bucketprobe.Sample `json:"sample,omitempty"`
}

// Sampler describes the objects under a prefix (bucketprobe.Prober).
type Sampler interface {
	Sample(ctx context.Context, bucket, prefix string, n int) (bucketprobe.Sample, bucketprobe.Result)
}

// Inspect measures a registered bucket and, when prefix is set, samples the
// objects under that listed prefix. Only buckets in the policy are touched.
func (g *Gate) Inspect(ctx context.Context, bucket, prefix string) (Inspection, error) {
	if g == nil || g.evaluator == nil {
		return Inspection{}, residency.ErrNotReady
	}
	entry, ok := g.buckets[bucket]
	if !ok {
		return Inspection{}, ErrUnknownBucket
	}
	obs, res := g.prober.Probe(ctx, bucket, bucketprobe.Options{Prefixes: true})
	if res.Code != "" {
		return Inspection{}, &ProbeError{Code: res.Code}
	}
	in := Inspection{Bucket: bucket, PolicyDigest: g.digest, Environment: entry.Environment, Label: entry.Label, Observation: obs, Prefixes: []PrefixStatus{}}
	cataloged := map[string]string{}
	for _, p := range entry.Prefixes {
		cataloged[p.Prefix] = p.Label
	}
	for _, p := range obs.Prefixes {
		switch label, ok := cataloged[p]; {
		case p == "":
			in.Prefixes = append(in.Prefixes, PrefixStatus{Prefix: "", Status: PrefixClassified, Label: entry.Label})
		case ok:
			in.Prefixes = append(in.Prefixes, PrefixStatus{Prefix: p, Status: PrefixClassified, Label: label})
		default:
			in.Prefixes = append(in.Prefixes, PrefixStatus{Prefix: p, Status: PrefixUnclassified})
		}
	}
	for p, label := range cataloged {
		if !slices.Contains(obs.Prefixes, p) {
			in.Prefixes = append(in.Prefixes, PrefixStatus{Prefix: p, Status: PrefixMissing, Label: label})
		}
	}
	slices.SortFunc(in.Prefixes, func(a, b PrefixStatus) int {
		switch {
		case a.Prefix < b.Prefix:
			return -1
		case a.Prefix > b.Prefix:
			return 1
		}
		return 0
	})
	if prefix == "" {
		return in, nil
	}
	if !slices.Contains(obs.Prefixes, prefix) || prefix == "" {
		return Inspection{}, ErrUnknownPrefix
	}
	s, ok := g.prober.(Sampler)
	if !ok {
		return Inspection{}, &ProbeError{Code: bucketprobe.CodeUnsupportedBackend}
	}
	sample, res := s.Sample(ctx, bucket, prefix, 0)
	if res.Code != "" {
		return Inspection{}, &ProbeError{Code: res.Code}
	}
	in.Sample = &sample
	return in, nil
}
