package recommend

import (
	"slices"
	"strings"
	"testing"
	"time"
)

func TestBuildRequiresComparableFullCostsAndPreservesCountryRestriction(t *testing.T) {
	now := time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC)
	app := Application{Name: "shop", Revision: "revision-1", Current: str("gke"), Environments: []string{"gke", "onprem", "eks"}, Persistent: true}
	envs := []Environment{{Name: "gke", Provider: "gcp", Region: "asia-northeast3", Country: "KR", Ready: true, ObservedAt: now}, {Name: "onprem", Provider: "onprem", Country: "KR", Ready: true, ObservedAt: now}, {Name: "eks", Provider: "aws", Region: "ap-northeast-1", Country: "JP", Ready: true, ObservedAt: now}}
	quote := func(e Environment, price float64) CostQuote {
		return CostQuote{App: app.Name, Environment: e.Name, Region: e.Region, WorkloadRevision: app.Revision, MonthlyUSD: price, PeriodHours: 730, ObservedAt: now, IncludesStorage: true, IncludesDataTransfer: true, IncludesOperations: true, IncludesPower: true, IncludesAmortization: true, CommitmentReviewed: true, CapacityReviewed: true, ResidencyReviewed: true}
	}
	book := &CostBook{Version: 1, Quotes: []CostQuote{quote(envs[0], 100), quote(envs[1], 40), quote(envs[2], 10)}}
	for _, tc := range []struct {
		name     string
		change   func(*Application, []Environment, *CostBook)
		wantMove bool
	}{
		{"complete", func(*Application, []Environment, *CostBook) {}, true},
		{"missing transfer", func(_ *Application, _ []Environment, b *CostBook) { b.Quotes[1].IncludesDataTransfer = false }, false},
		{"old revision", func(_ *Application, _ []Environment, b *CostBook) { b.Quotes[1].WorkloadRevision = "old" }, false},
		{"stale quote", func(_ *Application, _ []Environment, b *CostBook) { b.Quotes[1].ObservedAt = now.Add(-25 * time.Hour) }, false},
		{"free onprem", func(_ *Application, _ []Environment, b *CostBook) { b.Quotes[1].MonthlyUSD = 0 }, false},
		{"missing power", func(_ *Application, _ []Environment, b *CostBook) { b.Quotes[1].IncludesPower = false }, false},
		{"different period", func(_ *Application, _ []Environment, b *CostBook) { b.Quotes[1].PeriodHours = 720 }, false},
		{"stale environment", func(_ *Application, e []Environment, _ *CostBook) { e[1].ObservedAt = now.Add(-3 * time.Minute) }, false},
		{"writes held", func(a *Application, _ []Environment, _ *CostBook) { a.Moving = true }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := app
			e := slices.Clone(envs)
			b := *book
			b.Quotes = slices.Clone(book.Quotes)
			tc.change(&a, e, &b)
			items := Build([]Application{a}, e, &b, nil, now)
			found := false
			for _, c := range items {
				if c.Target != nil && *c.Target == "onprem" {
					found = true
					if slices.Contains(c.AllowedActions, "move") != tc.wantMove {
						t.Fatalf("move allowed=%v", c.AllowedActions)
					}
					if !tc.wantMove && c.EstimatedMonthlySavingsUSD != nil {
						t.Fatal("unknown treated as saving")
					}
				}
			}
			if !found {
				t.Fatal("registered candidate missing")
			}
		})
	}
	app.Countries = []string{"KR"}
	for _, c := range Build([]Application{app}, envs, book, nil, now) {
		if c.Target != nil && *c.Target == "eks" && (slices.Contains(c.AllowedActions, "move") || c.EstimatedMonthlySavingsUSD != nil) {
			t.Fatal("cheap JP quote overrode KR-only data")
		}
	}
	for _, c := range Build([]Application{app}, envs, nil, map[string]string{"eks": "reference compute rate"}, now) {
		if slices.Contains(c.AllowedActions, "move") || c.EstimatedMonthlySavingsUSD != nil {
			t.Fatal("public unit rate invented workload saving")
		}
	}
}

func TestLongRegisteredNamesStillProduceProducerCompatibleCandidateIDs(t *testing.T) {
	now := time.Now().UTC()
	source, target := strings.Repeat("s", 63), strings.Repeat("t", 63)
	app := Application{Name: strings.Repeat("a", 63), Current: &source, Environments: []string{source, target}}
	items := Build([]Application{app}, []Environment{{Name: source}, {Name: target}}, nil, nil, now)
	if len(items) != 1 || !candidateLabel.MatchString(items[0].ID) || validateCandidates(items) != nil {
		t.Fatal("long registered names broke the AI producer contract")
	}
}
