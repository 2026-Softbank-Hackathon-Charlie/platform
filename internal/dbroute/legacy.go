package dbroute

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/onboard"
)

// LegacyRoute is the LabelRoute value of an onboarding's route to its
// source database (ADR 0042): not an environment, so Reconcile, Sync and
// Remove never take it for a route of an environment the app left, and
// only RemoveLegacy deletes it.
const LegacyRoute = onboard.SourceName

// LegacyNamespace is the namespace of the legacy routes: the environments'
// routes' (the first one in name order when they differ).
func (m *Manager) LegacyNamespace() string {
	if m.cfg.LegacyNamespace != "" {
		return m.cfg.LegacyNamespace
	}
	for _, env := range slices.Sorted(maps.Keys(m.cfg.Routes)) {
		return m.cfg.Routes[env].Namespace
	}
	return ""
}

// LegacyHost is the "host:port" the management cluster reaches app's
// source at: db-legacy-<app>.<namespace>.svc.cluster.local:5432.
func (m *Manager) LegacyHost(app string) string {
	return onboard.RouteName(app) + "." + m.LegacyNamespace() + ".svc.cluster.local:5432"
}

// LegacyService is the egress Service to app's source over the tailnet,
// tailnet FQDN onboard.TailnetHost(app).
func (m *Manager) LegacyService(app string) *corev1.Service {
	svc := m.Service("", app, app)
	svc.Name, svc.Namespace = onboard.RouteName(app), m.LegacyNamespace()
	svc.Labels[LabelRoute] = LegacyRoute
	svc.Annotations[AnnotationTailnetFQDN] = onboard.TailnetHost(app) + "." + m.cfg.Tailnet
	svc.Annotations[AnnotationHostname] = "hop-db-management-to-" + LegacyRoute + "-" + app
	return svc
}

// EnsureLegacy creates app's legacy route unless it is there, and returns
// LegacyHost. A Service of that name that is not the platform's legacy
// route of app is ErrConflict.
func (m *Manager) EnsureLegacy(ctx context.Context, app string) (string, error) {
	want := m.LegacyService(app)
	if msgs := validation.IsDNS1035Label(want.Name); len(msgs) > 0 {
		return "", fmt.Errorf("service name %s: %s", want.Name, strings.Join(msgs, "; "))
	}
	svcs := m.cfg.Client.CoreV1().Services(want.Namespace)
	got, err := svcs.Get(ctx, want.Name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		if _, err := svcs.Create(ctx, want, metav1.CreateOptions{FieldManager: ManagedBy}); err != nil && !apierrors.IsAlreadyExists(err) {
			return "", err
		}
		m.cfg.Log.Info("legacy database route created", "app", app, "service", want.Namespace+"/"+want.Name)
		return m.LegacyHost(app), nil
	case err != nil:
		return "", err
	}
	if !m.legacyOurs(got, app) {
		return "", ErrConflict
	}
	return m.LegacyHost(app), nil
}

// RemoveLegacy deletes app's legacy route; gone already is no error.
func (m *Manager) RemoveLegacy(ctx context.Context, app string) error {
	ns, name := m.LegacyNamespace(), onboard.RouteName(app)
	got, err := m.cfg.Client.CoreV1().Services(ns).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !m.legacyOurs(got, app) {
		return ErrConflict
	}
	return m.delete(ctx, got, app, LegacyRoute)
}

func (m *Manager) legacyOurs(svc *corev1.Service, app string) bool {
	return svc.Labels[LabelManagedBy] == ManagedBy && svc.Labels[LabelApp] == app && svc.Labels[LabelRoute] == LegacyRoute
}
