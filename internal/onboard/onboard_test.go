package onboard

import (
	"os"
	"regexp"
	"slices"
	"testing"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/appspec"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/onboard/facts"
	"sigs.k8s.io/yaml"
)

// The names built from an onboarding id fit where they are used: the test
// app is an app name, a cutover plan id a plan id (plans policy).
func TestNamesFit(t *testing.T) {
	id := IDPrefix + "3f9a1c2b7d4e"
	if !ID.MatchString(id) {
		t.Fatalf("%s is not an id", id)
	}
	planID := regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`) // internal/plans planID
	if p := PlanID(id, 12); !planID.MatchString(p) {
		t.Errorf("plan id %q is not valid", p)
	}
	spec := []byte("name: " + TestApp(id) + "\nservices:\n  - {name: web, path: ., port: 8080, public: true}\n")
	if _, errs := appspec.Check(spec); slices.ContainsFunc(errs, func(e appspec.FieldError) bool { return e.Path == "name" }) {
		t.Errorf("test app %q is not a valid app name: %v", TestApp(id), errs)
	}
}

// OnboardingFinding.code in the OpenAPI contract lists exactly the codes
// of pkg/onboard/facts.
func TestFindingCodesMatchOpenAPI(t *testing.T) {
	raw, err := os.ReadFile("../../api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Components struct {
			Schemas map[string]struct {
				Properties map[string]struct {
					Enum []string `json:"enum"`
				} `json:"properties"`
			} `json:"schemas"`
		} `json:"components"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	got := doc.Components.Schemas["OnboardingFinding"].Properties["code"].Enum
	want := slices.Concat(facts.BlockerCodes, facts.WarningCodes)
	if !slices.Equal(got, want) {
		t.Fatalf("OnboardingFinding.code enum = %v\nwant %v", got, want)
	}
}
