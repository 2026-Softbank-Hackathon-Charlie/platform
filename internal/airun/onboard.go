package airun

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/onboard"
)

// Labels of an onboarding's hopai Job (kind onboard, ADR 0042).
const (
	LabelKind       = "hopcharlie.dev/ai-run-kind"
	LabelOnboarding = "hopcharlie.dev/onboarding"
	// KindOnboard is the KIND the onboarding Job's hopai gets.
	KindOnboard = "onboard"
	// inputVolume is the Job's volume of its input ConfigMap.
	inputVolume = "onboarding-input"
)

// OnboardRequest is one hopai run of an onboarding's patching or checking:
// write the contract of Repo on onboard.Branch from Facts, and on a retry
// fix it with the failed test deployment's log.
type OnboardRequest struct {
	ID    string // onb-<hex>
	App   string // the onboarded app
	Repo  string // owner/name
	Facts []byte // pkg/onboard/facts.Facts JSON
	// CheckLog is the failed test deployment's log, nil on the first run;
	// only its last onboard.CheckLogMax bytes go to the Job.
	CheckLog []byte
}

// StartOnboard creates the onboarding's input ConfigMap (facts.json and,
// on a retry, check.log), then the Job that mounts it at onboard.InputDir,
// and makes the Job the ConfigMap's owner so it goes with the Job. One run
// of an onboarding at a time.
func (r *Runner) StartOnboard(ctx context.Context, req OnboardRequest, actor string) (Run, error) {
	if !onboard.ID.MatchString(req.ID) || !name.MatchString(req.App) || strings.Count(req.Repo, "/") != 1 {
		return Run{}, fmt.Errorf("%w: onboarding %q of %q", ErrInvalid, req.ID, req.Repo)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	runs, err := r.ListOnboard(ctx, req.ID)
	if err != nil {
		return Run{}, err
	}
	for _, x := range runs {
		if x.Status == "running" {
			return Run{}, fmt.Errorf("%w: %s", ErrBusy, x.Name)
		}
	}
	labels := map[string]string{LabelRun: "true", LabelApp: req.App, LabelKind: KindOnboard, LabelOnboarding: req.ID}
	data := map[string]string{"facts.json": string(req.Facts)}
	env := map[string]string{"KIND": KindOnboard, "ONBOARDING": req.ID, "APP": req.App, "REPO": req.Repo,
		"BRANCH": onboard.Branch, "FACTS_FILE": onboard.FactsFile}
	if req.CheckLog != nil {
		log := req.CheckLog
		if len(log) > onboard.CheckLogMax {
			log = log[len(log)-onboard.CheckLogMax:]
		}
		data["check.log"] = string(log)
		env["CHECK_LOG_FILE"] = onboard.CheckLogFile
	}
	cm, err := r.cs.CoreV1().ConfigMaps(r.namespace).Create(ctx, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{GenerateName: req.ID + "-input-", Namespace: r.namespace, Labels: labels},
		Data:       data,
	}, metav1.CreateOptions{})
	if err != nil {
		return Run{}, fmt.Errorf("airun: create the onboarding input ConfigMap: %w", err)
	}
	job := r.template.DeepCopy()
	job.ObjectMeta = metav1.ObjectMeta{
		GenerateName: "hopai-" + req.ID + "-",
		Namespace:    r.namespace,
		Labels:       merge(r.template.Labels, labels),
		Annotations:  merge(r.template.Annotations, map[string]string{AnnotRequestedBy: actor}),
	}
	ps := &job.Spec.Template.Spec
	job.Spec.Template.Labels = merge(job.Spec.Template.Labels, labels)
	ps.Volumes = append(ps.Volumes, corev1.Volume{Name: inputVolume,
		VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: cm.Name}}}})
	for i := range ps.Containers {
		c := &ps.Containers[i]
		if c.Name == Container {
			c.Env = setEnv(c.Env, env)
			c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{Name: inputVolume, MountPath: onboard.InputDir, ReadOnly: true})
		}
	}
	created, err := r.cs.BatchV1().Jobs(r.namespace).Create(ctx, job, metav1.CreateOptions{})
	if err != nil {
		_ = r.cs.CoreV1().ConfigMaps(r.namespace).Delete(ctx, cm.Name, metav1.DeleteOptions{})
		return Run{}, fmt.Errorf("airun: create Job: %w", err)
	}
	// Owned by the Job, the ConfigMap goes when the cluster cleans the Job
	// up. Best effort: without it StopOnboard still deletes it.
	yes := true
	cm.OwnerReferences = []metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: created.Name, UID: created.UID, BlockOwnerDeletion: &yes}}
	_, _ = r.cs.CoreV1().ConfigMaps(r.namespace).Update(ctx, cm, metav1.UpdateOptions{})
	return runOf(created), nil
}

// ListOnboard returns an onboarding's hopai runs, newest first.
func (r *Runner) ListOnboard(ctx context.Context, id string) ([]Run, error) {
	if !onboard.ID.MatchString(id) {
		return nil, fmt.Errorf("%w: %q is not an onboarding id", ErrInvalid, id)
	}
	list, err := r.cs.BatchV1().Jobs(r.namespace).List(ctx, metav1.ListOptions{LabelSelector: LabelRun + "=true," + LabelOnboarding + "=" + id})
	if err != nil {
		return nil, fmt.Errorf("airun: list Jobs: %w", err)
	}
	out := make([]Run, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, runOf(&list.Items[i]))
	}
	sortRuns(out)
	return out, nil
}

// StopOnboard deletes an onboarding's Jobs (their pods with them) and
// input ConfigMaps. It needs the delete RBAC of both in the namespace
// (ADR 0042 addendum); without it the error says so and the Jobs end at
// their activeDeadlineSeconds.
func (r *Runner) StopOnboard(ctx context.Context, id string) error {
	if !onboard.ID.MatchString(id) {
		return fmt.Errorf("%w: %q is not an onboarding id", ErrInvalid, id)
	}
	sel := metav1.ListOptions{LabelSelector: LabelOnboarding + "=" + id}
	bg := metav1.DeletePropagationBackground
	jobs, err := r.cs.BatchV1().Jobs(r.namespace).List(ctx, sel)
	if err != nil {
		return fmt.Errorf("airun: list the onboarding's Jobs: %w", err)
	}
	for _, j := range jobs.Items {
		if err := r.cs.BatchV1().Jobs(r.namespace).Delete(ctx, j.Name, metav1.DeleteOptions{PropagationPolicy: &bg}); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("airun: delete Job %s: %w", j.Name, err)
		}
	}
	cms, err := r.cs.CoreV1().ConfigMaps(r.namespace).List(ctx, sel)
	if err != nil {
		return fmt.Errorf("airun: list the onboarding's input: %w", err)
	}
	for _, cm := range cms.Items {
		if err := r.cs.CoreV1().ConfigMaps(r.namespace).Delete(ctx, cm.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("airun: delete ConfigMap %s: %w", cm.Name, err)
		}
	}
	return nil
}

