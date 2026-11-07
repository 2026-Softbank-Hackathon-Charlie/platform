package cloudpolicy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/preflight/parity"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/preflight/probes"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/iam/types"
)

type simulator struct {
	out   *iam.SimulatePrincipalPolicyOutput
	calls int
	t     *testing.T
}

func (s *simulator) SimulatePrincipalPolicy(_ context.Context, in *iam.SimulatePrincipalPolicyInput, _ ...func(*iam.Options)) (*iam.SimulatePrincipalPolicyOutput, error) {
	s.calls++
	if aws.ToString(in.PolicySourceArn) != "arn:aws:iam::123456789012:role/shop" || len(in.ActionNames) != 1 || in.ActionNames[0] != "s3:GetObject" || len(in.ResourceArns) != 1 || in.ResourceArns[0] != "arn:aws:s3:::shop/probe" {
		s.t.Fatal("wrong simulated scope")
	}
	return s.out, nil
}

func TestAWSResourceScopeAndMissingContext(t *testing.T) {
	rule := probes.CloudRule{Permission: "s3:GetObject", Resource: "arn:aws:s3:::shop/probe"}
	for _, tc := range []struct {
		name     string
		decision types.PolicyEvaluationDecisionType
		resource string
		missing  bool
		known    bool
		allowed  bool
	}{
		{"allow", types.PolicyEvaluationDecisionTypeAllowed, rule.Resource, false, true, true},
		{"deny", types.PolicyEvaluationDecisionTypeExplicitDeny, rule.Resource, false, true, false},
		{"different resource", types.PolicyEvaluationDecisionTypeAllowed, "arn:aws:s3:::other/probe", false, false, false},
		{"missing condition", types.PolicyEvaluationDecisionTypeImplicitDeny, rule.Resource, true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			specific := types.ResourceSpecificResult{EvalResourceName: aws.String(tc.resource), EvalResourceDecision: tc.decision}
			if tc.missing {
				specific.MissingContextValues = []string{"aws:PrincipalTag/team"}
			}
			client := &simulator{t: t, out: &iam.SimulatePrincipalPolicyOutput{EvaluationResults: []types.EvaluationResult{{EvalActionName: aws.String(rule.Permission), EvalDecision: tc.decision, ResourceSpecificResults: []types.ResourceSpecificResult{specific}}}}}
			d, err := simulate(context.Background(), client, "arn:aws:iam::123456789012:role/shop", rule)
			if err != nil || (d.Allowed != nil) != tc.known || d.Complete {
				t.Fatalf("%+v %v", d, err)
			}
			if tc.known && *d.Allowed != tc.allowed {
				t.Fatal("wrong decision")
			}
		})
	}
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestGCPUsesOverallAccessAndBindsTuple(t *testing.T) {
	principal := "shop@project.iam.gserviceaccount.com"
	rule := probes.CloudRule{Permission: "storage.objects.get", Resource: "//storage.googleapis.com/projects/_/buckets/shop"}
	state, wrong := "CAN_ACCESS", false
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Fatal("not a policy query")
		}
		var req struct {
			Tuple map[string]string `json:"accessTuple"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		if req.Tuple["principal"] != principal || req.Tuple["permission"] != rule.Permission || req.Tuple["fullResourceName"] != rule.Resource {
			t.Fatal("wrong tuple")
		}
		if wrong {
			req.Tuple["principal"] = "other@project.iam.gserviceaccount.com"
		}
		json.NewEncoder(w).Encode(map[string]any{"overallAccessState": state, "accessTuple": req.Tuple, "allowPolicyExplanation": map[string]string{"allowAccessState": "ALLOW_ACCESS_STATE_GRANTED"}})
	}))
	defer s.Close()
	client := s.Client()
	base := client.Transport
	client.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != troubleshooterURL {
			t.Fatal("arbitrary endpoint")
		}
		local, _ := http.NewRequest(r.Method, s.URL, r.Body)
		local.Header = r.Header
		return base.RoundTrip(local.WithContext(r.Context()))
	})
	for _, tc := range []struct {
		state                 string
		wrong, known, allowed bool
	}{
		{"CAN_ACCESS", false, true, true}, {"CANNOT_ACCESS", false, true, false}, {"UNKNOWN_CONDITIONAL", false, false, false}, {"UNKNOWN_INFO", false, false, false}, {"CAN_ACCESS", true, false, false},
	} {
		state, wrong = tc.state, tc.wrong
		d, err := troubleshoot(context.Background(), client, principal, rule)
		if err != nil || (d.Allowed != nil) != tc.known {
			t.Fatalf("%s %+v %v", state, d, err)
		}
		if tc.known && (*d.Allowed != tc.allowed || !d.Complete) {
			t.Fatal("incorrect effective access")
		}
	}
}

func TestRegisteredProviderMismatchDoesNotCallAWS(t *testing.T) {
	yes, no := true, false
	cfg := probes.CloudIAM{Provider: "aws", Principal: "arn:aws:iam::123456789012:role/shop", Checks: []probes.CloudRule{{ID: "read", Permission: "s3:GetObject", Resource: "arn:aws:s3:::shop/probe", Allowed: &yes}, {ID: "deny", Permission: "iam:CreateUser", Resource: "*", Allowed: &no}}}
	client := &simulator{t: t}
	r := New(func(string) (Environment, error) { return Environment{Provider: "gcp"}, nil }, client, nil)
	if _, err := r.Check(context.Background(), parity.Target{Environment: "gke", Namespace: "shop", Application: "shop"}, cfg, cfg.Checks[0]); err == nil || client.calls != 0 {
		t.Fatal("cross-provider execution")
	}
}

func TestGCPDoesNotFollowRedirects(t *testing.T) {
	redirectCalls := 0
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirectCalls++ }))
	defer other.Close()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, other.URL, http.StatusFound) }))
	defer s.Close()
	c := s.Client()
	base := c.Transport
	c.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
		local, _ := http.NewRequest(r.Method, s.URL, r.Body)
		return base.RoundTrip(local.WithContext(r.Context()))
	})
	d, err := troubleshoot(context.Background(), c, "shop@project.iam.gserviceaccount.com", probes.CloudRule{Permission: "storage.objects.get", Resource: "//storage.googleapis.com/projects/_/buckets/shop"})
	if err == nil || d.Allowed != nil || redirectCalls != 0 {
		t.Fatal("redirected policy query")
	}
}
