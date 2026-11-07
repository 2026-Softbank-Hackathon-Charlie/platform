package probes

import (
	"context"
	"regexp"
	"strings"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/preflight/parity"
)

type CloudIAM struct {
	Provider  string      `json:"provider"`
	Principal string      `json:"principal"`
	Checks    []CloudRule `json:"checks"`
}
type CloudRule struct {
	ID         string `json:"id"`
	Permission string `json:"permission"`
	Resource   string `json:"resource"`
	Allowed    *bool  `json:"allowed"`
}
type CloudDecision struct {
	Allowed *bool
	// Complete means this provider evaluated the configured tuple's effective
	// access, not that all of the principal's permissions have been inspected.
	Complete bool
}
type CloudFetch func(context.Context, parity.Target, CloudIAM, CloudRule) (CloudDecision, error)

var awsPrincipal = regexp.MustCompile(`^arn:(aws|aws-cn|aws-us-gov):iam::[0-9]{12}:(user|role)/[a-zA-Z0-9+=,.@_/-]+$`)
var gcpPrincipal = regexp.MustCompile(`^[a-zA-Z0-9._-]+@[a-zA-Z0-9.-]+\.iam\.gserviceaccount\.com$`)
var awsPermission = regexp.MustCompile(`^[a-zA-Z0-9-]+:[a-zA-Z0-9]+$`)
var gcpPermission = regexp.MustCompile(`^[a-zA-Z0-9]+\.[a-zA-Z0-9.]+$`)

func (c CloudIAM) Validate() error {
	if len(c.Checks) < 2 || len(c.Checks) > 16 {
		return ErrInvalidConfig
	}
	if c.Provider == "aws" {
		if !awsPrincipal.MatchString(c.Principal) {
			return ErrInvalidConfig
		}
	} else if c.Provider == "gcp" {
		if !gcpPrincipal.MatchString(c.Principal) {
			return ErrInvalidConfig
		}
	} else {
		return ErrInvalidConfig
	}
	ids := map[string]bool{}
	allow, deny := false, false
	for _, r := range c.Checks {
		if !simpleID.MatchString(r.ID) || ids[r.ID] || r.Allowed == nil || len(r.Resource) > 2048 || strings.ContainsAny(r.Resource, "\r\n\t ?#") {
			return ErrInvalidConfig
		}
		if c.Provider == "aws" {
			if !awsPermission.MatchString(r.Permission) || (r.Resource != "*" && !strings.HasPrefix(r.Resource, "arn:")) {
				return ErrInvalidConfig
			}
		} else {
			if !gcpPermission.MatchString(r.Permission) || !strings.HasPrefix(r.Resource, "//") || strings.Contains(r.Resource, "*") {
				return ErrInvalidConfig
			}
		}
		ids[r.ID] = true
		allow = allow || *r.Allowed
		deny = deny || !*r.Allowed
	}
	if !allow || !deny {
		return ErrInvalidConfig
	}
	return nil
}

// WithCloudIAM composes Kubernetes and cloud policy evidence. Missing RBAC,
// unavailable policy data or partial simulation may not become identity pass.
func WithCloudIAM(target parity.Target, rbac parity.Probe, cfg CloudIAM, fetch CloudFetch) parity.Probe {
	return cloudIdentity{target: target, rbac: rbac, cfg: cfg, fetch: fetch}
}

func MissingCloudIAM(target parity.Target, rbac parity.Probe) parity.Probe {
	return cloudIdentity{target: target, rbac: rbac, missing: true}
}

type cloudIdentity struct {
	target  parity.Target
	rbac    parity.Probe
	cfg     CloudIAM
	fetch   CloudFetch
	missing bool
}

func (p cloudIdentity) Check(ctx context.Context, target parity.Target) (parity.Result, error) {
	r := parity.Result{Capability: parity.Identity, Target: target, Status: parity.Pass, Code: "configured_identity_checks_verified", ObservedAt: time.Now().UTC(), Checks: []parity.Check{}}
	if target != p.target {
		return r, ErrWrongTarget
	}
	if err := p.cfg.Validate(); err != nil && !p.missing {
		return r, err
	}
	if p.rbac != nil {
		b, err := p.rbac.Check(ctx, target)
		if err != nil || b.Target != target || b.Capability != parity.Identity {
			r.Checks = append(r.Checks, parity.Check{ID: "rbac", Status: parity.Unknown, Code: "rbac_unavailable"})
		} else {
			for _, c := range b.Checks {
				c.ID = "rbac-" + c.ID
				r.Checks = append(r.Checks, c)
			}
			if b.Status != parity.Pass {
				r.Checks = append(r.Checks, parity.Check{ID: "rbac-summary", Status: b.Status, Code: "rbac_incomplete"})
			}
		}
	} else {
		r.Checks = append(r.Checks, parity.Check{ID: "rbac", Status: parity.Unknown, Code: "rbac_not_configured"})
	}
	partial := false
	if p.missing {
		r.Checks = append(r.Checks, parity.Check{ID: "cloud-policy", Status: parity.Unknown, Code: "cloud_policy_not_configured"})
	}
	for _, q := range p.cfg.Checks {
		c := parity.Check{ID: "cloud-" + q.ID, Status: parity.Unknown, Code: "cloud_policy_unavailable"}
		if p.fetch != nil && ctx.Err() == nil {
			d, err := p.fetch(ctx, target, p.cfg, q)
			if err == nil && ctx.Err() == nil && d.Allowed != nil {
				c.Status, c.Code = parity.Pass, "cloud_policy_matches"
				if *d.Allowed != *q.Allowed {
					c.Status, c.Code = parity.Fail, "cloud_policy_mismatch"
				}
				if !d.Complete {
					partial = true
					c.Code = "cloud_identity_policy_simulation_matches"
					if c.Status == parity.Fail {
						c.Code = "cloud_identity_policy_simulation_mismatch"
					}
				}
			}
		}
		r.Checks = append(r.Checks, c)
	}
	if partial {
		r.Checks = append(r.Checks, parity.Check{ID: "cloud-policy-coverage", Status: parity.Unknown, Code: "resource_and_session_policies_unverified"})
	}
	for _, c := range r.Checks {
		if c.Status == parity.Fail {
			r.Status, r.Code = parity.Fail, "identity_policy_mismatch"
		} else if c.Status != parity.Pass && r.Status != parity.Fail {
			r.Status, r.Code = parity.Unknown, "identity_policy_incomplete"
		}
	}
	return r, nil
}
