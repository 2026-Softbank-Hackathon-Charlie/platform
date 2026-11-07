package airun

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/recommend"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestRecommendationUsesExistingImageWithoutClusterInputsAndCannotMultiplyBudget(t *testing.T) {
	cs := clientset()
	tpl := template()
	tpl.Annotations = map[string]string{recommendationCapability: "v1"}
	tpl.Spec.Template.Spec.InitContainers = []corev1.Container{{Name: "fetch", Image: "aws-cli:test"}}
	r, _ := New(cs, "ns", tpl)
	now := time.Now().UTC()
	input, _ := json.Marshal(recommend.Input{SnapshotID: "rec-123", GeneratedAt: now, ExpiresAt: now.Add(recommend.AnalysisTimeout), Candidates: []recommend.Candidate{}})
	q := recommend.Launch{ID: "rec-123", Input: input, Nonce: "one-use-nonce", CreatedAt: now, Actor: "viewer"}
	if err := r.StartRecommendation(context.Background(), q); err != nil {
		t.Fatal(err)
	}
	if err := r.StartRecommendation(context.Background(), q); !errors.Is(err, ErrBusy) {
		t.Fatal("duplicate assessment budget", err)
	}
	jobs, _ := cs.BatchV1().Jobs("ns").List(context.Background(), metav1.ListOptions{})
	if len(jobs.Items) != 1 {
		t.Fatal("multiple inference jobs")
	}
	j := jobs.Items[0]
	if j.Spec.Template.Spec.Containers[0].Image != "hopai:test" || *j.Spec.ActiveDeadlineSeconds != int64(recommend.AnalysisTimeout.Seconds()) {
		t.Fatal("image/budget changed")
	}
	for _, c := range append(j.Spec.Template.Spec.Containers, j.Spec.Template.Spec.InitContainers...) {
		env := map[string]string{}
		for _, e := range c.Env {
			env[e.Name] = e.Value
		}
		if env["KIND"] != "recommend" {
			t.Fatal("recommendation init fetched cluster credentials")
		}
		if c.Name == Container && (env["HOP_RECOMMENDATION_INPUT"] == "" || env["HOP_RECOMMENDATION_NONCE"] != q.Nonce) {
			t.Fatal("lost bound input")
		}
	}
	if normal, _ := r.List(context.Background(), ""); len(normal) != 0 {
		t.Fatal("recommendation shown as migration run")
	}
}
func TestLegacyTemplateDoesNotAccidentallyRunMigrationForRecommendation(t *testing.T) {
	r, _ := New(clientset(), "ns", template())
	if r.SupportsRecommendations() {
		t.Fatal("unreviewed template accepted")
	}
	if err := r.StartRecommendation(context.Background(), recommend.Launch{}); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
}
