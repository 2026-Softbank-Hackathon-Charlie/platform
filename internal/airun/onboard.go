package airun

import (
	"context"
	"fmt"
	"slices"
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

// InputConfigMap is the name of an onboarding's input ConfigMap,
// hopai-<onboarding id>: the only names the platform's RBAC lets it create
// and delete (infra #149).
func InputConfigMap(id string) string { return "hopai-" + id }

// inputVolume is the template's volume of the input, mounted at
// onboard.InputDir; StartOnboard adds both when the template has none.
const inputVolume = "onboarding"

// StartOnboard creates the Job, whose volume onboarding is the ConfigMap
// InputConfigMap(id) (not optional: the pod waits for it), then that
// ConfigMap with facts.json and, on a retry, check.log, owned by the Job so
// it goes with it. One run of an onboarding at a time.
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
	cmName := InputConfigMap(req.ID)
	job := r.template.DeepCopy()
	job.ObjectMeta = metav1.ObjectMeta{
		GenerateName: "hopai-" + req.ID + "-",
		Namespace:    r.namespace,
		Labels:       merge(r.template.Labels, labels),
		Annotations:  merge(r.template.Annotations, map[string]string{AnnotRequestedBy: actor}),
	}
	ps := &job.Spec.Template.Spec
	job.Spec.Template.Labels = merge(job.Spec.Template.Labels, labels)
	no := false
	source := corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: cmName}, Optional: &no}}
	found := false
	for i := range ps.Volumes {
		if ps.Volumes[i].Name == inputVolume {
			ps.Volumes[i].VolumeSource, found = source, true
		}
	}
	if !found {
		ps.Volumes = append(ps.Volumes, corev1.Volume{Name: inputVolume, VolumeSource: source})
	}
	for i := range ps.Containers {
		c := &ps.Containers[i]
		if c.Name != Container {
			continue
		}
		c.Env = setEnv(c.Env, env)
		if !slices.ContainsFunc(c.VolumeMounts, func(m corev1.VolumeMount) bool { return m.Name == inputVolume }) {
			c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{Name: inputVolume, MountPath: onboard.InputDir, ReadOnly: true})
		}
	}
	created, err := r.cs.BatchV1().Jobs(r.namespace).Create(ctx, job, metav1.CreateOptions{})
	if err != nil {
		return Run{}, fmt.Errorf("airun: create Job: %w", err)
	}
	yes := true
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: cmName, Namespace: r.namespace, Labels: labels,
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: created.Name, UID: created.UID, BlockOwnerDeletion: &yes}}},
		Data: data,
	}
	cms := r.cs.CoreV1().ConfigMaps(r.namespace)
	// The ConfigMap of an earlier run whose Job is not cleaned up yet.
	if err := cms.Delete(ctx, cmName, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return runOf(created), fmt.Errorf("airun: replace the onboarding input: %w", err)
	}
	if _, err := cms.Create(ctx, cm, metav1.CreateOptions{}); err != nil {
		bg := metav1.DeletePropagationBackground
		_ = r.cs.BatchV1().Jobs(r.namespace).Delete(ctx, created.Name, metav1.DeleteOptions{PropagationPolicy: &bg})
		return Run{}, fmt.Errorf("airun: create the onboarding input ConfigMap: %w", err)
	}
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
	if err := r.cs.CoreV1().ConfigMaps(r.namespace).Delete(ctx, InputConfigMap(id), metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("airun: delete the onboarding input: %w", err)
	}
	return nil
}


// OnboardOutcome is how an onboarding's hopai Job ended (ai-sandbox hopai
// onboard): its container's exit code (0 done, 1 failed, 2 usage, 3 no
// contract can be added, or the default branch has platform.yaml already)
// and its result, the last log line with "kind":"onboard" (nil when there
// is none).
type OnboardOutcome struct {
	ExitCode int
	Result   []byte
	// Log is the end of the Job's log, for the events.
	Log string
}

// onboardLogTail is how much of the pod's log OnboardOutcome reads.
const onboardLogTail = 256 << 10

// Outcome reads the pod of a finished Job: its exit code and log. It needs
// get and list of pods and get of pods/log in the namespace.
func (r *Runner) Outcome(ctx context.Context, job string) (OnboardOutcome, error) {
	pods, err := r.cs.CoreV1().Pods(r.namespace).List(ctx, metav1.ListOptions{LabelSelector: "job-name=" + job})
	if err != nil {
		return OnboardOutcome{}, fmt.Errorf("airun: list the pods of %s: %w", job, err)
	}
	if len(pods.Items) == 0 {
		return OnboardOutcome{}, fmt.Errorf("airun: Job %s has no pod", job)
	}
	pod := pods.Items[len(pods.Items)-1] // the last attempt
	out := OnboardOutcome{ExitCode: -1}
	for _, s := range pod.Status.ContainerStatuses {
		if s.Name == Container && s.State.Terminated != nil {
			out.ExitCode = int(s.State.Terminated.ExitCode)
		}
	}
	limit := int64(onboardLogTail)
	raw, err := r.cs.CoreV1().Pods(r.namespace).GetLogs(pod.Name, &corev1.PodLogOptions{Container: Container, LimitBytes: &limit}).DoRaw(ctx)
	if err != nil {
		return out, fmt.Errorf("airun: read the log of %s: %w", pod.Name, err)
	}
	out.Log = string(raw)
	out.Result = lastResult(raw)
	return out, nil
}

// lastResult is the last line of log that holds "kind":"onboard".
func lastResult(log []byte) []byte {
	lines := strings.Split(string(log), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if strings.HasPrefix(l, "{") && (strings.Contains(l, `"kind":"onboard"`) || strings.Contains(l, `"kind": "onboard"`)) {
			return []byte(l)
		}
	}
	return nil
}
