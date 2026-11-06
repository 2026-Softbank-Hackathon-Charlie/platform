package residency

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/netip"
	"regexp"
	"slices"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/preflight"
	"github.com/open-policy-agent/opa/v1/rego"
)

//go:embed policy.rego
var policySource string

var (
	ErrNotReady     = errors.New("residency evaluator is not initialized")
	ErrInvalidInput = errors.New("invalid residency input")
	ErrEvaluation   = errors.New("residency evaluation failed")
)

// Evaluator owns a prepared query and an immutable policy snapshot. It is safe
// for concurrent Evaluate calls. Use New; a nil or zero evaluator denies.
type Evaluator struct {
	query  *rego.PreparedEvalQuery
	digest string
}

func New(ctx context.Context, cfg Config) (*Evaluator, error) {
	return prepare(ctx, cfg, policySource)
}

func prepare(ctx context.Context, cfg Config, source string) (*Evaluator, error) {
	cfg, err := cfg.normalized()
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		return nil, ErrInvalidConfig
	}
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, ErrInvalidConfig
	}
	h := sha256.New()
	h.Write([]byte("hopcharlie-residency-v1\x00"))
	h.Write([]byte(source))
	h.Write([]byte{0})
	h.Write(raw)
	q, err := rego.New(
		rego.Query("data.hopcharlie.residency.decision"),
		rego.Module("residency.rego", source),
		rego.Data(map[string]any{"policy": data}),
		rego.StrictBuiltinErrors(true),
		rego.UnsafeBuiltins(map[string]struct{}{"http.send": {}, "net.lookup_ip_addr": {}, "opa.runtime": {}}),
	).PrepareForEval(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, ErrEvaluation // SDK errors may contain input or policy data.
	}
	return &Evaluator{query: &q, digest: hex.EncodeToString(h.Sum(nil))}, nil
}

// PolicyDigest identifies the embedded policy and its normalized configuration.
// It is empty for a nil or zero evaluator.
func (e *Evaluator) PolicyDigest() string {
	if e == nil {
		return ""
	}
	return e.digest
}

func (e *Evaluator) Evaluate(ctx context.Context, request preflight.Request) (preflight.ResidencyVerdict, error) {
	v := preflight.ResidencyVerdict{Reasons: []preflight.Reason{}}
	deny := func(code string, err error) (preflight.ResidencyVerdict, error) {
		v.Allowed = false
		v.Reasons = []preflight.Reason{{Code: code}}
		return v, err
	}
	if e == nil || e.query == nil || e.digest == "" {
		return deny("not_ready", ErrNotReady)
	}
	v.PolicyDigest = e.digest
	if err := ctx.Err(); err != nil {
		return deny("evaluation_cancelled", err)
	}
	if !validRequest(request) {
		return deny("invalid_input", ErrInvalidInput)
	}
	results, err := e.query.Eval(ctx, rego.EvalInput(request))
	if ctx.Err() != nil {
		return deny("evaluation_cancelled", ctx.Err())
	}
	if err != nil {
		return deny("evaluation_failed", ErrEvaluation)
	}
	allowed, reasons, err := decodeDecision(results)
	if err != nil {
		return deny("evaluation_failed", ErrEvaluation)
	}
	v.Allowed, v.Reasons = allowed, reasons
	return v, nil
}

func validRequest(r preflight.Request) bool {
	if len(r.Movements) == 0 {
		return false
	}
	ids := map[string]bool{}
	for _, m := range r.Movements {
		if !resourceID.MatchString(m.ID) || ids[m.ID] || m.Kind != preflight.PhysicalReplication ||
			!validScope(m.Source) || !resourceID.MatchString(m.Destination) || m.Source.Environment == m.Destination ||
			!m.Inventory.Complete || m.Inventory.Scope != m.Source || len(m.Inventory.Datasets) == 0 {
			return false
		}
		ids[m.ID] = true
		if f := m.Files; f != nil && (!ValidBucket(f.Source) || !ValidBucket(f.Target) || f.Source == f.Target ||
			!validObservation(f.SourceObserved) || !validObservation(f.TargetObserved)) {
			return false
		}
		if m.Files != nil && (len(m.Files.ContentLabels) > 3 || !distinct(m.Files.ContentLabels, validLabel)) {
			return false
		}
		datasets := map[preflight.Dataset]bool{}
		for _, d := range m.Inventory.Datasets {
			if !validDataset(d) || datasets[d] {
				return false
			}
			datasets[d] = true
		}
	}
	return true
}

// Bounds on an observation, far above what a real bucket shows; the probe
// reports a larger listing as incomplete instead of truncating it.
const (
	MaxPrefixes    = 1000
	maxEndpointIPs = 32
	maxReplicas    = 64
)

// replicaID is a bucket name as a replication rule names it.
var replicaID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,221}$`)

// ValidReplica reports whether s is a replica bucket name the policy can name.
func ValidReplica(s string) bool { return replicaID.MatchString(s) }

func validObservation(o *preflight.BucketObservation) bool {
	if o == nil || len(o.Prefixes) > MaxPrefixes || len(o.EndpointIPs) > maxEndpointIPs || len(o.Replicas) > maxReplicas {
		return false
	}
	switch o.Hosting {
	case preflight.HostingProvider:
		if !locationID.MatchString(o.Location) || !slices.Contains([]string{"region", "dual-region", "multi-region"}, o.LocationType) {
			return false
		}
	case preflight.HostingSelf:
		if (o.Location != "" && !locationID.MatchString(o.Location)) || (o.LocationType != "" && o.LocationType != "region") {
			return false
		}
	default:
		return false
	}
	return distinct(o.EndpointIPs, func(s string) bool { a, err := netip.ParseAddr(s); return err == nil && a.String() == s }) &&
		distinct(o.Prefixes, func(s string) bool { return s == "" || prefixID.MatchString(s) }) &&
		distinct(o.Replicas, replicaID.MatchString)
}

func distinct(xs []string, valid func(string) bool) bool {
	seen := map[string]bool{}
	for _, x := range xs {
		if !valid(x) || seen[x] {
			return false
		}
		seen[x] = true
	}
	return true
}

// An undefined query, missing boolean, null reasons or inconsistent verdict is
// an evaluation failure, never an implicit allow. Only fixed policy codes pass.
func decodeDecision(rs rego.ResultSet) (bool, []preflight.Reason, error) {
	if len(rs) != 1 || len(rs[0].Expressions) != 1 {
		return false, nil, ErrEvaluation
	}
	raw, err := json.Marshal(rs[0].Expressions[0].Value)
	if err != nil {
		return false, nil, ErrEvaluation
	}
	var d struct {
		Allowed *bool              `json:"allowed"`
		Reasons []preflight.Reason `json:"reasons"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&d); err != nil || d.Allowed == nil || d.Reasons == nil || *d.Allowed != (len(d.Reasons) == 0) {
		return false, nil, ErrEvaluation
	}
	for _, r := range d.Reasons {
		if !resourceID.MatchString(r.MovementID) {
			return false, nil, ErrEvaluation
		}
		ok := false
		switch r.Code {
		case "unknown_source", "unknown_destination", "unclassified_cluster":
			ok = r.Dataset == nil && r.Bucket == "" && r.Prefix == "" && r.Replica == ""
		case "unclassified_dataset", "inventory_catalog_mismatch", "country_not_allowed":
			ok = r.Dataset != nil && validDataset(*r.Dataset) && r.Bucket == "" && r.Prefix == "" && r.Replica == ""
		case "unregistered_bucket", "bucket_country_not_allowed", "bucket_location_unverified", "bucket_location_mismatch",
			"bucket_multi_region", "bucket_endpoint_outside_network", "bucket_tiering_not_allowed":
			ok = r.Dataset == nil && ValidBucket(r.Bucket) && r.Prefix == "" && r.Replica == ""
		case "unclassified_prefix", "prefix_catalog_mismatch", "prefix_country_not_allowed":
			ok = r.Dataset == nil && ValidBucket(r.Bucket) && prefixID.MatchString(r.Prefix) && r.Replica == ""
		case "bucket_replication_unregistered", "bucket_replica_country_not_allowed":
			ok = r.Dataset == nil && ValidBucket(r.Bucket) && r.Prefix == "" && replicaID.MatchString(r.Replica)
		}
		if !ok {
			return false, nil, ErrEvaluation
		}
	}
	slices.SortFunc(d.Reasons, func(a, b preflight.Reason) int {
		var ad, bd preflight.Dataset
		if a.Dataset != nil {
			ad = *a.Dataset
		}
		if b.Dataset != nil {
			bd = *b.Dataset
		}
		return cmp.Or(cmp.Compare(a.MovementID, b.MovementID), cmp.Compare(a.Code, b.Code), compareDataset(ad, bd), cmp.Compare(a.Bucket, b.Bucket),
			cmp.Compare(a.Prefix, b.Prefix), cmp.Compare(a.Replica, b.Replica))
	})
	return *d.Allowed, d.Reasons, nil
}
