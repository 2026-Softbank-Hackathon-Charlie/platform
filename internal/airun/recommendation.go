package airun

import (
	"context"
	"encoding/json"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/recommend"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const recommendationCapability = "hopcharlie.dev/recommendations"
const recommendationLabel = "hopcharlie.dev/ai-recommendation"
const recommendationCreated = "hopcharlie.dev/recommendation-created-at"

// A legacy template would execute a full rehearsal. Opt in only with the
// reviewed command/fetch branches; no permission or credential is added here.
func (r *Runner) SupportsRecommendations() bool {
	return r.template.Annotations[recommendationCapability] == "v1"
}

func (r *Runner) StartRecommendation(ctx context.Context, q recommend.Launch) error {
	if !r.SupportsRecommendations() || !name.MatchString(q.ID) || len(q.Input) > recommend.MaxInputBytes || len(q.Nonce) < 8 || len(q.Nonce) > 128 {
		return ErrInvalid
	}
	var input recommend.Input
	if json.Unmarshal(q.Input, &input) != nil || input.SnapshotID != q.ID || q.CreatedAt.IsZero() {
		return ErrInvalid
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	jobs, err := r.cs.BatchV1().Jobs(r.namespace).List(ctx, metav1.ListOptions{LabelSelector: recommendationLabel + "=true", Limit: 100})
	if err != nil {
		return err
	}
	if jobs.Continue != "" {
		return ErrBusy
	}
	for _, j := range jobs.Items {
		created := j.CreationTimestamp.Time
		if value, e := time.Parse(time.RFC3339Nano, j.Annotations[recommendationCreated]); e == nil {
			created = value
		}
		if created.IsZero() || q.CreatedAt.Before(created.Add(recommend.TTL)) {
			return ErrBusy
		}
	}
	job := r.template.DeepCopy()
	job.ObjectMeta = metav1.ObjectMeta{GenerateName: "hopai-recommend-", Namespace: r.namespace,
		Labels:      merge(r.template.Labels, map[string]string{LabelRun: "true", recommendationLabel: "true", LabelApp: "recommendations"}),
		Annotations: merge(r.template.Annotations, map[string]string{AnnotRequestedBy: q.Actor, recommendationCreated: q.CreatedAt.UTC().Format(time.RFC3339Nano)})}
	job.Spec.Template.Labels = merge(job.Spec.Template.Labels, map[string]string{LabelRun: "true", recommendationLabel: "true", LabelApp: "recommendations"})
	deadline, backoff := int64(recommend.AnalysisTimeout.Seconds()), int32(0)
	job.Spec.ActiveDeadlineSeconds, job.Spec.BackoffLimit = &deadline, &backoff
	for i := range job.Spec.Template.Spec.InitContainers {
		c := &job.Spec.Template.Spec.InitContainers[i]
		c.Env = setEnv(c.Env, map[string]string{"KIND": "recommend"})
	}
	for i := range job.Spec.Template.Spec.Containers {
		c := &job.Spec.Template.Spec.Containers[i]
		if c.Name == Container {
			c.Env = setEnv(c.Env, map[string]string{"KIND": "recommend", "HOP_RECOMMENDATION_ID": q.ID, "HOP_RECOMMENDATION_INPUT": string(q.Input), "HOP_RECOMMENDATION_NONCE": q.Nonce})
		}
	}
	_, err = r.cs.BatchV1().Jobs(r.namespace).Create(ctx, job, metav1.CreateOptions{})
	return err
}
