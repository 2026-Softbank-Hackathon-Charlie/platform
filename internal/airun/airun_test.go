package airun

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

var ctx = context.Background()

// clientset is a fake that names created Jobs from generateName, as the API server does.
func clientset() *fake.Clientset {
	cs := fake.NewClientset()
	n := 0
	cs.PrependReactor("create", "jobs", func(a k8stesting.Action) (bool, runtime.Object, error) {
		j := a.(k8stesting.CreateAction).GetObject().(*batchv1.Job)
		if j.Name == "" {
			n++
			j.Name = fmt.Sprintf("%s%05d", j.GenerateName, n)
		}
		return false, nil, nil
	})
	return cs
}

func template() batchv1.Job {
	return batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app.kubernetes.io/component": "hopai"}},
		Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: Container, Image: "hopai:test",
			Env: []corev1.EnvVar{{Name: "APP", Value: "template-default"}, {Name: "PLATFORM_URL", Value: "http://api"}},
		}}}}},
	}
}

func TestStartCreatesTheJob(t *testing.T) {
	cs := clientset()
	r, err := New(cs, "charlie-platform", template())
	if err != nil {
		t.Fatal(err)
	}
	run, err := r.Start(ctx, Request{App: "moimroom", Source: "gke", Target: "eks"}, "seohyun")
	if err != nil {
		t.Fatal(err)
	}
	if run.App != "moimroom" || run.Source != "gke" || run.Target != "eks" || run.Status != "running" || run.RequestedBy != "seohyun" {
		t.Fatalf("run = %+v", run)
	}
	jobs, _ := cs.BatchV1().Jobs("charlie-platform").List(ctx, metav1.ListOptions{})
	if len(jobs.Items) != 1 {
		t.Fatalf("jobs = %d", len(jobs.Items))
	}
	j := jobs.Items[0]
	if j.GenerateName != "hopai-moimroom-" || j.Labels["app.kubernetes.io/component"] != "hopai" || j.Labels[LabelRun] != "true" {
		t.Fatalf("job meta = %+v", j.ObjectMeta)
	}
	env := map[string]string{}
	for _, e := range j.Spec.Template.Spec.Containers[0].Env {
		if _, dup := env[e.Name]; dup {
			t.Fatalf("duplicate env %s", e.Name)
		}
		env[e.Name] = e.Value
	}
	if env["APP"] != "moimroom" || env["FROM"] != "gke" || env["TO"] != "eks" || env["PLATFORM_URL"] != "http://api" {
		t.Fatalf("env = %v", env)
	}
}

func TestStartOneRunPerAppAtATime(t *testing.T) {
	cs := clientset()
	r, _ := New(cs, "ns", template())
	if _, err := r.Start(ctx, Request{App: "moimroom", Source: "gke", Target: "eks"}, "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Start(ctx, Request{App: "moimroom", Source: "eks", Target: "gke"}, "b"); !errors.Is(err, ErrBusy) {
		t.Fatalf("second run while the first runs: %v", err)
	}
	if _, err := r.Start(ctx, Request{App: "other", Source: "gke", Target: "eks"}, "b"); err != nil {
		t.Fatalf("another app: %v", err)
	}
	// once the first finishes, the app can run again
	jobs, _ := cs.BatchV1().Jobs("ns").List(ctx, metav1.ListOptions{LabelSelector: LabelApp + "=moimroom"})
	j := jobs.Items[0]
	j.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, LastTransitionTime: metav1.NewTime(time.Now())}}
	if _, err := cs.BatchV1().Jobs("ns").UpdateStatus(ctx, &j, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	runs, err := r.List(ctx, "moimroom")
	if err != nil || len(runs) != 1 || runs[0].Status != "failed" || runs[0].FinishedAt == nil {
		t.Fatalf("runs = %+v, %v", runs, err)
	}
	if _, err := r.Start(ctx, Request{App: "moimroom", Source: "gke", Target: "eks"}, "c"); err != nil {
		t.Fatalf("after the first failed: %v", err)
	}
}

func TestStartRejects(t *testing.T) {
	r, _ := New(fake.NewClientset(), "ns", template())
	for _, req := range []Request{{"Moim", "gke", "eks"}, {"moimroom", "gke", "gke"}, {"moimroom", "", "eks"}, {"moimroom", "gke", "eks;rm"}} {
		if _, err := r.Start(ctx, req, "x"); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%+v: %v", req, err)
		}
	}
}

func TestNewNeedsTheHopaiContainer(t *testing.T) {
	tpl := template()
	tpl.Spec.Template.Spec.Containers[0].Name = "other"
	if _, err := New(fake.NewClientset(), "ns", tpl); err == nil {
		t.Fatal("want an error without a hopai container")
	}
}

func TestFromEnvWithoutTemplateIsOff(t *testing.T) {
	r, err := FromEnv(func(string) string { return "" })
	if r != nil || err != nil {
		t.Fatalf("FromEnv = %v, %v", r, err)
	}
}

// A suspended Job does not hold the app forever.
func TestSuspendedJobDoesNotBlock(t *testing.T) {
	cs := clientset()
	r, _ := New(cs, "ns", template())
	if _, err := r.Start(ctx, Request{App: "moimroom", Source: "gke", Target: "eks"}, "a"); err != nil {
		t.Fatal(err)
	}
	jobs, _ := cs.BatchV1().Jobs("ns").List(ctx, metav1.ListOptions{})
	j := jobs.Items[0]
	yes := true
	j.Spec.Suspend = &yes
	if _, err := cs.BatchV1().Jobs("ns").Update(ctx, &j, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Start(ctx, Request{App: "moimroom", Source: "gke", Target: "eks"}, "b"); err != nil {
		t.Fatalf("after the first was suspended: %v", err)
	}
}

// Concurrent starts for one app create one Job.
func TestConcurrentStartsCreateOneJob(t *testing.T) {
	cs := clientset()
	r, _ := New(cs, "ns", template())
	errs := make(chan error, 8)
	for range 8 {
		go func() {
			_, err := r.Start(ctx, Request{App: "moimroom", Source: "gke", Target: "eks"}, "x")
			errs <- err
		}()
	}
	ok := 0
	for range 8 {
		if err := <-errs; err == nil {
			ok++
		} else if !errors.Is(err, ErrBusy) {
			t.Fatal(err)
		}
	}
	jobs, _ := cs.BatchV1().Jobs("ns").List(ctx, metav1.ListOptions{})
	if ok != 1 || len(jobs.Items) != 1 {
		t.Fatalf("started %d, jobs %d", ok, len(jobs.Items))
	}
}
