// Package gate decides whether a migration plan may start under the server's
// residency policy. It turns a parsed plan into a whole-cluster physical
// replication request, collects the source inventory itself and evaluates the
// embedded policy. Every failure denies.
package gate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/engine"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/fileguard"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/plans"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/preflight"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/preflight/bucketprobe"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/preflight/inventory"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/preflight/residency"
)

// Reason codes the gate adds to the policy's own. A plan's files copy is
// measured here (bucketprobe) and checked by the policy.
const (
	// CodeInventoryIncomplete: the source could not be listed entirely. The
	// collector's code follows as a second reason, e.g. "inventory:connect_failed".
	CodeInventoryIncomplete = "inventory_incomplete"
	// CodeInvalidInput: the plan's identifiers are outside what the policy
	// can name (e.g. upper-case namespace).
	CodeInvalidInput = "invalid_input"
	// CodeEvaluationFailed: the policy could not be evaluated.
	CodeEvaluationFailed = "evaluation_failed"
	// CodeBucketProbeIncomplete: a files copy's bucket could not be measured.
	// The probe's code follows as a second reason, e.g.
	// "bucket_probe:remote_not_configured"; both name the bucket.
	CodeBucketProbeIncomplete = "bucket_probe_incomplete"
)

// Prober measures a bucket through the rclone remote the copy will use.
type Prober interface {
	Probe(ctx context.Context, bucket string, opt bucketprobe.Options) (preflight.BucketObservation, bucketprobe.Result)
}

// Collector lists the relations the physical copy of a source would carry.
type Collector interface {
	Collect(ctx context.Context, dsn string, scope preflight.ClusterScope) (preflight.Inventory, inventory.Result)
}

// ContentInspector reads all source bytes into private staging, without an upload.
type ContentInspector interface {
	Inspect(context.Context, string, []fileguard.Review, string) (fileguard.Report, error)
}

// Gate checks plans against one immutable residency policy.
type Gate struct {
	evaluator *residency.Evaluator
	collector Collector
	prober    Prober
	// registered buckets are the only ones the gate connects to.
	registered            map[string]bool
	buckets               map[string]residency.Bucket
	digest                string
	inspector             ContentInspector
	inspectionEnvironment string
	classificationCache   *fileguard.ClassificationCache
}

// New prepares the policy. collector nil uses inventory.Collector{}; the
// buckets of a files copy are measured with bucketprobe.Prober{}.
func New(ctx context.Context, cfg residency.Config, collector Collector) (*Gate, error) {
	// Keep the measured catalog/reviews just as immutable as the OPA data.
	raw, err := json.Marshal(cfg)
	if err != nil {
		return nil, residency.ErrInvalidConfig
	}
	cfg, err = residency.ParseConfig(raw)
	if err != nil {
		return nil, err
	}
	e, err := residency.New(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if collector == nil {
		collector = inventory.Collector{}
	}
	registered, buckets := map[string]bool{}, map[string]residency.Bucket{}
	for _, b := range cfg.Buckets {
		registered[b.Bucket], buckets[b.Bucket] = true, b
	}
	cache := fileguard.NewClassificationCache(fileguard.MaxClassificationEntries)
	return &Gate{evaluator: e, collector: collector, prober: bucketprobe.Prober{}, registered: registered, buckets: buckets, digest: e.PolicyDigest(), inspector: fileguard.NewInspector(cache), inspectionEnvironment: cfg.InspectionEnvironment, classificationCache: cache}, nil
}

// WithProber replaces the bucket prober (tests). It returns g.
func (g *Gate) WithProber(p Prober) *Gate {
	g.prober = p
	return g
}

// WithContentInspector replaces content reads in tests, never in production wiring.
func (g *Gate) WithContentInspector(i ContentInspector) *Gate { g.inspector = i; return g }

// PolicyDigest identifies the embedded policy and its configuration.
func (g *Gate) PolicyDigest() string { return g.digest }

// Check returns the residency verdict for starting p. p must have its
// secrets expanded (plans.Service.Reserve): the source DSN is used to list
// the source's relations and never leaves this call. A nil error does not
// mean allowed: always check Allowed.
func (g *Gate) Check(ctx context.Context, p *plan.Plan) (preflight.ResidencyVerdict, error) {
	v, err := g.check(ctx, p, nil)
	if err != nil || !v.Allowed || p.Files == nil {
		return v, err
	}
	if g.inspector == nil {
		v.Allowed = false
		v.Reasons = []preflight.Reason{{Code: "file_inspection_failed"}}
		return v, nil
	}
	inspect := g.inspector.Inspect
	if scoped, ok := g.inspector.(interface {
		InspectBound(context.Context, string, []fileguard.Review, string, string) (fileguard.Report, error)
	}); ok {
		inspect = func(ctx context.Context, source string, reviews []fileguard.Review, binary string) (fileguard.Report, error) {
			return scoped.InspectBound(ctx, source, reviews, binary, g.inspectionBinding(p))
		}
	}
	r, err := inspect(ctx, p.Files.Source, g.buckets[p.Files.Source].ReviewedFiles, g.buckets[p.Files.Source].BinaryLabel)
	if err != nil {
		v.Allowed = false
		v.Reasons = []preflight.Reason{{Code: "file_inspection_failed", MovementID: movementID(p)}}
		for _, fixed := range []error{fileguard.ErrUnsupported, fileguard.ErrLimit, fileguard.ErrRead, fileguard.ErrPath, fileguard.ErrChanged, fileguard.ErrCleanup, fileguard.ErrOptions} {
			if errors.Is(err, fixed) {
				v.Reasons = append(v.Reasons, preflight.Reason{Code: fixed.Error(), MovementID: movementID(p)})
				break
			}
		}
		return v, nil // scanner/command errors must not expose object keys or values
	}
	v, err = g.check(ctx, p, r.Labels)
	v.Content = &r
	return v, err
}

// FileSyncer is installed for normal starts AND resumes. The runner retains the
// existing explicit rollback exemption; this is not a global rollback policy.
func (g *Gate) FileSyncer(p *plan.Plan) engine.FileSyncer {
	before := func(ctx context.Context) error {
		v, err := g.check(ctx, p, nil)
		if err != nil || !v.Allowed {
			return fileguard.ErrDenied
		}
		return nil
	}
	authorize := func(ctx context.Context, r fileguard.Report) error {
		v, err := g.check(ctx, p, r.Labels)
		if err != nil || !v.Allowed {
			return fileguard.ErrDenied
		}
		return nil
	}
	return fileguard.New(p.Files.Source, p.Files.Target, g.buckets[p.Files.Source].ReviewedFiles, g.buckets[p.Files.Source].BinaryLabel, before, authorize).WithClassificationCache(g.classificationCache, g.inspectionBinding(p))
}

// An endpoint pair is unordered so an ordinary reverse migration can reuse
// content classification. The policy, both concrete app scopes and the gateway
// app route remain bound; classification policy is additionally keyed by the
// scanner. No DSN/token or caller-chosen plan/run ID participates.
func (g *Gate) inspectionBinding(p *plan.Plan) string {
	if g == nil || p == nil || p.Files == nil {
		return ""
	}
	endpoint := func(e plan.Endpoint, bucket string) string {
		raw, _ := json.Marshal([]string{e.Env, e.Namespace, e.Cluster, e.Context, e.Kubeconfig, bucket})
		return string(raw)
	}
	ends := []string{endpoint(p.Source, p.Files.Source), endpoint(p.Target, p.Files.Target)}
	slices.Sort(ends)
	raw, _ := json.Marshal([]any{g.digest, p.Gateway.URL, ends})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func (g *Gate) check(ctx context.Context, p *plan.Plan, labels []string) (preflight.ResidencyVerdict, error) {
	deny := func(codes ...string) preflight.ResidencyVerdict {
		v := preflight.ResidencyVerdict{PolicyDigest: g.digest, Reasons: []preflight.Reason{}}
		for _, c := range codes {
			v.Reasons = append(v.Reasons, preflight.Reason{Code: c, MovementID: movementID(p)})
		}
		return v
	}
	if g == nil || g.evaluator == nil || g.collector == nil {
		return preflight.ResidencyVerdict{Reasons: []preflight.Reason{{Code: "not_ready"}}}, residency.ErrNotReady
	}
	if p == nil {
		return deny(CodeInvalidInput), residency.ErrInvalidInput
	}
	kind := preflight.PhysicalReplication
	scope := preflight.ClusterScope{Environment: p.Source.Env, Namespace: p.Source.Namespace, Cluster: p.Source.Cluster}
	var inv preflight.Inventory
	if plans.NoDatabase(p) {
		// Only files move (ADR 0049): no database to list, so the movement
		// is the object copy alone and the files rules decide.
		if p.Files == nil {
			return deny(CodeInvalidInput), residency.ErrInvalidInput
		}
		kind, scope = preflight.ObjectCopy, preflight.ClusterScope{Environment: p.Source.Env}
	} else {
		var res inventory.Result
		inv, res = g.collector.Collect(ctx, p.Source.DSN, scope)
		if !inv.Complete || res.Code != "" {
			code := res.Code
			if code == "" {
				code = "unknown"
			}
			return deny(CodeInventoryIncomplete, "inventory:"+code), nil
		}
	}
	// The source's prefixes decide what the copy carries; the target's
	// replication decides where it goes next.
	var files *preflight.BucketCopy
	var evidence []preflight.ObservedBucket
	if p.Files != nil {
		if !residency.ValidBucket(p.Files.Source) || !residency.ValidBucket(p.Files.Target) {
			return deny(CodeInvalidInput), residency.ErrInvalidInput
		}
		if len(p.Files.Flags) != 0 || (p.Files.Rclone != "" && p.Files.Rclone != "rclone") {
			return deny("file_guard_options_not_supported"), nil
		}
		files = &preflight.BucketCopy{Source: p.Files.Source, Target: p.Files.Target, ContentLabels: labels}
		// A store the policy does not know is never contacted.
		var unregistered []preflight.Reason
		for _, name := range []string{p.Files.Source, p.Files.Target} {
			if !g.registered[name] && residency.ValidBucket(name) {
				unregistered = append(unregistered, preflight.Reason{Code: "unregistered_bucket", MovementID: movementID(p), Bucket: name})
			}
		}
		if len(unregistered) > 0 {
			v := deny()
			v.Reasons = unregistered
			return v, nil
		}
		for _, b := range []struct {
			name string
			opt  bucketprobe.Options
			obs  **preflight.BucketObservation
		}{
			{p.Files.Source, bucketprobe.Options{Prefixes: true}, &files.SourceObserved},
			{p.Files.Target, bucketprobe.Options{Replication: true}, &files.TargetObserved},
		} {
			obs, res := g.prober.Probe(ctx, b.name, b.opt)
			if res.Code != "" {
				bucket := ""
				if residency.ValidBucket(b.name) {
					bucket = b.name
				}
				v := deny()
				for _, c := range []string{CodeBucketProbeIncomplete, "bucket_probe:" + res.Code} {
					v.Reasons = append(v.Reasons, preflight.Reason{Code: c, MovementID: movementID(p), Bucket: bucket})
				}
				return v, nil
			}
			*b.obs = &obs
			evidence = append(evidence, preflight.ObservedBucket{Bucket: b.name, Observation: obs})
		}
	}
	v, err := g.evaluator.Evaluate(ctx, preflight.Request{Movements: []preflight.Movement{{
		ID:          p.ID,
		Kind:        kind,
		Source:      scope,
		Destination: p.Target.Env,
		Inventory:   inv,
		Files:       files,
	}}})
	switch {
	case errors.Is(err, residency.ErrInvalidInput):
		return deny(CodeInvalidInput), err
	case err != nil:
		return deny(CodeEvaluationFailed), err
	}
	v.Buckets = evidence
	if v.Allowed && p.Files != nil && g.inspectionEnvironment == "" {
		v.Allowed = false
		v.Reasons = []preflight.Reason{{Code: "inspection_location_unverified", MovementID: movementID(p)}}
	}
	return v, nil
}

// movementID is the plan id when it is a valid identifier, so a reason never
// echoes arbitrary input.
func movementID(p *plan.Plan) string {
	if p != nil && residency.ValidResourceID(p.ID) {
		return p.ID
	}
	return ""
}
