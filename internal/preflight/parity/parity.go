// Package parity compares trusted source requirements with actual target probe
// results. It neither discovers live source dependencies nor changes a cluster.
// Missing collectors and incomplete discovery must never become a passing check.
package parity

import (
	"context"
	"errors"
	"regexp"
	"time"
)

type Capability string

const (
	Identity      Capability = "identity"
	Secret        Capability = "secret"
	Audit         Capability = "audit"
	Observability Capability = "metrics-alerting"
	BackupRestore Capability = "backup-restore"
)

var capabilities = [...]Capability{Identity, Secret, Audit, Observability, BackupRestore}

type Status string

const (
	Pass          Status = "pass"
	Fail          Status = "fail"
	Unknown       Status = "unknown"
	NotApplicable Status = "not_applicable"
)

type Target struct {
	Environment string `json:"environment"`
	Namespace   string `json:"namespace"`
	Application string `json:"application"`
}

// Baseline must come from trusted source discovery. Complete is not proof of
// discovery or freshness. Do not use AI-submitted capabilities as a baseline.
// Discovery failures must set Complete=false, not silently remove capabilities.
type Baseline struct {
	Target       Target       `json:"target"`
	Complete     bool         `json:"complete"`
	Capabilities []Capability `json:"capabilities"`
}

type Request struct {
	Source    Baseline     `json:"source"`
	Target    Target       `json:"target"`
	Mandatory []Capability `json:"mandatory"` // organization minimum, unioned with source usage
}

// Check contains a bounded probe summary, never credentials or response bodies.
// Successful checks retain evidence even if another part is not implemented.
type Check struct {
	ID       string `json:"id"`
	Status   Status `json:"status"`
	Code     string `json:"code"`
	Evidence string `json:"evidence,omitempty"`
}

type Result struct {
	Capability Capability `json:"capability"`
	Target     Target     `json:"target"`
	Status     Status     `json:"status"`
	Code       string     `json:"code"`
	ObservedAt time.Time  `json:"observedAt,omitempty"`
	Checks     []Check    `json:"checks"`
}

type Report struct {
	Passed         bool      `json:"passed"`
	Results        []Result  `json:"results"`
	ConfigDigest   string    `json:"configDigest,omitempty"`
	BaselineStatus string    `json:"baselineStatus,omitempty"`
	ObservedAt     time.Time `json:"observedAt,omitempty"`
	ExpiresAt      time.Time `json:"expiresAt,omitempty"`
}

// Probe implementations are trusted code with server-owned clients/config.
// This is not an API for accepting externally supplied verification reports.
type Probe interface {
	Check(context.Context, Target) (Result, error)
}

var ErrInvalidRequest = errors.New("invalid parity request")

// ErrNotConfigured: the server has no parity expectations for the target
// (for example, no app entry for its namespace). Nothing was evaluated.
var ErrNotConfigured = errors.New("no parity configuration for the target")

var identifier = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

func ValidTarget(t Target) bool {
	return identifier.MatchString(t.Environment) && identifier.MatchString(t.Namespace) && identifier.MatchString(t.Application)
}

func known(c Capability) bool {
	for _, supported := range capabilities {
		if c == supported {
			return true
		}
	}
	return false
}

// Evaluate requires the union of source-used and organizational capabilities.
// Required missing/error/unknown/not-applicable probes deny. Results include all
// five categories, but non-required categories are explicitly not_applicable.
// The caller must not mutate request, probe configuration or the registry while
// it is in use. Each probe has a bounded execution context.
func Evaluate(ctx context.Context, req Request, probes map[Capability]Probe) (Report, error) {
	report := Report{Results: []Result{}, ObservedAt: time.Now().UTC()}
	report.ExpiresAt = report.ObservedAt.Add(5 * time.Minute)
	if !ValidTarget(req.Source.Target) || !ValidTarget(req.Target) ||
		req.Source.Target.Environment == req.Target.Environment || req.Source.Target.Application != req.Target.Application {
		return report, ErrInvalidRequest
	}
	required := map[Capability]bool{}
	for _, list := range [][]Capability{req.Source.Capabilities, req.Mandatory} {
		for _, c := range list {
			if !known(c) {
				return report, ErrInvalidRequest
			}
			required[c] = true
		}
	}
	if len(required) == 0 && req.Source.Complete {
		return report, ErrInvalidRequest
	}
	report.Passed = req.Source.Complete
	for _, c := range capabilities {
		r := Result{Capability: c, Target: req.Target, Status: Unknown, Checks: []Check{}}
		switch {
		case !req.Source.Complete:
			r.Code = "source_discovery_incomplete"
		case !required[c]:
			r.Status, r.Code = NotApplicable, "not_required"
		case ctx.Err() != nil:
			r.Code = "evaluation_cancelled"
		case probes[c] == nil:
			r.Code = "probe_not_configured"
		default:
			pctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			got, err := probes[c].Check(pctx, req.Target)
			expired := pctx.Err() != nil
			cancel()
			if err != nil || expired {
				r.Code = "probe_failed" // never expose a raw provider error
			} else if !validResult(got, c, req.Target) {
				r.Code = "invalid_probe_result"
			} else if got.ObservedAt.After(time.Now().Add(5*time.Second)) || time.Since(got.ObservedAt) > 5*time.Minute {
				r.Code = "stale_probe_result"
			} else {
				r = got
				if expiry := got.ObservedAt.Add(5 * time.Minute); expiry.Before(report.ExpiresAt) {
					report.ExpiresAt = expiry
				}
			}
		}
		if r.Status != Pass && r.Status != NotApplicable || required[c] && r.Status != Pass {
			report.Passed = false
		}
		report.Results = append(report.Results, r)
	}
	if ctx.Err() != nil {
		report.Passed = false
		return report, ctx.Err()
	}
	if !time.Now().Before(report.ExpiresAt) {
		report.Passed = false
	}
	return report, nil
}

func validResult(r Result, c Capability, target Target) bool {
	if r.Capability != c || r.Target != target || r.Code == "" || r.ObservedAt.IsZero() {
		return false
	}
	if r.Status != Pass && r.Status != Fail && r.Status != Unknown {
		return false
	}
	if r.Status == Pass && len(r.Checks) == 0 {
		return false
	}
	for _, check := range r.Checks {
		if check.ID == "" || check.Code == "" || (check.Status != Pass && check.Status != Fail && check.Status != Unknown) {
			return false
		}
		if r.Status == Pass && check.Status != Pass {
			return false
		}
	}
	return true
}
