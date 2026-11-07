package dbroute

import (
	"context"
	"errors"
	"testing"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
)

// TestLegacyRoute: an onboarding's route to its source (ADR 0042) is made
// and removed only by EnsureLegacy and RemoveLegacy; Reconcile and Remove
// leave it, as it is the route of no environment.
func TestLegacyRoute(t *testing.T) {
	ctx := context.Background()
	m, cs := newManager(t, &pairs{list: []store.AppDatabase{pair("shop")}})
	host, err := m.EnsureLegacy(ctx, "moim")
	if err != nil {
		t.Fatal(err)
	}
	if host != "db-legacy-moim."+ns+".svc.cluster.local:5432" {
		t.Fatalf("host %s", host)
	}
	svc, ok := get(t, cs, "db-legacy-moim")
	if !ok {
		t.Fatal("not created")
	}
	if a := svc.Annotations; a[AnnotationTailnetFQDN] != "hop-db-legacy-moim.tail567fe8.ts.net" || a[AnnotationHostname] != "hop-db-management-to-legacy-moim" ||
		a[AnnotationTags] != "tag:hop-db-client-management" || svc.Labels[LabelRoute] != LegacyRoute {
		t.Fatalf("service %+v", svc.ObjectMeta)
	}
	if _, err := m.EnsureLegacy(ctx, "moim"); err != nil {
		t.Fatalf("again: %v", err)
	}
	// moim has no pair (yet) and shop has one: neither removes the route.
	if err := m.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if err := m.Remove(ctx, "moim"); err != nil {
		t.Fatal(err)
	}
	if _, ok := get(t, cs, "db-legacy-moim"); !ok {
		t.Fatal("Reconcile or Remove deleted the legacy route")
	}
	if err := m.RemoveLegacy(ctx, "moim"); err != nil {
		t.Fatal(err)
	}
	if _, ok := get(t, cs, "db-legacy-moim"); ok {
		t.Fatal("RemoveLegacy left it")
	}
	if err := m.RemoveLegacy(ctx, "moim"); err != nil {
		t.Fatalf("removing a gone route: %v", err)
	}
	// A Service of the name that is not ours is left alone.
	m, _ = newManager(t, &pairs{}, service("db-legacy-x", nil))
	if _, err := m.EnsureLegacy(ctx, "x"); !errors.Is(err, ErrConflict) {
		t.Fatalf("foreign service: %v", err)
	}
	if err := m.RemoveLegacy(ctx, "x"); !errors.Is(err, ErrConflict) {
		t.Fatalf("remove foreign: %v", err)
	}
}
