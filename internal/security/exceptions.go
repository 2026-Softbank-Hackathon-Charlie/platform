package security

import (
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

// SEC-08 (ADR 0034): an operator's time-limited exception for one finding or
// vulnerability. Reports show the active exception of each item; it never
// changes severity, and a severity change does not end it.

// ExceptionFromStore is a stored exception as the API returns it.
func ExceptionFromStore(e store.SecurityException) apiv1.SecurityException {
	out := apiv1.SecurityException{Id: e.ID, TargetType: apiv1.SecurityExceptionTarget(e.TargetType), TargetId: e.TargetID,
		Reason: e.Reason, CreatedBy: e.CreatedBy, CreatedAt: e.CreatedAt.UTC(), ExpiresAt: e.ExpiresAt.UTC()}
	if e.RevokedAt != nil {
		at, by := e.RevokedAt.UTC(), e.RevokedBy
		out.RevokedAt, out.RevokedBy = &at, &by
	}
	return out
}

// ActiveExceptions returns the exceptions that are not revoked and expire
// after now, by exceptionKey.
func ActiveExceptions(all []apiv1.SecurityException, now time.Time) map[string]apiv1.SecurityException {
	out := map[string]apiv1.SecurityException{}
	for _, e := range all {
		if e.RevokedAt == nil && e.ExpiresAt.After(now) {
			out[exceptionKey(e.TargetType, e.TargetId)] = e
		}
	}
	return out
}

// AttachFindingExceptions sets the active exception on each finding of rep.
func AttachFindingExceptions(rep *apiv1.SecurityReport, active map[string]apiv1.SecurityException) {
	for i := range rep.Findings {
		rep.Findings[i].Exception = lookupException(active, apiv1.SecurityExceptionTargetFinding, rep.Findings[i].Id)
	}
}

// AttachVulnerabilityExceptions sets the active exception on each
// vulnerability of rep.
func AttachVulnerabilityExceptions(rep *apiv1.SecurityVulnerabilityReport, active map[string]apiv1.SecurityException) {
	for i := range rep.Vulnerabilities {
		rep.Vulnerabilities[i].Exception = lookupException(active, apiv1.SecurityExceptionTargetVulnerability, rep.Vulnerabilities[i].Id)
	}
}

func exceptionKey(t apiv1.SecurityExceptionTarget, id string) string { return string(t) + "/" + id }

// lookupException returns a copy of the target's active exception, or nil.
func lookupException(active map[string]apiv1.SecurityException, t apiv1.SecurityExceptionTarget, id string) *apiv1.SecurityException {
	e, ok := active[exceptionKey(t, id)]
	if !ok {
		return nil
	}
	return &e
}
