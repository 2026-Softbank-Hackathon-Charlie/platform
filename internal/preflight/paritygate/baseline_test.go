package paritygate

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestBaselineFreshnessAndConfigOwnership(t *testing.T) {
	cfg, err := ParseConfig([]byte(strings.Replace(config, "PROM", "http://localhost", 1)), []string{"gke", "eks"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	cfg.Apps[0].Baselines = map[string]Baseline{"gke": {Revision: "review-1", ReviewedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour)}}
	g := New(cfg, nil, nil)
	// Mutating a caller-owned map cannot replace a reviewed source baseline.
	cfg.Apps[0].Baselines["gke"] = Baseline{}
	r, err := g.Check(context.Background(), shopPlan())
	if err != nil || r.BaselineStatus != "operator_reviewed" || len(r.ConfigDigest) != 64 || r.ExpiresAt.IsZero() {
		t.Fatalf("%+v %v", r, err)
	}
	expired := New(cfg, nil, nil)
	r, err = expired.Check(context.Background(), shopPlan())
	if err != nil || r.Passed || r.BaselineStatus != "expired_or_future" || r.ConfigDigest == g.digest {
		t.Fatalf("%+v %v", r, err)
	}
	cfg.Apps[0].Baselines = nil
	r, err = New(cfg, nil, nil).Check(context.Background(), shopPlan())
	if err != nil || r.Passed || r.BaselineStatus != "not_configured" {
		t.Fatalf("%+v %v", r, err)
	}
	p := shopPlan()
	p.Source.Namespace = "another-app"
	r, err = g.Check(context.Background(), p)
	if err != nil || r.Passed || r.BaselineStatus != "source_scope_mismatch" {
		t.Fatalf("%+v %v", r, err)
	}
}

func TestBaselineConfigRejectsUnboundedOrUnregisteredReview(t *testing.T) {
	for _, b := range []Baseline{
		{},
		{Revision: "review-1", ReviewedAt: time.Now(), ExpiresAt: time.Now().Add(31 * 24 * time.Hour)},
	} {
		for _, env := range []string{"gke", "unregistered"} {
			c, err := ParseConfig([]byte(strings.Replace(config, "PROM", "http://localhost", 1)), []string{"gke", "eks"})
			if err != nil {
				t.Fatal(err)
			}
			c.Apps[0].Baselines = map[string]Baseline{env: b}
			if err := c.validate([]string{"gke", "eks"}); err == nil {
				t.Fatal("invalid baseline accepted")
			}
		}
	}
}
