package plans

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

const rollbackSuffix = "-rollback"

// Rollback stores the reverse of plan ref as a new plan that is already
// approved: the operator asking for the rollback is the human approval, and
// comment records why. The reverse is built from the plan JSON as submitted,
// so ${VAR} references stay unexpanded and no secret reaches the store. It
// passes the same policy as a submitted plan.
func (s *Service) Rollback(ctx context.Context, ref, actor, comment string) (apiv1.PlanRecord, error) {
	orig, err := s.get(ctx, ref)
	if err != nil {
		return apiv1.PlanRecord{}, err
	}
	raw, err := reversePlan(orig.Raw)
	if err != nil {
		return apiv1.PlanRecord{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	p, err := s.Parse(raw)
	if err != nil {
		return apiv1.PlanRecord{}, err
	}
	sum, err := json.Marshal(Summarize(p))
	if err != nil {
		return apiv1.PlanRecord{}, err
	}
	now := s.now()
	row := store.Plan{
		Ref:           newRef("pln_"),
		PlanID:        p.ID,
		Status:        store.PlanApproved,
		Summary:       sum,
		Raw:           raw,
		SubmittedBy:   actor,
		SubmittedAt:   now,
		ReviewedBy:    actor,
		ReviewedAt:    &now,
		ReviewComment: comment,
	}
	if err := s.store.CreatePlan(ctx, row); err != nil {
		return apiv1.PlanRecord{}, err
	}
	return s.recordOf(ctx, row)
}

// reversePlan swaps source and target of a submitted plan: the endpoints
// (env, context, namespace, cluster, dsn), the file remotes and the gateway
// backends. The target manifest is dropped: after a cutover the old source
// still exists as a replica of the new primary, so nothing has to be created.
func reversePlan(raw []byte) ([]byte, error) {
	var p apiv1.EnginePlan
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("decode the original plan: %w", err)
	}
	p.Id = rollbackID(p.Id)
	p.Source, p.Target = p.Target, p.Source
	p.Source.Manifest, p.Target.Manifest = nil, nil
	if p.Files != nil {
		p.Files.Source, p.Files.Target = p.Files.Target, p.Files.Source
	}
	if g := p.Gateway; g != nil {
		// An unset backend defaults to its endpoint's env, which moved with the endpoint.
		g.SourceBackend, g.TargetBackend = g.TargetBackend, g.SourceBackend
	}
	return json.MarshalIndent(p, "", "  ")
}

// rollbackID appends -rollback, shortening the original id so the result
// still fits the 63 characters a plan id may have.
func rollbackID(id string) string {
	if limit := 63 - len(rollbackSuffix); len(id) > limit {
		id = strings.TrimRight(id[:limit], "-")
	}
	return id + rollbackSuffix
}
