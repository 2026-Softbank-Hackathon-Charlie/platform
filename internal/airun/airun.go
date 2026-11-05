// Package airun starts the AI runner (ai-sandbox hopai: AI plan, sandbox
// rehearsal, diagnosis) as a Kubernetes Job next to the platform, so the
// console can run it without anyone installing hopai. The Job is a template
// the deployment mounts (infra hopcharlie-platform chart); the platform only
// names the app and the route. The runner talks back through the normal API
// (plans, attachments, agent sessions) with its own agent token.
package airun

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/yaml"
)

// Labels and annotations on the Jobs this package creates. The label also
// selects them, so the template cannot hide a Job from List or the
// one-at-a-time check.
const (
	LabelRun         = "hopcharlie.dev/ai-run"
	LabelApp         = "hopcharlie.dev/ai-run-app"
	LabelSource      = "hopcharlie.dev/ai-run-source"
	LabelTarget      = "hopcharlie.dev/ai-run-target"
	AnnotRequestedBy = "hopcharlie.dev/requested-by"
	// Container is the name of the template's container that runs hopai. It
	// gets APP, FROM and TO in its environment.
	Container = "hopai"
)

var (
	// ErrBusy means a run for the app has not finished yet.
	ErrBusy = errors.New("an AI run for this app is still running")
	// ErrInvalid means the request names something that is not a valid name.
	ErrInvalid = errors.New("invalid AI run request")
)

var name = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

// Request is one run: plan the move of App from Source to Target and
// rehearse it.
type Request struct {
	App, Source, Target string
}

// Run is a Job this package started.
type Run struct {
	Name        string
	App         string
	Source      string
	Target      string
	Status      string // running, succeeded, failed
	RequestedBy string
	CreatedAt   time.Time
	FinishedAt  *time.Time
}

// Runner creates and lists AI runs in one namespace.
type Runner struct {
	cs        kubernetes.Interface
	namespace string
	template  batchv1.Job
}

// New checks the template: a Job with a container named hopai.
func New(cs kubernetes.Interface, namespace string, template batchv1.Job) (*Runner, error) {
	if namespace == "" {
		return nil, errors.New("airun: namespace is required")
	}
	found := false
	for _, c := range template.Spec.Template.Spec.Containers {
		if c.Name == Container {
			found = true
		}
	}
	if !found {
		return nil, fmt.Errorf("airun: the Job template has no container named %q", Container)
	}
	return &Runner{cs: cs, namespace: namespace, template: template}, nil
}

// FromEnv builds the Runner from PLATFORM_AIRUN_JOB_TEMPLATE (a Job YAML
// file) with the in-cluster client, in the namespace the pod runs in unless
// PLATFORM_AIRUN_NAMESPACE says otherwise. It returns nil when the template
// is not set: the AI runner is then started by hand (hopai run).
func FromEnv(getenv func(string) string) (*Runner, error) {
	path := getenv("PLATFORM_AIRUN_JOB_TEMPLATE")
	if path == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("airun: read the Job template: %w", err)
	}
	var job batchv1.Job
	if err := yaml.UnmarshalStrict(raw, &job); err != nil {
		return nil, fmt.Errorf("airun: parse the Job template %s: %w", path, err)
	}
	ns := getenv("PLATFORM_AIRUN_NAMESPACE")
	if ns == "" {
		b, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
		if err != nil {
			return nil, fmt.Errorf("airun: set PLATFORM_AIRUN_NAMESPACE outside a cluster: %w", err)
		}
		ns = strings.TrimSpace(string(b))
	}
	rc, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("airun: in-cluster configuration: %w", err)
	}
	cs, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return nil, err
	}
	return New(cs, ns, job)
}

// Start creates the Job for req unless a run for the same app has not
// finished. actor is recorded on the Job.
func (r *Runner) Start(ctx context.Context, req Request, actor string) (Run, error) {
	for _, v := range []string{req.App, req.Source, req.Target} {
		if !name.MatchString(v) {
			return Run{}, fmt.Errorf("%w: %q is not a name", ErrInvalid, v)
		}
	}
	if req.Source == req.Target {
		return Run{}, fmt.Errorf("%w: source and target are both %q", ErrInvalid, req.Source)
	}
	runs, err := r.List(ctx, req.App)
	if err != nil {
		return Run{}, err
	}
	for _, x := range runs {
		if x.Status == "running" {
			return Run{}, fmt.Errorf("%w: %s (%s -> %s)", ErrBusy, x.Name, x.Source, x.Target)
		}
	}
	job := r.template.DeepCopy()
	job.ObjectMeta = metav1.ObjectMeta{
		GenerateName: "hopai-" + truncate(req.App, 40) + "-",
		Namespace:    r.namespace,
		Labels:       merge(r.template.Labels, map[string]string{LabelRun: "true", LabelApp: req.App, LabelSource: req.Source, LabelTarget: req.Target}),
		Annotations:  merge(r.template.Annotations, map[string]string{AnnotRequestedBy: actor}),
	}
	job.Spec.Template.Labels = merge(job.Spec.Template.Labels, map[string]string{LabelRun: "true", LabelApp: req.App})
	for i := range job.Spec.Template.Spec.Containers {
		c := &job.Spec.Template.Spec.Containers[i]
		if c.Name == Container {
			c.Env = setEnv(c.Env, map[string]string{"APP": req.App, "FROM": req.Source, "TO": req.Target})
		}
	}
	created, err := r.cs.BatchV1().Jobs(r.namespace).Create(ctx, job, metav1.CreateOptions{})
	if err != nil {
		return Run{}, fmt.Errorf("airun: create Job: %w", err)
	}
	return runOf(created), nil
}

// List returns the runs, newest first, for app or for every app when app is
// empty. Jobs the cluster has cleaned up (ttlSecondsAfterFinished) are gone.
func (r *Runner) List(ctx context.Context, app string) ([]Run, error) {
	sel := LabelRun + "=true"
	if app != "" {
		if !name.MatchString(app) {
			return nil, fmt.Errorf("%w: %q is not a name", ErrInvalid, app)
		}
		sel += "," + LabelApp + "=" + app
	}
	list, err := r.cs.BatchV1().Jobs(r.namespace).List(ctx, metav1.ListOptions{LabelSelector: sel})
	if err != nil {
		return nil, fmt.Errorf("airun: list Jobs: %w", err)
	}
	out := make([]Run, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, runOf(&list.Items[i]))
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].Name > out[j].Name
	})
	return out, nil
}

func runOf(j *batchv1.Job) Run {
	run := Run{
		Name: j.Name, App: j.Labels[LabelApp], Source: j.Labels[LabelSource], Target: j.Labels[LabelTarget],
		RequestedBy: j.Annotations[AnnotRequestedBy], CreatedAt: j.CreationTimestamp.UTC(), Status: "running",
	}
	for _, c := range j.Status.Conditions {
		if c.Status != corev1.ConditionTrue {
			continue
		}
		switch c.Type {
		case batchv1.JobComplete:
			run.Status = "succeeded"
		case batchv1.JobFailed:
			run.Status = "failed"
		default:
			continue
		}
		t := c.LastTransitionTime.UTC()
		run.FinishedAt = &t
	}
	if j.Status.CompletionTime != nil {
		t := j.Status.CompletionTime.UTC()
		run.FinishedAt = &t
	}
	return run
}

func merge(a, b map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

func setEnv(env []corev1.EnvVar, set map[string]string) []corev1.EnvVar {
	out := make([]corev1.EnvVar, 0, len(env)+len(set))
	for _, e := range env {
		if _, ok := set[e.Name]; !ok {
			out = append(out, e)
		}
	}
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out = append(out, corev1.EnvVar{Name: k, Value: set[k]})
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.TrimRight(s[:n], "-")
}
