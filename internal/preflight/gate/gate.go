// Package gate decides whether a migration plan may start under the server's
// residency policy. It turns a parsed plan into a whole-cluster physical
// replication request, collects the source inventory itself and evaluates the
// embedded policy. Every failure denies.
package gate

import (
	"context"
	"errors"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/preflight"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/preflight/inventory"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/preflight/residency"
)

// Reason codes the gate adds to the policy's own. A plan's files copy is
// checked by the policy (registered buckets, unregistered_bucket otherwise).
const (
	// CodeInventoryIncomplete: the source could not be listed entirely. The
	// collector's code follows as a second reason, e.g. "inventory:connect_failed".
	CodeInventoryIncomplete = "inventory_incomplete"
	// CodeInvalidInput: the plan's identifiers are outside what the policy
	// can name (e.g. upper-case namespace).
	CodeInvalidInput = "invalid_input"
	// CodeEvaluationFailed: the policy could not be evaluated.
	CodeEvaluationFailed = "evaluation_failed"
)

// Collector lists the relations the physical copy of a source would carry.
type Collector interface {
	Collect(ctx context.Context, dsn string, scope preflight.ClusterScope) (preflight.Inventory, inventory.Result)
}

// Gate checks plans against one immutable residency policy.
type Gate struct {
	evaluator *residency.Evaluator
	collector Collector
	digest    string
}

// New prepares the policy. collector nil uses inventory.Collector{}.
func New(ctx context.Context, cfg residency.Config, collector Collector) (*Gate, error) {
	e, err := residency.New(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if collector == nil {
		collector = inventory.Collector{}
	}
	return &Gate{evaluator: e, collector: collector, digest: e.PolicyDigest()}, nil
}

// PolicyDigest identifies the embedded policy and its configuration.
func (g *Gate) PolicyDigest() string { return g.digest }

// Check returns the residency verdict for starting p. p must have its
// secrets expanded (plans.Service.Reserve): the source DSN is used to list
// the source's relations and never leaves this call. A nil error does not
// mean allowed: always check Allowed.
func (g *Gate) Check(ctx context.Context, p *plan.Plan) (preflight.ResidencyVerdict, error) {
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
	var files *preflight.BucketCopy
	if p.Files != nil {
		files = &preflight.BucketCopy{Source: p.Files.Source, Target: p.Files.Target}
	}
	scope := preflight.ClusterScope{Environment: p.Source.Env, Namespace: p.Source.Namespace, Cluster: p.Source.Cluster}
	inv, res := g.collector.Collect(ctx, p.Source.DSN, scope)
	if !inv.Complete || res.Code != "" {
		code := res.Code
		if code == "" {
			code = "unknown"
		}
		return deny(CodeInventoryIncomplete, "inventory:"+code), nil
	}
	v, err := g.evaluator.Evaluate(ctx, preflight.Request{Movements: []preflight.Movement{{
		ID:          p.ID,
		Kind:        preflight.PhysicalReplication,
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
