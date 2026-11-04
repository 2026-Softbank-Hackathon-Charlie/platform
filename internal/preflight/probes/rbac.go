// Package probes contains opt-in, read-only target checks. paritygate runs them
// on a run's start for the audit log only; nothing here stops a run. Clients,
// endpoints and requirements must be server-owned and bound to the intended
// environment by the caller, never supplied by AI.
package probes

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/preflight/parity"
	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	authorizationclient "k8s.io/client-go/kubernetes/typed/authorization/v1"
)

var ErrInvalidConfig = errors.New("invalid parity probe configuration")
var ErrWrongTarget = errors.New("parity probe target mismatch")

var simpleID = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// AccessRule checks one concrete Kubernetes resource operation. Both positive
// and negative requirements are mandatory; this does not prove AWS IAM access.
// Creating a SubjectAccessReview evaluates authorization without performing the
// operation itself. The probe's own identity needs permission to create SARs.
type AccessRule struct {
	ID          string
	Attributes  authorizationv1.ResourceAttributes
	WantAllowed bool
}

type RBAC struct {
	target  parity.Target
	client  authorizationclient.SubjectAccessReviewInterface
	account string
	rules   []AccessRule
}

func NewRBAC(target parity.Target, client authorizationclient.SubjectAccessReviewInterface, account string, rules []AccessRule) (*RBAC, error) {
	if !parity.ValidTarget(target) || client == nil || !simpleID.MatchString(account) || len(rules) == 0 {
		return nil, ErrInvalidConfig
	}
	p := &RBAC{target: target, client: client, account: account}
	seen := map[string]bool{}
	positive, negative := false, false
	for _, rule := range rules {
		if !simpleID.MatchString(rule.ID) || seen[rule.ID] || rule.Attributes.Verb == "" || rule.Attributes.Verb == "*" || rule.Attributes.Resource == "" || rule.Attributes.Resource == "*" {
			return nil, ErrInvalidConfig
		}
		seen[rule.ID] = true
		positive = positive || rule.WantAllowed
		negative = negative || !rule.WantAllowed
		rule.Attributes = *rule.Attributes.DeepCopy()
		p.rules = append(p.rules, rule)
	}
	if !positive || !negative {
		return nil, ErrInvalidConfig
	}
	return p, nil
}

func (p *RBAC) Check(ctx context.Context, target parity.Target) (parity.Result, error) {
	r := parity.Result{Capability: parity.Identity, Target: target, Status: parity.Unknown, Code: "not_ready", ObservedAt: time.Now().UTC(), Checks: []parity.Check{}}
	if p == nil || p.client == nil {
		return r, ErrInvalidConfig
	}
	if target != p.target {
		return r, ErrWrongTarget
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	r.Status, r.Code = parity.Pass, "rbac_verified"
	for _, rule := range p.rules {
		check := parity.Check{ID: rule.ID, Status: parity.Unknown, Code: "authorization_unavailable"}
		response, err := p.client.Create(ctx, &authorizationv1.SubjectAccessReview{
			Spec: authorizationv1.SubjectAccessReviewSpec{
				User:               "system:serviceaccount:" + target.Namespace + ":" + p.account,
				Groups:             []string{"system:serviceaccounts", "system:serviceaccounts:" + target.Namespace, "system:authenticated"},
				ResourceAttributes: rule.Attributes.DeepCopy(),
			},
		}, metav1.CreateOptions{})
		switch {
		case ctx.Err() != nil:
			check.Code = "authorization_cancelled"
		case err != nil || response == nil:
			// Do not include SDK errors or response reason: either may contain secrets.
		case response.Status.EvaluationError != "" || response.Status.Allowed && response.Status.Denied:
			check.Code = "authorization_indeterminate"
		default:
			check.Evidence = fmt.Sprintf("SubjectAccessReview allowed=%t expected=%t", response.Status.Allowed, rule.WantAllowed)
			if response.Status.Allowed == rule.WantAllowed {
				check.Status, check.Code = parity.Pass, "authorization_matches"
			} else {
				check.Status, check.Code = parity.Fail, "authorization_mismatch"
			}
		}
		r.Checks = append(r.Checks, check)
		if check.Status == parity.Fail {
			r.Status, r.Code = parity.Fail, "rbac_mismatch"
		}
		if check.Status == parity.Unknown && r.Status != parity.Fail {
			r.Status, r.Code = parity.Unknown, "rbac_incomplete"
		}
		if ctx.Err() != nil {
			break
		}
	}
	return r, nil
}
