package parity

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

type probeFunc func(context.Context, Target) (Result, error)

func (f probeFunc) Check(ctx context.Context, t Target) (Result, error) { return f(ctx, t) }

func request() Request {
	return Request{
		Source: Baseline{Target: Target{"aws", "shop", "shop"}, Complete: true, Capabilities: []Capability{Identity}},
		Target: Target{"onprem", "shop", "shop"},
	}
}

func passing(c Capability) Probe {
	return probeFunc(func(ctx context.Context, target Target) (Result, error) {
		return Result{Capability: c, Target: target, Status: Pass, Code: "verified", ObservedAt: time.Now(), Checks: []Check{{ID: "actual-check", Status: Pass, Code: "matches", Evidence: "test fixture only"}}}, nil
	})
}

func TestRequirementUnionAndNonRequired(t *testing.T) {
	r := request()
	r.Mandatory = []Capability{Audit, Identity, Audit}
	calls := 0
	p := probeFunc(func(ctx context.Context, target Target) (Result, error) {
		calls++
		return passing(Identity).Check(ctx, target)
	})
	v, err := Evaluate(context.Background(), r, map[Capability]Probe{Identity: p})
	if err != nil || v.Passed || calls != 1 || len(v.Results) != 5 {
		t.Fatalf("%+v calls=%d err=%v", v, calls, err)
	}
	if v.Results[1].Status != NotApplicable || v.Results[2].Code != "probe_not_configured" {
		t.Fatalf("%+v", v.Results)
	}
	v, err = Evaluate(context.Background(), r, map[Capability]Probe{Identity: p, Audit: passing(Audit)})
	if err != nil || !v.Passed {
		t.Fatalf("%+v %v", v, err)
	}
}

func TestIncompleteDiscoveryDoesNotRunProbes(t *testing.T) {
	r := request()
	r.Source.Complete = false
	p := probeFunc(func(context.Context, Target) (Result, error) {
		t.Fatal("probe ran after failed discovery")
		return Result{}, nil
	})
	v, err := Evaluate(context.Background(), r, map[Capability]Probe{Identity: p})
	if err != nil || v.Passed {
		t.Fatalf("%+v %v", v, err)
	}
	for _, result := range v.Results {
		if result.Status != Unknown || result.Code != "source_discovery_incomplete" {
			t.Fatalf("%+v", result)
		}
	}
}

func TestRequiredResultsFailClosed(t *testing.T) {
	for name, change := range map[string]func(*Result){
		"unknown":              func(r *Result) { r.Status = Unknown },
		"fail":                 func(r *Result) { r.Status = Fail },
		"not applicable":       func(r *Result) { r.Status = NotApplicable },
		"empty checks":         func(r *Result) { r.Checks = nil },
		"missing observation":  func(r *Result) { r.ObservedAt = time.Time{} },
		"wrong capability":     func(r *Result) { r.Capability = Secret },
		"wrong target":         func(r *Result) { r.Target.Environment = "tokyo" },
		"inconsistent success": func(r *Result) { r.Checks[0].Status = Unknown },
		"bad status":           func(r *Result) { r.Status = "success" },
	} {
		t.Run(name, func(t *testing.T) {
			p := probeFunc(func(ctx context.Context, target Target) (Result, error) {
				r, _ := passing(Identity).Check(ctx, target)
				change(&r)
				return r, nil
			})
			v, err := Evaluate(context.Background(), request(), map[Capability]Probe{Identity: p})
			if err != nil || v.Passed {
				t.Fatalf("%+v %v", v, err)
			}
		})
	}
}

func TestProbeErrorsAreNotExposed(t *testing.T) {
	p := probeFunc(func(context.Context, Target) (Result, error) { return Result{}, errors.New("postgres://TOPSECRET") })
	v, err := Evaluate(context.Background(), request(), map[Capability]Probe{Identity: p})
	raw, _ := json.Marshal(v)
	if err != nil || v.Passed || strings.Contains(string(raw), "TOPSECRET") || v.Results[0].Code != "probe_failed" {
		t.Fatalf("%s %v", raw, err)
	}
}

func TestInvalidRequestAndCancellation(t *testing.T) {
	for name, change := range map[string]func(*Request){
		"wrong app":          func(r *Request) { r.Target.Application = "other" },
		"same environment":   func(r *Request) { r.Target.Environment = "aws" },
		"unknown capability": func(r *Request) { r.Mandatory = []Capability{"invented"} },
		"no requirements":    func(r *Request) { r.Source.Capabilities = nil },
		"invalid target":     func(r *Request) { r.Target.Namespace = "TOPSECRET\n" },
	} {
		t.Run(name, func(t *testing.T) {
			r := request()
			change(&r)
			v, err := Evaluate(context.Background(), r, nil)
			if v.Passed || !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("%+v %v", v, err)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	v, err := Evaluate(ctx, request(), map[Capability]Probe{Identity: passing(Identity)})
	if v.Passed || !errors.Is(err, context.Canceled) {
		t.Fatalf("%+v %v", v, err)
	}
}

func TestDirectionIndependent(t *testing.T) {
	for _, direction := range [][2]string{{"aws", "onprem"}, {"onprem", "aws"}} {
		r := request()
		r.Source.Target.Environment, r.Target.Environment = direction[0], direction[1]
		v, err := Evaluate(context.Background(), r, map[Capability]Probe{Identity: passing(Identity)})
		if err != nil || !v.Passed {
			t.Fatalf("%+v %v", v, err)
		}
	}
}
