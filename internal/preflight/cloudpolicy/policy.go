// Package cloudpolicy queries provider policy evaluators without executing the
// simulated application operations or changing IAM policies.
package cloudpolicy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/preflight/parity"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/preflight/probes"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/iam/types"
	"golang.org/x/oauth2/google"
)

type Environment struct{ Provider, Region string }
type Lookup func(string) (Environment, error)
type IAM interface {
	SimulatePrincipalPolicy(context.Context, *iam.SimulatePrincipalPolicyInput, ...func(*iam.Options)) (*iam.SimulatePrincipalPolicyOutput, error)
}
type Runner struct {
	lookup Lookup
	iam    IAM
	gcp    *http.Client
}

// Nil clients use the server's AWS credentials and Google ADC. Test clients
// are injected here, never through public API or parity configuration.
func New(lookup Lookup, awsClient IAM, gcpClient *http.Client) *Runner {
	return &Runner{lookup, awsClient, gcpClient}
}

func (r *Runner) Check(ctx context.Context, target parity.Target, cfg probes.CloudIAM, rule probes.CloudRule) (probes.CloudDecision, error) {
	if r == nil || r.lookup == nil || !parity.ValidTarget(target) {
		return probes.CloudDecision{}, errors.New("invalid cloud target")
	}
	if err := cfg.Validate(); err != nil {
		return probes.CloudDecision{}, err
	}
	e, err := r.lookup(target.Environment)
	if err != nil {
		return probes.CloudDecision{}, err
	}
	if e.Provider != cfg.Provider {
		return probes.CloudDecision{}, errors.New("cloud provider mismatch")
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	switch cfg.Provider {
	case "aws":
		client := r.iam
		if client == nil {
			c, err := config.LoadDefaultConfig(ctx, config.WithRegion(e.Region))
			if err != nil {
				return probes.CloudDecision{}, err
			}
			client = iam.NewFromConfig(c)
		}
		return simulate(ctx, client, cfg.Principal, rule)
	case "gcp":
		client := r.gcp
		if client == nil {
			client, err = google.DefaultClient(ctx, "https://www.googleapis.com/auth/cloud-platform")
			if err != nil {
				return probes.CloudDecision{}, err
			}
		}
		return troubleshoot(ctx, client, cfg.Principal, rule)
	}
	return probes.CloudDecision{}, errors.New("unsupported cloud provider")
}

func simulate(ctx context.Context, client IAM, principal string, rule probes.CloudRule) (probes.CloudDecision, error) {
	out, err := client.SimulatePrincipalPolicy(ctx, &iam.SimulatePrincipalPolicyInput{PolicySourceArn: aws.String(principal), ActionNames: []string{rule.Permission}, ResourceArns: []string{rule.Resource}})
	if err != nil {
		return probes.CloudDecision{}, err
	}
	if out == nil || out.IsTruncated || len(out.EvaluationResults) != 1 {
		return probes.CloudDecision{}, nil
	}
	r := out.EvaluationResults[0]
	if aws.ToString(r.EvalActionName) != rule.Permission || len(r.MissingContextValues) > 0 {
		return probes.CloudDecision{}, nil
	}
	decision := r.EvalDecision
	if len(r.ResourceSpecificResults) > 0 {
		if len(r.ResourceSpecificResults) != 1 {
			return probes.CloudDecision{}, nil
		}
		s := r.ResourceSpecificResults[0]
		if aws.ToString(s.EvalResourceName) != rule.Resource || len(s.MissingContextValues) > 0 {
			return probes.CloudDecision{}, nil
		}
		decision = s.EvalResourceDecision
	} else if rule.Resource != "*" && aws.ToString(r.EvalResourceName) != rule.Resource {
		return probes.CloudDecision{}, nil
	}
	var allowed bool
	switch decision {
	case types.PolicyEvaluationDecisionTypeAllowed:
		allowed = true
	case types.PolicyEvaluationDecisionTypeExplicitDeny, types.PolicyEvaluationDecisionTypeImplicitDeny:
	default:
		return probes.CloudDecision{}, nil
	}
	// This call does not include resource policies, session policies, context
	// or every external control. It is never a complete effective-access proof.
	return probes.CloudDecision{Allowed: &allowed, Complete: false}, nil
}

const troubleshooterURL = "https://policytroubleshooter.googleapis.com/v3/iam:troubleshoot"

func troubleshoot(ctx context.Context, client *http.Client, principal string, rule probes.CloudRule) (probes.CloudDecision, error) {
	body, _ := json.Marshal(map[string]any{"accessTuple": map[string]string{"principal": principal, "fullResourceName": rule.Resource, "permission": rule.Permission}})
	req, err := http.NewRequestWithContext(ctx, "POST", troubleshooterURL, bytes.NewReader(body))
	if err != nil {
		return probes.CloudDecision{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	// A redirect must not send Google credentials or query data to another host.
	c := *client
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := c.Do(req)
	if err != nil {
		return probes.CloudDecision{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return probes.CloudDecision{}, errors.New("cloud policy query unavailable")
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20+1))
	if err != nil || len(raw) > 1<<20 {
		return probes.CloudDecision{}, errors.New("invalid policy response")
	}
	var result struct {
		OverallAccessState string `json:"overallAccessState"`
		AccessTuple        struct {
			Principal  string `json:"principal"`
			Resource   string `json:"fullResourceName"`
			Permission string `json:"permission"`
		} `json:"accessTuple"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return probes.CloudDecision{}, errors.New("invalid policy response")
	}
	if result.AccessTuple.Principal != principal || result.AccessTuple.Resource != rule.Resource || result.AccessTuple.Permission != rule.Permission {
		return probes.CloudDecision{}, nil
	}
	var allowed bool
	switch result.OverallAccessState {
	case "CAN_ACCESS":
		allowed = true
	case "CANNOT_ACCESS":
	default:
		return probes.CloudDecision{}, nil
	}
	return probes.CloudDecision{Allowed: &allowed, Complete: true}, nil
}
