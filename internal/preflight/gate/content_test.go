package gate

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/fileguard"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/preflight/residency"
)

// Only metadata/policy unit tests use this inspector. Production always uses
// fileguard.Inspector, whose byte/ZIP/snapshot tests are in internal/fileguard.
type cleanInspector struct{}

func (cleanInspector) Inspect(context.Context, string, []fileguard.Review, string) (fileguard.Report, error) {
	return fileguard.Report{Rules: fileguard.RulesVersion, Objects: 1, Bytes: 5, ManifestDigest: strings.Repeat("a", 64), Labels: []string{"global"}}, nil
}

type controlledInspector struct {
	labels  []string
	err     error
	calls   int
	reviews []fileguard.Review
}

func (i *controlledInspector) Inspect(_ context.Context, _ string, reviews []fileguard.Review, _ string) (fileguard.Report, error) {
	i.calls++
	i.reviews = reviews
	return fileguard.Report{Rules: fileguard.RulesVersion, Objects: 1, ManifestDigest: strings.Repeat("b", 64), Labels: i.labels, Findings: map[string]int{"email": 1}}, i.err
}

func contentGate(t *testing.T, change func(*residency.Config)) *Gate {
	t.Helper()
	cfg, err := residency.ParseConfig([]byte(strings.ReplaceAll(policy, "LABEL", "global")))
	if err != nil {
		t.Fatal(err)
	}
	sourceBucket(&cfg).Label = "global"
	if change != nil {
		change(&cfg)
	}
	g, err := New(context.Background(), cfg, complete())
	if err != nil {
		t.Fatal(err)
	}
	return g.WithProber(newProber())
}

func sourceBucket(c *residency.Config) *residency.Bucket {
	for i := range c.Buckets {
		if c.Buckets[i].Bucket == "s3:photos" {
			return &c.Buckets[i]
		}
	}
	panic("missing source test bucket")
}

func contentPlan() *plan.Plan {
	p := shopPlan("eks-jp")
	p.Files = &plan.FilesSpec{Source: "s3:photos", Target: "gcs:photos-tokyo"}
	return p
}

func TestContentsStrengthenDeclaredClassification(t *testing.T) {
	g := contentGate(t, nil)
	i := &controlledInspector{labels: []string{"global", "kr-only"}}
	g.WithContentInspector(i)
	v, err := g.Check(context.Background(), contentPlan())
	if err != nil || v.Allowed || !slices.Contains(codes(v), "bucket_country_not_allowed") || v.Content == nil || i.calls != 1 {
		t.Fatalf("PII under global bucket allowed: %+v %v", v, err)
	}
	g = contentGate(t, func(c *residency.Config) { sourceBucket(c).Label = "kr-only" })
	i = &controlledInspector{labels: []string{"global"}}
	g.WithContentInspector(i)
	v, err = g.Check(context.Background(), contentPlan())
	if err != nil || v.Allowed || i.calls != 0 {
		t.Fatalf("declared restriction weakened or unnecessary read: %+v %v", v, err)
	}
}

func TestInspectionFailureAndLocationAreClosed(t *testing.T) {
	for _, missingLocation := range []bool{false, true} {
		g := contentGate(t, func(c *residency.Config) {
			if missingLocation {
				c.InspectionEnvironment = ""
			}
		})
		i := &controlledInspector{err: errors.New("TOPSECRET body key")}
		g.WithContentInspector(i)
		v, err := g.Check(context.Background(), contentPlan())
		want := "file_inspection_failed"
		if missingLocation {
			want = "inspection_location_unverified"
		}
		if err != nil || v.Allowed || !slices.Contains(codes(v), want) || strings.Contains(strings.Join(codes(v), " "), "TOPSECRET") {
			t.Fatalf("unsafe verdict: %+v %v", v, err)
		}
		if missingLocation && i.calls != 0 {
			t.Fatal("unknown-location staging was contacted")
		}
	}
}

func TestPlanCannotChangeGuardOptions(t *testing.T) {
	g := contentGate(t, nil)
	i := &controlledInspector{labels: []string{"global"}}
	g.WithContentInspector(i)
	for _, flags := range [][]string{{"--size-only"}, {"--filter", "- *"}} {
		p := contentPlan()
		p.Files.Flags = flags
		v, err := g.Check(context.Background(), p)
		if err != nil || v.Allowed || !slices.Contains(codes(v), "file_guard_options_not_supported") || i.calls != 0 {
			t.Fatalf("options bypass: %+v %v", v, err)
		}
	}
}

func TestReviewCatalogIsImmutableAndPassedToInspector(t *testing.T) {
	rv := fileguard.Review{SHA256: strings.Repeat("a", 64), Label: "kr-only"}
	reviews := []fileguard.Review{rv}
	g := contentGate(t, func(c *residency.Config) { sourceBucket(c).ReviewedFiles = reviews })
	reviews[0].Label = "global" // mutation after construction must not change the gate
	i := &controlledInspector{labels: []string{"global"}}
	g.WithContentInspector(i)
	if _, err := g.Check(context.Background(), contentPlan()); err != nil {
		t.Fatal(err)
	}
	if len(i.reviews) != 1 || i.reviews[0] != rv {
		t.Fatalf("review catalog omitted: %v", i.reviews)
	}
}
