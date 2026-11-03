package residency

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/preflight"
)

func fixtureRequest() preflight.Request {
	c := fixtureConfig().Catalog[0]
	return preflight.Request{Movements: []preflight.Movement{{
		ID: "move", Kind: preflight.PhysicalReplication, Source: c.Scope, Destination: "onprem",
		Inventory: preflight.Inventory{Scope: c.Scope, Complete: true, Datasets: []preflight.Dataset{c.Datasets[0].Dataset, c.Datasets[1].Dataset}},
	}}}
}

func mustEvaluator(t *testing.T, c Config) *Evaluator {
	t.Helper()
	e, err := New(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestPolicyDestinations(t *testing.T) {
	for _, tc := range []struct {
		label, destination string
		want               bool
	}{
		{"kr-only", "onprem", true}, {"kr-only", "tokyo", false},
		{"apac", "tokyo", true}, {"apac", "us", false},
		{"global", "us", true}, {"global", "unknown", false},
	} {
		t.Run(tc.label+"/"+tc.destination, func(t *testing.T) {
			c := fixtureConfig()
			c.Catalog[0].Datasets[0].Label = tc.label
			r := fixtureRequest()
			r.Movements[0].Destination = tc.destination
			v, err := mustEvaluator(t, c).Evaluate(context.Background(), r)
			if err != nil || v.Allowed != tc.want || len(v.PolicyDigest) != 64 {
				t.Fatalf("verdict=%+v err=%v", v, err)
			}
			if !tc.want && len(v.Reasons) == 0 {
				t.Fatal("denial without reasons")
			}
		})
	}
}

func TestPolicyUnknownAndMissingScope(t *testing.T) {
	e := mustEvaluator(t, fixtureConfig())
	for name, tc := range map[string]struct {
		change func(*preflight.Movement)
		code   string
	}{
		"unknown source":  {func(m *preflight.Movement) { m.Source.Environment = "unknown"; m.Inventory.Scope = m.Source }, "unknown_source"},
		"unknown cluster": {func(m *preflight.Movement) { m.Source.Cluster = "other"; m.Inventory.Scope = m.Source }, "unclassified_cluster"},
		"new dataset": {func(m *preflight.Movement) {
			m.Inventory.Datasets = append(m.Inventory.Datasets, preflight.Dataset{Database: "app", Schema: "public", Table: "new_table"})
		}, "unclassified_dataset"},
		"omit restricted dataset": {func(m *preflight.Movement) { m.Inventory.Datasets = m.Inventory.Datasets[1:]; m.Destination = "tokyo" }, "inventory_catalog_mismatch"},
		"other db restricted":     {func(m *preflight.Movement) { m.Destination = "tokyo" }, "country_not_allowed"},
	} {
		t.Run(name, func(t *testing.T) {
			r := fixtureRequest()
			tc.change(&r.Movements[0])
			v, err := e.Evaluate(context.Background(), r)
			if err != nil || v.Allowed || !slices.ContainsFunc(v.Reasons, func(x preflight.Reason) bool { return x.Code == tc.code }) {
				t.Fatalf("verdict=%+v err=%v", v, err)
			}
		})
	}
}

func TestInvalidInputsFailClosed(t *testing.T) {
	e := mustEvaluator(t, fixtureConfig())
	cases := map[string]func(*preflight.Request){
		"empty":              func(r *preflight.Request) { r.Movements = nil },
		"logical":            func(r *preflight.Request) { r.Movements[0].Kind = "logical" },
		"blank kind":         func(r *preflight.Request) { r.Movements[0].Kind = "" },
		"incomplete":         func(r *preflight.Request) { r.Movements[0].Inventory.Complete = false },
		"empty inventory":    func(r *preflight.Request) { r.Movements[0].Inventory.Datasets = nil },
		"scope mismatch":     func(r *preflight.Request) { r.Movements[0].Inventory.Scope.Cluster = "other" },
		"same environment":   func(r *preflight.Request) { r.Movements[0].Destination = "aws" },
		"duplicate movement": func(r *preflight.Request) { r.Movements = append(r.Movements, r.Movements[0]) },
		"duplicate dataset": func(r *preflight.Request) {
			m := &r.Movements[0]
			m.Inventory.Datasets = append(m.Inventory.Datasets, m.Inventory.Datasets[0])
		},
		"secret in id":    func(r *preflight.Request) { r.Movements[0].ID = "postgres://TOPSECRET" },
		"bad dataset":     func(r *preflight.Request) { r.Movements[0].Inventory.Datasets[0].Table = "TOPSECRET\n" },
		"bad source":      func(r *preflight.Request) { r.Movements[0].Source.Namespace = "TOPSECRET\n" },
		"bad destination": func(r *preflight.Request) { r.Movements[0].Destination = "https://TOPSECRET" },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			r := fixtureRequest()
			change(&r)
			v, err := e.Evaluate(context.Background(), r)
			raw, _ := json.Marshal(v)
			if v.Allowed || !errors.Is(err, ErrInvalidInput) || strings.Contains(string(raw), "TOPSECRET") || strings.Contains(err.Error(), "TOPSECRET") {
				t.Fatalf("unsafe result %s err=%v", raw, err)
			}
		})
	}
}

func TestWholeClusterMixedLabelsAndDirections(t *testing.T) {
	c := fixtureConfig()
	// The restricted table is in a different database from the app.
	c.Catalog[0].Datasets[0].Label = "global"
	c.Catalog[0].Datasets[1].Label = "kr-only"
	for _, source := range []string{"aws", "onprem"} {
		c.Catalog[0].Scope.Environment = source
		r := fixtureRequest()
		r.Movements[0].Source = c.Catalog[0].Scope
		r.Movements[0].Inventory.Scope = c.Catalog[0].Scope
		r.Movements[0].Destination = "tokyo"
		v, err := mustEvaluator(t, c).Evaluate(context.Background(), r)
		if err != nil || v.Allowed || len(v.Reasons) != 1 || *v.Reasons[0].Dataset != c.Catalog[0].Datasets[1].Dataset {
			t.Fatalf("%+v %v", v, err)
		}
		r.Movements[0].Destination = "aws"
		if source == "aws" {
			r.Movements[0].Destination = "onprem"
		}
		v, err = mustEvaluator(t, c).Evaluate(context.Background(), r)
		if err != nil || !v.Allowed {
			t.Fatalf("reverse/same-country: %+v %v", v, err)
		}
	}
}

func TestEveryMovementMustPass(t *testing.T) {
	e := mustEvaluator(t, fixtureConfig())
	r := fixtureRequest()
	other := r.Movements[0]
	other.ID = "denied"
	other.Destination = "tokyo"
	r.Movements = append(r.Movements, other)
	v, err := e.Evaluate(context.Background(), r)
	if err != nil || v.Allowed || len(v.Reasons) != 1 || v.Reasons[0].MovementID != "denied" {
		t.Fatalf("%+v %v", v, err)
	}
	slices.Reverse(r.Movements)
	v2, err := e.Evaluate(context.Background(), r)
	if err != nil || !reflect.DeepEqual(v, v2) {
		t.Fatalf("order changed result: %+v %v", v2, err)
	}
}

func TestDigestAndPolicySnapshot(t *testing.T) {
	c := fixtureConfig()
	e := mustEvaluator(t, c)
	slices.Reverse(c.Environments)
	slices.Reverse(c.APACCountries)
	slices.Reverse(c.Catalog[0].Datasets)
	if e.digest != mustEvaluator(t, c).digest {
		t.Fatal("order changed digest")
	}
	for i := range c.Catalog[0].Datasets {
		c.Catalog[0].Datasets[i].Label = "global"
	}
	if e.digest == mustEvaluator(t, c).digest {
		t.Fatal("policy change did not change digest")
	}
	r := fixtureRequest()
	r.Movements[0].Destination = "tokyo"
	v, err := e.Evaluate(context.Background(), r)
	if err != nil || v.Allowed {
		t.Fatalf("caller mutation changed policy: %+v %v", v, err)
	}
	changed, err := prepare(context.Background(), fixtureConfig(), policySource+"\n# revision\n")
	if err != nil || changed.digest == e.digest {
		t.Fatal("rego not bound to digest", err)
	}
}

func TestUnreadyAndCancelled(t *testing.T) {
	for _, e := range []*Evaluator{nil, {}} {
		v, err := e.Evaluate(context.Background(), fixtureRequest())
		if v.Allowed || !errors.Is(err, ErrNotReady) {
			t.Fatalf("%+v %v", v, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	v, err := mustEvaluator(t, fixtureConfig()).Evaluate(ctx, fixtureRequest())
	if v.Allowed || !errors.Is(err, context.Canceled) {
		t.Fatalf("%+v %v", v, err)
	}
	if _, err := New(ctx, fixtureConfig()); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestPolicyErrorsAndMalformedResults(t *testing.T) {
	for name, body := range map[string]string{
		"undefined":         `other := true`,
		"missing allowed":   `decision := {"reasons": []}`,
		"null reasons":      `decision := {"allowed": true, "reasons": null}`,
		"inconsistent deny": `decision := {"allowed": false, "reasons": []}`,
		"unknown field":     `decision := {"allowed": true, "reasons": [], "TOPSECRET": true}`,
		"invalid reason":    `decision := {"allowed": false, "reasons": [{"code":"TOPSECRET","movementId":"move"}]}`,
		"builtin error":     `decision := {"allowed": to_number("TOPSECRET"), "reasons": []}`,
	} {
		t.Run(name, func(t *testing.T) {
			e, err := prepare(context.Background(), fixtureConfig(), "package hopcharlie.residency\nimport rego.v1\n"+body)
			if err != nil {
				t.Fatal(err)
			}
			v, err := e.Evaluate(context.Background(), fixtureRequest())
			if v.Allowed || !errors.Is(err, ErrEvaluation) {
				t.Fatalf("%+v %v", v, err)
			}
			raw, _ := json.Marshal(v)
			if strings.Contains(string(raw)+err.Error(), "TOPSECRET") {
				t.Fatal("leaked evaluation input")
			}
		})
	}
	for _, policy := range []string{"TOPSECRET INVALID REGO", `package hopcharlie.residency
 decision := http.send({"method":"GET", "url":"https://invalid.example"})`} {
		if _, err := prepare(context.Background(), fixtureConfig(), policy); !errors.Is(err, ErrEvaluation) {
			t.Fatalf("unsafe policy compiled: %v", err)
		}
	}
}

func TestConcurrentEvaluation(t *testing.T) {
	e := mustEvaluator(t, fixtureConfig())
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			v, err := e.Evaluate(context.Background(), fixtureRequest())
			if err != nil || !v.Allowed {
				t.Errorf("%+v %v", v, err)
			}
		})
	}
	wg.Wait()
}
