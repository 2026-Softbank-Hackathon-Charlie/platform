package airun

import (
	"bytes"
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/onboard"
)

func TestStartOnboard(t *testing.T) {
	cs := clientset()
	n := 0
	cs.PrependReactor("create", "configmaps", func(a k8stesting.Action) (bool, runtime.Object, error) {
		cm := a.(k8stesting.CreateAction).GetObject().(*corev1.ConfigMap)
		if cm.Name == "" {
			n++
			cm.Name = fmt.Sprintf("%s%05d", cm.GenerateName, n)
		}
		return false, nil, nil
	})
	r, err := New(cs, "charlie-platform", template())
	if err != nil {
		t.Fatal(err)
	}
	long := bytes.Repeat([]byte("x"), onboard.CheckLogMax+10)
	long[len(long)-1] = 'E'
	run, err := r.StartOnboard(ctx, OnboardRequest{ID: "onb-3f9a1c2b7d4e", App: "shop", Repo: "org/shop", Facts: []byte(`{"version":1}`), CheckLog: long}, "op")
	if err != nil {
		t.Fatal(err)
	}
	job, err := cs.BatchV1().Jobs("charlie-platform").Get(ctx, run.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	c := job.Spec.Template.Spec.Containers[0]
	env := map[string]string{}
	for _, e := range c.Env {
		env[e.Name] = e.Value
	}
	for k, v := range map[string]string{"KIND": "onboard", "ONBOARDING": "onb-3f9a1c2b7d4e", "REPO": "org/shop", "BRANCH": onboard.Branch,
		"FACTS_FILE": "/onboarding/facts.json", "CHECK_LOG_FILE": "/onboarding/check.log", "PLATFORM_URL": "http://api"} {
		if env[k] != v {
			t.Errorf("env %s = %q, want %q", k, env[k], v)
		}
	}
	if len(c.VolumeMounts) != 1 || c.VolumeMounts[0].MountPath != onboard.InputDir {
		t.Fatalf("mounts %+v", c.VolumeMounts)
	}
	cmName := job.Spec.Template.Spec.Volumes[0].ConfigMap.Name
	cm, err := cs.CoreV1().ConfigMaps("charlie-platform").Get(ctx, cmName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if cm.Data["facts.json"] != `{"version":1}` || len(cm.Data["check.log"]) != onboard.CheckLogMax || cm.Data["check.log"][onboard.CheckLogMax-1] != 'E' {
		t.Fatalf("input: facts %q, log %d bytes", cm.Data["facts.json"], len(cm.Data["check.log"]))
	}
	if len(cm.OwnerReferences) != 1 || cm.OwnerReferences[0].Name != job.Name {
		t.Fatalf("owner %+v", cm.OwnerReferences)
	}
	if _, err := r.StartOnboard(ctx, OnboardRequest{ID: "onb-3f9a1c2b7d4e", App: "shop", Repo: "org/shop"}, "op"); err == nil {
		t.Fatal("a second run of a running onboarding started")
	}
	if err := r.StopOnboard(ctx, "onb-3f9a1c2b7d4e"); err != nil {
		t.Fatal(err)
	}
	if runs, _ := r.ListOnboard(ctx, "onb-3f9a1c2b7d4e"); len(runs) != 0 {
		t.Fatalf("after stop: %+v", runs)
	}
	// Without a log the Job gets no CHECK_LOG_FILE.
	run, err = r.StartOnboard(ctx, OnboardRequest{ID: "onb-3f9a1c2b7d4e", App: "shop", Repo: "org/shop", Facts: []byte(`{}`)}, "op")
	if err != nil {
		t.Fatal(err)
	}
	job, _ = cs.BatchV1().Jobs("charlie-platform").Get(ctx, run.Name, metav1.GetOptions{})
	for _, e := range job.Spec.Template.Spec.Containers[0].Env {
		if e.Name == "CHECK_LOG_FILE" {
			t.Fatal("first run got CHECK_LOG_FILE")
		}
	}
}

func TestLastResult(t *testing.T) {
	log := "starting\n{\"kind\":\"onboard\",\"head\":\"old\"}\nmore log\n{\"kind\":\"onboard\",\"head\":\"new\"}\ntrailing stderr\n"
	if got := string(lastResult([]byte(log))); got != `{"kind":"onboard","head":"new"}` {
		t.Fatalf("got %q", got)
	}
	if lastResult([]byte("nothing\n")) != nil {
		t.Fatal("a result from a log without one")
	}
}
