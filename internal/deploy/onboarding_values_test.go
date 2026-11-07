package deploy

import (
	"errors"
	"testing"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/appspec"
)

// The gitops values of an onboarding (ADR 0042) are a contract only: Render
// refuses them rather than leave them out, until it renders them.
func TestRenderRefusesOnboardingValues(t *testing.T) {
	spec, errs := appspec.Check([]byte(pgSpec))
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	base := func() File {
		return File{App: "shop", Instance: "shop", Env: "eks", Commit: "c", Deployment: "dep_1", Spec: *spec,
			Images:     map[string]string{"web": "r/web@sha256:aa"},
			Sealed:     &Sealed{PGApp: map[string]string{"password": "Ag1"}, PGCA: map[string]string{"ca.crt": "Ag2", "ca.key": "Ag3"}},
			DBTopology: &DBTopology{Primary: "eks", Peer: "gke", PeerAbsent: true}}
	}
	if _, err := Render(base()); err != nil {
		t.Fatalf("the same file without them: %v", err)
	}
	for name, set := range map[string]func(*File){
		"postgres": func(f *File) {
			f.Postgres = &PostgresValues{Image: "ghcr.io/cloudnative-pg/postgresql:17.11-standard-bookworm"}
		},
		"onboarding cold": func(f *File) { f.OnboardingCold = true },
		"external": func(f *File) {
			f.DBTopology.External = &ExternalSource{Name: "legacy", Host: "hop-db-legacy-shop", User: "hopcharlie_replicator",
				PasswordSecret: "shop-db-legacy", SSLMode: "prefer"}
		},
		"pgLegacy": func(f *File) { f.Sealed.PGLegacy = map[string]string{"password": "Ag4"} },
	} {
		f := base()
		set(&f)
		if out, err := Render(f); !errors.Is(err, errOnboardingValues) {
			t.Errorf("%s: Render = %v\n%s", name, err, out)
		}
	}
}
