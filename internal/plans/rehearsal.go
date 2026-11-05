package plans

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

// RehearsalReportName is the attachment the AI runner (ai-sandbox hopai)
// uploads after rehearsing a plan in a sandbox.
//
// Trust boundary (platform #24): the gate checks that a passing report exists
// for exactly this plan body and target.yaml, and that the principal that
// submitted the plan uploaded it. It does not prove the rehearsal really ran:
// the hashes can be computed from the plan itself, so whoever holds the
// runner's agent token can write a report by hand. The gate relies on the
// runner being trusted, like the engine relies on the platform. Two setups
// skip the gate because no caller is an agent: an Insecure (dev) server,
// where every caller is an operator, and a runner given an operator token.
const RehearsalReportName = "rehearsal-report.json"

// RehearsalReportSchema is the report format this server reads
// (ai-sandbox rehearsal.ReportSchema).
const RehearsalReportSchema = "hopcharlie.rehearsal/v1"

// ErrRehearsal means an AI plan cannot be approved because it has no
// rehearsal report that passed for the plan as it is now (platform #24).
var ErrRehearsal = errors.New("rehearsal evidence required")

// rehearsalReport is the part of the report the gate reads. Candidate is
// what the last attempt ran: the SHA-256 of the plan body as submitted and of
// the target.yaml attachment.
type rehearsalReport struct {
	SchemaVersion string `json:"schemaVersion"`
	Passed        bool   `json:"passed"`
	Candidate     struct {
		PlanSHA256     string `json:"planSha256"`
		ManifestSHA256 string `json:"manifestSha256"`
	} `json:"candidate"`
}

// needsRehearsal reports whether approving the plan needs rehearsal
// evidence: plans an agent submitted (AI plans), and plans from before the
// role was kept that carry a rehearsal report anyway. Plans a human wrote
// (operator, or a console draft) keep the plain approval flow, as do
// rollback plans, which the server builds from a run that already happened.
func needsRehearsal(row store.Plan, attached []store.PlanAttachment) bool {
	if row.RollbackOf != "" {
		return false
	}
	switch row.SubmittedRole {
	case string(apiv1.RoleAgent):
		return true
	case "":
		for _, a := range attached {
			if a.Name == RehearsalReportName {
				return true
			}
		}
	}
	return false
}

// rehearsalCheck says why the plan's rehearsal evidence is not enough, or ""
// when the latest report passed for exactly this plan body and target.yaml.
// The report cannot name the revision it belongs to (uploading it raises the
// revision), so it is bound to the content by hash instead: any later change
// to the body is impossible, and any later target.yaml upload changes the
// hash and needs a new rehearsal.
func (s *Service) rehearsalCheck(ctx context.Context, row store.Plan, attached []store.PlanAttachment) (string, error) {
	var report *store.PlanAttachment
	manifest := ""
	name, usesManifest := manifestName(row)
	for i := range attached {
		switch attached[i].Name {
		case RehearsalReportName:
			report = &attached[i]
		case name:
			if usesManifest {
				manifest = attached[i].SHA256
			}
		}
	}
	if report == nil {
		return "no rehearsal report: run the AI runner (hopai run) so it rehearses this plan in a sandbox and uploads " + RehearsalReportName, nil
	}
	if row.SubmittedBy != "" && report.CreatedBy != row.SubmittedBy {
		// Only the runner that submitted the plan reports on it; another
		// principal (another agent, an operator) cannot supply the evidence.
		return fmt.Sprintf("the rehearsal report was uploaded by %q, not by %q who submitted the plan", report.CreatedBy, row.SubmittedBy), nil
	}
	full, err := s.store.GetPlanAttachment(ctx, row.Ref, RehearsalReportName)
	if err != nil {
		return "", err
	}
	var rep rehearsalReport
	if err := json.Unmarshal(full.Content, &rep); err != nil {
		return "the rehearsal report is not valid JSON", nil
	}
	if rep.SchemaVersion != RehearsalReportSchema {
		return fmt.Sprintf("the rehearsal report has schemaVersion %q, want %q", rep.SchemaVersion, RehearsalReportSchema), nil
	}
	if !rep.Passed {
		return "the rehearsal did not pass: fix the plan and rehearse it again", nil
	}
	sum := sha256.Sum256(row.Raw)
	if rep.Candidate.PlanSHA256 != hex.EncodeToString(sum[:]) {
		return "the rehearsal report is for a different plan body", nil
	}
	if usesManifest && rep.Candidate.ManifestSHA256 != manifest {
		return "the rehearsal report is for a different " + name + " than the one attached now: rehearse again", nil
	}
	if !usesManifest && rep.Candidate.ManifestSHA256 != "" {
		return "the rehearsal report names a target manifest, but this plan does not use one", nil
	}
	return "", nil
}

// requireRehearsal fails when an AI plan has no passing rehearsal report for
// its current content. Other plans pass.
func (s *Service) requireRehearsal(ctx context.Context, row store.Plan, attached []store.PlanAttachment) error {
	if row.Status != store.PlanPending || !needsRehearsal(row, attached) {
		return nil
	}
	why, err := s.rehearsalCheck(ctx, row, attached)
	if err != nil || why == "" {
		return err
	}
	return fmt.Errorf("%w: plan %s was submitted by the AI runner and %s", ErrRehearsal, row.Ref, why)
}

// rehearsalGate is what the console shows next to the approve button.
func (s *Service) rehearsalGate(ctx context.Context, row store.Plan, attached []store.PlanAttachment) (*apiv1.PlanRehearsalGate, error) {
	if !needsRehearsal(row, attached) {
		return &apiv1.PlanRehearsalGate{Required: false, Passed: false}, nil
	}
	why, err := s.rehearsalCheck(ctx, row, attached)
	if err != nil {
		return nil, err
	}
	g := &apiv1.PlanRehearsalGate{Required: true, Passed: why == ""}
	if why != "" {
		g.Reason = &why
	}
	return g, nil
}

// manifestName is the attachment target.manifest refers to, if any.
func manifestName(row store.Plan) (string, bool) {
	var refs struct {
		Target struct {
			Manifest string `json:"manifest"`
		} `json:"target"`
	}
	if err := json.Unmarshal(row.Raw, &refs); err != nil {
		return "", false
	}
	return ManifestAttachment(&plan.Plan{Target: plan.Endpoint{Manifest: refs.Target.Manifest}})
}
