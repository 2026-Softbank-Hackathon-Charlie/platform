package api

import (
	"context"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/airun"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/runs"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

// fakeRuns records starts and answers like airun.Runner.
type fakeRuns struct {
	started []airun.Request
	busy    bool
}

func (f *fakeRuns) Start(_ context.Context, req airun.Request, actor string) (airun.Run, error) {
	if f.busy {
		return airun.Run{}, airun.ErrBusy
	}
	f.started = append(f.started, req)
	return airun.Run{Name: "hopai-" + req.App + "-1", App: req.App, Source: req.Source, Target: req.Target, Status: "running", RequestedBy: actor}, nil
}

func (f *fakeRuns) List(_ context.Context, app string) ([]airun.Run, error) {
	var out []airun.Run
	for i, r := range f.started {
		if app == "" || r.App == app {
			out = append(out, airun.Run{Name: "hopai-" + r.App + "-" + string(rune('1'+i)), App: r.App, Source: r.Source, Target: r.Target, Status: "running"})
		}
	}
	return out, nil
}

func withAIRuns(f AIRunner) option {
	return func(_ *harness, _ *runs.Config, d *Deps) { d.AIRuns = f }
}

func TestAIRunsNotConfigured(t *testing.T) {
	h := newHarness(t, withApps(""))
	if code, b := h.do("POST", "/v1/ai-runs", opToken, apiv1.AIRunRequest{App: "moimroom", Source: "gke", Target: "eks"}); code != 503 || errCode(t, b) != "ai_runner_not_configured" {
		t.Fatalf("start without a runner: %d %s", code, b)
	}
	if code, b := h.do("GET", "/v1/ai-runs", viewerToken, nil); code != 503 {
		t.Fatalf("list without a runner: %d %s", code, b)
	}
}

func TestAIRunsStartAndList(t *testing.T) {
	f := &fakeRuns{}
	h := newHarness(t, withApps(""), withAIRuns(f))
	req := apiv1.AIRunRequest{App: "moimroom", Source: "gke", Target: "eks"}
	if code, _ := h.do("POST", "/v1/ai-runs", agentToken, req); code != 403 {
		t.Fatalf("agent starts a run: %d", code)
	}
	code, b := h.do("POST", "/v1/ai-runs", opToken, req)
	if run := decode[apiv1.AIRun](t, b); code != 202 || run.Status != apiv1.AIRunStatusRunning || run.RequestedBy != "op" || run.App != "moimroom" {
		t.Fatalf("start: %d %s", code, b)
	}
	if len(f.started) != 1 || f.started[0] != (airun.Request{App: "moimroom", Source: "gke", Target: "eks"}) {
		t.Fatalf("started = %+v", f.started)
	}
	code, b = h.do("GET", "/v1/ai-runs?app=moimroom", viewerToken, nil)
	if list := decode[apiv1.AIRunList](t, b); code != 200 || len(list.Runs) != 1 {
		t.Fatalf("list: %d %s", code, b)
	}
	if a := h.lastAudit(); a.Action != "POST /v1/ai-runs" || a.Actor != "op" {
		t.Fatalf("audit = %+v", a)
	}
}

func TestAIRunsRejects(t *testing.T) {
	f := &fakeRuns{}
	h := newHarness(t, withApps(""), withAIRuns(f))
	for _, req := range []apiv1.AIRunRequest{
		{App: "shop", Source: "gke", Target: "eks"},       // not in the apps file
		{App: "moimroom", Source: "gke", Target: "gke"},   // same environment
		{App: "moimroom", Source: "gke", Target: "azure"}, // unknown environment
	} {
		if code, b := h.do("POST", "/v1/ai-runs", opToken, req); code != 400 || errCode(t, b) != "invalid_request" {
			t.Fatalf("%+v: %d %s", req, code, b)
		}
	}
	if len(f.started) != 0 {
		t.Fatalf("started = %+v", f.started)
	}
	f.busy = true
	if code, b := h.do("POST", "/v1/ai-runs", opToken, apiv1.AIRunRequest{App: "moimroom", Source: "gke", Target: "eks"}); code != 409 || errCode(t, b) != "ai_run_active" {
		t.Fatalf("busy: %d %s", code, b)
	}
}

// The real runner behind the API, with a fake cluster: the Job carries the route.
func TestAIRunsWithTheJobRunner(t *testing.T) {
	cs := fake.NewClientset()
	tpl := batchv1.Job{Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: airun.Container, Image: "hopai:test"}}}}}}
	r, err := airun.New(cs, "charlie-platform", tpl)
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, withApps(""), withAIRuns(r))
	if code, b := h.do("POST", "/v1/ai-runs", opToken, apiv1.AIRunRequest{App: "moimroom", Source: "gke", Target: "eks"}); code != 202 {
		t.Fatalf("start: %d %s", code, b)
	}
	jobs, _ := cs.BatchV1().Jobs("charlie-platform").List(context.Background(), metav1.ListOptions{})
	if len(jobs.Items) != 1 || jobs.Items[0].Labels[airun.LabelTarget] != "eks" {
		t.Fatalf("jobs = %+v", jobs.Items)
	}
}
