package probes

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/preflight/parity"
)

type passedRBAC struct{}

func (passedRBAC) Check(_ context.Context, t parity.Target) (parity.Result, error) {
	return parity.Result{Capability: parity.Identity, Target: t, Status: parity.Pass, ObservedAt: time.Now(), Checks: []parity.Check{{ID: "read", Status: parity.Pass, Code: "authorization_matches"}}}, nil
}

func TestCloudPolicyMatchesFailuresUnknownAndRedaction(t *testing.T) {
	yes, no := true, false
	target := parity.Target{Environment: "eks", Namespace: "shop", Application: "shop"}
	cfg := CloudIAM{Provider: "aws", Principal: "arn:aws:iam::123456789012:role/shop", Checks: []CloudRule{{ID: "read", Permission: "s3:GetObject", Resource: "arn:aws:s3:::shop/probe", Allowed: &yes}, {ID: "no-admin", Permission: "iam:CreateUser", Resource: "*", Allowed: &no}}}
	for _, tc := range []struct {
		name                            string
		complete, mismatch, unavailable bool
		want                            parity.Status
	}{
		{"matches", true, false, false, parity.Pass}, {"partial AWS", false, false, false, parity.Unknown}, {"excess permission", true, true, false, parity.Fail}, {"unavailable", true, false, true, parity.Unknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := WithCloudIAM(target, passedRBAC{}, cfg, func(_ context.Context, got parity.Target, _ CloudIAM, q CloudRule) (CloudDecision, error) {
				if got != target {
					t.Fatal("target changed")
				}
				if tc.unavailable {
					return CloudDecision{}, errors.New("sensitive-provider-error")
				}
				allowed := *q.Allowed
				if tc.mismatch && q.ID == "no-admin" {
					allowed = true
				}
				return CloudDecision{Allowed: &allowed, Complete: tc.complete}, nil
			})
			r, err := p.Check(context.Background(), target)
			if err != nil || r.Status != tc.want {
				t.Fatalf("%+v %v", r, err)
			}
			raw, _ := json.Marshal(r)
			if strings.Contains(string(raw), "sensitive-provider-error") {
				t.Fatal("provider error leaked")
			}
		})
	}
	for _, bad := range []CloudIAM{{Provider: "aws", Principal: "arn:aws:sts::123456789012:assumed-role/shop/session", Checks: cfg.Checks}, {Provider: "aws", Principal: cfg.Principal, Checks: cfg.Checks[:1]}} {
		if bad.Validate() == nil {
			t.Fatal("unsafe/incomplete cloud policy")
		}
	}
}

func TestCloudConfigurationMissingCannotPassRBACAlone(t *testing.T) {
	target := parity.Target{Environment: "eks", Namespace: "shop", Application: "shop"}
	r, err := MissingCloudIAM(target, passedRBAC{}).Check(context.Background(), target)
	if err != nil || r.Status != parity.Unknown {
		t.Fatalf("%+v %v", r, err)
	}
	if r.Checks[len(r.Checks)-1].Code != "cloud_policy_not_configured" {
		t.Fatal("missing policy not reported")
	}
}
