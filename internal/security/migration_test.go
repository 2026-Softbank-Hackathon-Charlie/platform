package security

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

const migrationFixtureApp = "moimroom-mig"

func migrationPod(name string, spec corev1.PodSpec) *corev1.Pod {
	p := pod(migrationFixtureApp, "moimroom-mig-web", name, spec)
	p.Labels["hopcharlie.dev/app"] = migrationFixtureApp
	p.Labels["hopcharlie.dev/service"] = "web"
	p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	return p
}

func TestMigrationPrivilegedUsesScopedPages(t *testing.T) {
	kube := newFakeKube()
	spec := corev1.PodSpec{Containers: []corev1.Container{{Name: "web"}}}
	one := migrationPod("web-first", spec)
	two := migrationPod("web-second", spec)
	two.OwnerReferences[0].Name = "moimroom-mig-api-5d8f"
	kube.PrependReactor("list", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		list := a.(k8stesting.ListActionImpl)
		selector := list.GetListRestrictions().Labels
		if a.GetNamespace() != migrationFixtureApp ||
			!selector.Matches(labels.Set{"hopcharlie.dev/app": migrationFixtureApp, "hopcharlie.dev/service": "web"}) ||
			selector.Matches(labels.Set{"hopcharlie.dev/app": "other", "hopcharlie.dev/service": "web"}) ||
			selector.Matches(labels.Set{"hopcharlie.dev/app": migrationFixtureApp}) ||
			selector.Matches(labels.Set{"hopcharlie.dev/app": migrationFixtureApp, "hopcharlie.dev/service": "postgres", "cnpg.io/cluster": "db"}) {
			t.Fatalf("list was not scoped to the app's managed workload pods: %+v", list)
		}
		opts := list.GetListOptions()
		if opts.Limit <= 0 {
			t.Fatal("list does not bound a page")
		}
		switch opts.Continue {
		case "":
			return true, &corev1.PodList{ListMeta: metav1.ListMeta{Continue: "next"}, Items: []corev1.Pod{*one}}, nil
		case "next":
			return true, &corev1.PodList{Items: []corev1.Pod{*two}}, nil
		default:
			return true, nil, fmt.Errorf("unexpected continuation %q", opts.Continue)
		}
	})
	got := AssessMigrationPrivileged(t.Context(), kube, migrationFixtureApp, migrationFixtureApp)
	if got.Result != apiv1.SecurityControlResultPass || got.Reason != "clear" ||
		got.PolicyVersion != PolicyVersion || got.Workload != "moimroom-mig / App/moimroom-mig" ||
		got.Evidence == nil || !strings.Contains(*got.Evidence, "2 ") || len(kube.Actions()) != 2 {
		t.Fatalf("assessment: %+v, %d list actions", got, len(kube.Actions()))
	}
}

func TestMigrationPrivilegedFindsViolationAcrossContainerTypes(t *testing.T) {
	privileged := corev1.Container{Name: "privileged-container", SecurityContext: &corev1.SecurityContext{Privileged: yes()}}
	for _, tc := range []struct {
		name string
		spec corev1.PodSpec
	}{
		{"regular", corev1.PodSpec{Containers: []corev1.Container{privileged}}},
		{"init", corev1.PodSpec{InitContainers: []corev1.Container{privileged}}},
		{"ephemeral", corev1.PodSpec{EphemeralContainers: []corev1.EphemeralContainer{{EphemeralContainerCommon: corev1.EphemeralContainerCommon{Name: privileged.Name, SecurityContext: privileged.SecurityContext}}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kube := newFakeKube(
				migrationPod("web-clean", corev1.PodSpec{Containers: []corev1.Container{{Name: "web"}}}),
				migrationPod("web-privileged", tc.spec),
			)
			got := AssessMigrationPrivileged(t.Context(), kube, migrationFixtureApp, migrationFixtureApp)
			if got.Result != apiv1.SecurityControlResultFail || got.Reason != "privileged" ||
				got.Workload != "moimroom-mig / Deployment/moimroom-mig-web" ||
				got.Evidence == nil || !strings.Contains(*got.Evidence, "web-privileged") ||
				!strings.Contains(*got.Evidence, "privileged-container") ||
				!strings.Contains(*got.Evidence, "securityContext.privileged: true") {
				t.Fatalf("assessment: %+v", got)
			}
		})
	}
}

func TestMigrationPrivilegedDoesNotPassWithoutActiveManagedPods(t *testing.T) {
	spec := corev1.PodSpec{Containers: []corev1.Container{{Name: "web", SecurityContext: &corev1.SecurityContext{Privileged: yes()}}}}
	finished := migrationPod("finished", spec)
	finished.Status.Phase = corev1.PodSucceeded
	deleting := migrationPod("deleting", spec)
	now := metav1.Now()
	deleting.DeletionTimestamp = &now
	other := migrationPod("other", spec)
	other.Labels["hopcharlie.dev/app"] = "other"
	database := migrationPod("db", spec)
	delete(database.Labels, "hopcharlie.dev/service")
	kube := newFakeKube(finished, deleting, other, database)
	got := AssessMigrationPrivileged(t.Context(), kube, migrationFixtureApp, migrationFixtureApp)
	if got.Result != apiv1.SecurityControlResultUnknown || got.Reason != "workload_not_ready" ||
		got.Workload != "moimroom-mig / App/moimroom-mig" {
		t.Fatalf("assessment: %+v", got)
	}
}

func TestMigrationPrivilegedSanitizesPartialReadFailure(t *testing.T) {
	kube := newFakeKube()
	p := migrationPod("web-first", corev1.PodSpec{Containers: []corev1.Container{{Name: "web"}}})
	kube.PrependReactor("list", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.(k8stesting.ListActionImpl).GetListOptions().Continue == "" {
			return true, &corev1.PodList{ListMeta: metav1.ListMeta{Continue: "next"}, Items: []corev1.Pod{*p}}, nil
		}
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "", errors.New("internal API detail"))
	})
	got := AssessMigrationPrivileged(t.Context(), kube, migrationFixtureApp, migrationFixtureApp)
	if got.Result != apiv1.SecurityControlResultUnknown || got.Reason != "forbidden" ||
		got.Evidence == nil || !strings.Contains(*got.Evidence, "403") || strings.Contains(*got.Evidence, "internal API detail") {
		t.Fatalf("assessment: %+v", got)
	}
}

func TestMigrationPrivilegedUsesAppIdentityForUnrecognizedOwner(t *testing.T) {
	p := migrationPod("web", corev1.PodSpec{Containers: []corev1.Container{{Name: "web", SecurityContext: &corev1.SecurityContext{Privileged: yes()}}}})
	p.OwnerReferences[0].Kind = "Other"
	p.OwnerReferences[0].Name = "https://owner-ref.example"
	got := AssessMigrationPrivileged(t.Context(), newFakeKube(p), migrationFixtureApp, migrationFixtureApp)
	if got.Result != apiv1.SecurityControlResultFail || got.Workload != "moimroom-mig / App/moimroom-mig" ||
		got.Evidence == nil || strings.Contains(*got.Evidence, "owner-ref") {
		t.Fatalf("assessment: %+v", got)
	}
}

func TestMigrationPrivilegedRequiresReadyApplicationPodForPass(t *testing.T) {
	clean := corev1.PodSpec{Containers: []corev1.Container{{Name: "web"}}}
	db := migrationPod("db", clean)
	db.Labels["hopcharlie.dev/service"] = "postgres"
	db.Labels["cnpg.io/cluster"] = "db"
	unready := migrationPod("unready", clean)
	unready.Status.Conditions[0].Status = corev1.ConditionFalse
	for _, p := range []*corev1.Pod{db, unready} {
		t.Run(p.Name, func(t *testing.T) {
			got := AssessMigrationPrivileged(t.Context(), newFakeKube(p), migrationFixtureApp, migrationFixtureApp)
			if got.Result != apiv1.SecurityControlResultUnknown || got.Reason != "workload_not_ready" ||
				got.Evidence == nil || !strings.Contains(*got.Evidence, "0 Ready and running") {
				t.Fatalf("assessment: %+v", got)
			}
		})
	}
}

func TestMigrationPrivilegedFindsPendingViolationWithReadyPod(t *testing.T) {
	ready := migrationPod("ready", corev1.PodSpec{Containers: []corev1.Container{{Name: "web"}}})
	pending := migrationPod("pending", corev1.PodSpec{Containers: []corev1.Container{{Name: "web", SecurityContext: &corev1.SecurityContext{Privileged: yes()}}}})
	pending.Status.Phase, pending.Status.Conditions = corev1.PodPending, nil
	got := AssessMigrationPrivileged(t.Context(), newFakeKube(ready, pending), migrationFixtureApp, migrationFixtureApp)
	if got.Result != apiv1.SecurityControlResultFail || got.Reason != "privileged" ||
		got.Evidence == nil || !strings.Contains(*got.Evidence, "Pod/pending") {
		t.Fatalf("assessment: %+v", got)
	}
}
