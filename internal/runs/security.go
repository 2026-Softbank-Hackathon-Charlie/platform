package runs

import (
	"context"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/engine"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

// SecurityAssessor reads a fresh HC-POL-02 observation for the bound endpoint.
type SecurityAssessor func(context.Context, apiv1.PlanEndpoint) apiv1.RunSecurityAssessment

// Cutover runs with writes held. Bound the read and the wait for the writer
// together so an unavailable evidence source cannot stall the migration.
const migrationSecurityBudget = 2 * time.Second

func (r *Runner) recordMigrationSecurity(ctx context.Context, l *live, step string) bool {
	if l.securityAssessor == nil || ctx.Err() != nil {
		return false
	}
	source := step == engine.StepPrecheck
	if !source && step != engine.StepCutover {
		return false
	}
	if rec := l.eng.State().Steps[step]; rec != nil && (rec.StartedAt != nil || rec.Status != engine.StatusPending) {
		return false
	}
	l.mu.Lock()
	alreadyAttempted := l.doc.Engine.Steps[step].StartedAt != nil
	endpoint := l.doc.Target
	if source {
		endpoint = l.doc.Source
	}
	l.mu.Unlock()
	if alreadyAttempted {
		return false
	}

	captureCtx, cancel := context.WithTimeout(ctx, migrationSecurityBudget)
	defer cancel()
	a := l.securityAssessor(captureCtx, endpoint)
	a.AssessedAt = r.now()
	l.mu.Lock()
	if l.doc.SecurityComparison == nil {
		l.doc.SecurityComparison = &apiv1.RunSecurityComparison{ControlId: "HC-POL-02"}
	}
	if source {
		l.doc.SecurityComparison.Source = &a
	} else {
		l.doc.SecurityComparison.Target = &a
	}
	l.mu.Unlock()

	for attempt := range 2 {
		if attempt > 0 && captureCtx.Err() != nil {
			break
		}
		result := make(chan error, 1)
		l.q.push(item{doc: true, persistResult: result})
		select {
		case err := <-result:
			if err == nil {
				return true
			}
		case <-captureCtx.Done():
			return true
		}
	}
	return true
}
