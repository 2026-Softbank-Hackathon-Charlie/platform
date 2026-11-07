package plans

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/preflight/parity"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
)

// ParityGate checks the target's identity and monitoring against what the
// app needs (paritygate.Gate). Its report is recorded, never enforced.
type ParityGate interface {
	Check(ctx context.Context, p *plan.Plan) (parity.Report, error)
}

// SetParityGate makes Reserve record a parity report for every new run.
// The report never stops a run (audit mode). Without a gate nothing runs.
func (s *Service) SetParityGate(g ParityGate) { s.parity = g }

// Parity outcomes recorded on a reservation.
const (
	ParityNotConfigured  = "not_configured"
	ParityRollbackExempt = "rollback_exempt"
	// ParityRecorded: the probes ran; Report says what they found.
	ParityRecorded = "recorded"
	// ParityNoApp: the parity file has no app for the target namespace.
	ParityNoApp = "no_app"
	// ParityError: the check itself could not run (invalid request, cancelled).
	ParityError = "error"
	// ParityNoDatabase: the plan moves an app without a database (ADR
	// 0049); its probes look at a CNPG cluster, which it has none of ("DB 없음").
	ParityNoDatabase = "no_database"
)

// ParityCheck is what was recorded for a reservation, for the audit log.
type ParityCheck struct {
	Outcome string
	Report  *parity.Report
	Binding *ParityBinding
}

// ParityBinding binds start-time evidence to the approved, unexpanded content.
// It is an audit record, not authorization to reuse evidence for another run.
type ParityBinding struct {
	PlanRef        string     `json:"planRef"`
	Revision       int64      `json:"revision"`
	PlanDigest     string     `json:"planDigest"`
	ManifestDigest string     `json:"manifestDigest,omitempty"`
	ReviewedAt     *time.Time `json:"reviewedAt,omitempty"`
	Operation      string     `json:"operation"`
	DurabilityMode string     `json:"durabilityMode"`
	PolicyDigest   string     `json:"policyDigest"`
}

func (s *Service) bindParity(ctx context.Context, row store.Plan, check *ParityCheck) {
	sum := sha256.Sum256(row.Raw)
	binding := &ParityBinding{PlanRef: row.Ref, Revision: row.Revision, PlanDigest: hex.EncodeToString(sum[:]), ReviewedAt: row.ReviewedAt}
	binding.Operation = "planned_migration"
	if row.RollbackOf != "" {
		binding.Operation = "planned_reverse"
	}
	binding.DurabilityMode = "asynchronous_environment_rpo_unverified"
	policy, err := json.Marshal(s.policy)
	if err != nil {
		check.Outcome, check.Report = ParityError, nil
	} else {
		digest := sha256.Sum256(policy)
		binding.PolicyDigest = hex.EncodeToString(digest[:])
	}
	name, usesManifest := manifestName(row)
	if usesManifest {
		attachment, err := s.store.GetPlanAttachment(ctx, row.Ref, name)
		if err != nil {
			check.Outcome, check.Report = ParityError, nil
		} else {
			binding.ManifestDigest = attachment.SHA256
		}
	}
	check.Binding = binding
}

// checkParity records parity for a reservation. It never returns an error:
// in audit mode nothing it finds may stop or delay a run beyond its timeout.
// A rollback is exempt, as for the other gates.
func (s *Service) checkParity(ctx context.Context, rollbackOf string, p *plan.Plan) ParityCheck {
	switch {
	case s.parity == nil:
		return ParityCheck{Outcome: ParityNotConfigured}
	case NoDatabase(p):
		return ParityCheck{Outcome: ParityNoDatabase}
	case rollbackOf != "":
		return ParityCheck{Outcome: ParityRollbackExempt}
	}
	r, err := s.parity.Check(ctx, p)
	switch {
	case errors.Is(err, parity.ErrNotConfigured):
		return ParityCheck{Outcome: ParityNoApp}
	case err != nil:
		return ParityCheck{Outcome: ParityError}
	}
	return ParityCheck{Outcome: ParityRecorded, Report: &r}
}
