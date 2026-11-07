package plans

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/preflight"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store/storetest"
)

const onbHost = "db-legacy-shop.hopcharlie-db.svc.cluster.local:5432"

// onbPlan is an onboarding's cutover plan as internal/onboard writes it.
func onbPlan(edit func(string) string) []byte {
	s := `{
  "id": "onb-3f9a1c2b7d4e-1",
  "source": {"kind":"external","name":"legacy","env":"legacy","namespace":"","cluster":"",
    "dsn":"postgres://moim:${LEGACY_PGPASSWORD}@db-legacy-shop.hopcharlie-db.svc.cluster.local:5432/moim?sslmode=prefer"},
  "target": {"env":"eks","context":"eks-ctx","namespace":"shop","cluster":"shop-db",
    "dsn":"postgres://moim:${DST_PGPASSWORD}@shop-db-eks.ts.net:5432/moim",
    "app":{"mode":"cold","kind":"Deployment","name":"shop-web","replicas":2,
      "smokeUrl":"https://shop.eks.example","smoke":[{"method":"GET","path":"/healthz"}]}},
  "gateway": {"mode":"http","url":"http://gw:9901/apps/shop","sourceBackend":"legacy","targetBackend":"eks"},
  "files": {"source":"legacy:moim-files","target":"eks:shop-files"},
  "verify": {"fenceGate": true}
}`
	if edit != nil {
		s = edit(s)
	}
	return []byte(s)
}

func onbPolicy() Policy {
	p := DefaultPolicy()
	p.GatewayURL = "http://gw:9901"
	p.Buckets = []string{"eks:shop-files"}
	p.AppDBHosts = map[string][]string{"shop": {"shop-db-eks.ts.net:5432"}}
	return p
}

var onbScope = OnboardingScope{App: "shop", SourceHost: onbHost, SourceBucket: "legacy:moim-files", SmokeURL: "https://shop.eks.example", Password: "fence-pw"}

func TestReserveOnboarding(t *testing.T) {
	t.Setenv(LegacyPasswordEnv, "")
	t.Setenv("DST_PGPASSWORD", "app-pw")
	st := storetest.NewMemory()
	s := New(onbPolicy(), st)
	res, ref, err := s.ReserveOnboarding(context.Background(), onbPlan(nil), "op", onbScope)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Plan.Source.External() || !res.Plan.Target.App.Cold() || !strings.Contains(res.Plan.Source.DSN, "fence-pw") {
		t.Fatalf("plan %+v", res.Plan.Source)
	}
	if res.Version.Outcome != OutcomeExternalSource || res.Capacity.Outcome != OutcomeExternalSource || res.Parity.Outcome != OutcomeExternalSource {
		t.Fatalf("gates %+v %+v %+v", res.Version, res.Capacity, res.Parity)
	}
	row, err := st.GetPlan(context.Background(), ref)
	if err != nil || row.Status != store.PlanStarting || row.SubmittedRole != OnboardingRole {
		t.Fatalf("stored %+v %v", row, err)
	}
	if v, set := os.LookupEnv(LegacyPasswordEnv); set || v != "" {
		t.Fatal("the fence password stayed in the process environment")
	}
	if strings.Contains(string(row.Raw), "fence-pw") {
		t.Fatal("the stored plan holds the expanded password")
	}

	// Its halted run resumes with the same scope; without it, it does not load.
	if _, err := s.Load(context.Background(), ref); err == nil {
		t.Fatal("an onboarding plan loaded without the onboardings' scopes")
	}
	s.SetOnboardingScopes(func(_ context.Context, id string) (OnboardingScope, bool, error) {
		return onbScope, id == "onb-3f9a1c2b7d4e-1", nil
	})
	if p, err := s.Load(context.Background(), ref); err != nil || !p.Target.App.Cold() {
		t.Fatalf("load: %v", err)
	}
}

func TestOnboardingPolicyRefuses(t *testing.T) {
	t.Setenv(LegacyPasswordEnv, "fence-pw")
	t.Setenv("DST_PGPASSWORD", "app-pw")
	cases := map[string]func(string) string{
		// LEGACY_PGPASSWORD goes to the source route only.
		"password to another host": func(s string) string {
			return strings.Replace(s, "@db-legacy-shop.hopcharlie-db.svc.cluster.local:5432/moim?sslmode=prefer", "@evil.example:5432/moim", 1)
		},
		"cnpg source": func(s string) string {
			return strings.Replace(s, `"kind":"external","name":"legacy",`, ``, 1)
		},
		"warm app":    func(s string) string { return strings.Replace(s, `"mode":"cold"`, `"mode":"warm"`, 1) },
		"another app": func(s string) string { return strings.Replace(s, `"name":"shop-web"`, `"name":"other-web"`, 1) },
		"smoke url": func(s string) string {
			return strings.Replace(s, `"https://shop.eks.example"`, `"https://evil.example"`, 1)
		},
		"writing smoke": func(s string) string {
			return strings.Replace(s, `"method":"GET"`, `"method":"POST"`, 1)
		},
		"unregistered bucket": func(s string) string {
			return strings.Replace(s, `"legacy:moim-files"`, `"legacy:other"`, 1)
		},
		"another gateway scope": func(s string) string {
			return strings.Replace(s, `/apps/shop"`, `/apps/other"`, 1)
		},
	}
	for name, edit := range cases {
		t.Run(name, func(t *testing.T) {
			s := New(onbPolicy(), storetest.NewMemory())
			if _, _, err := s.ReserveOnboarding(context.Background(), onbPlan(edit), "op", onbScope); !errors.Is(err, ErrInvalid) {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

// TestServerPolicyRefusesOnboardingPlan: the same plan submitted through
// the API is refused: no LEGACY_PGPASSWORD, no external source, no cold app.
func TestServerPolicyRefusesOnboardingPlan(t *testing.T) {
	t.Setenv(LegacyPasswordEnv, "fence-pw")
	s := New(onbPolicy(), storetest.NewMemory())
	if _, err := s.Submit(context.Background(), onbPlan(nil), "op"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("submit: %v", err)
	}
	// Even with an allowed password variable, an external source is refused.
	p := onbPolicy()
	p.PasswordEnvs = append(p.PasswordEnvs, LegacyPasswordEnv)
	p.AppDBHosts["shop"] = append(p.AppDBHosts["shop"], onbHost)
	p.Buckets = append(p.Buckets, "legacy:moim-files")
	s = New(p, storetest.NewMemory())
	_, err := s.Submit(context.Background(), onbPlan(nil), "op")
	if !errors.Is(err, ErrInvalid) || (!strings.Contains(err.Error(), "external")) {
		t.Fatalf("submit external: %v", err)
	}
}

type refusingGate struct{ t *testing.T }

func (g refusingGate) Check(context.Context, *plan.Plan) (preflight.ResidencyVerdict, error) {
	g.t.Error("the residency gate ran for an external source")
	return preflight.ResidencyVerdict{}, nil
}

// TestReserveOnboardingSkipsResidency: with a residency gate, an
// onboarding's plan is not checked by it and records the outcome.
func TestReserveOnboardingSkipsResidency(t *testing.T) {
	t.Setenv("DST_PGPASSWORD", "app-pw")
	s := New(onbPolicy(), storetest.NewMemory())
	s.SetResidencyGate(refusingGate{t})
	res, _, err := s.ReserveOnboarding(context.Background(), onbPlan(nil), "op", onbScope)
	if err != nil || res.Residency.Outcome != OutcomeExternalSource {
		t.Fatalf("residency %+v %v", res.Residency, err)
	}
}
