package deploy

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	kubefake "k8s.io/client-go/kubernetes/fake"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/appspec"
)

func TestCheckTemplate(t *testing.T) {
	for _, ok := range []string{
		"http://{app}-{service}.{instance}.svc.cluster.local:{port}",
		"http://{instance}.eks.hopcharlie.internal",
		"https://{instance}.{env}.test/?env={env}",
		"http://10.80.0.7:80",
	} {
		if err := checkTemplate(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for in, want := range map[string]string{
		"{instance}.eks.test":               "http:// or https://",
		"ftp://{instance}.test":             "http:// or https://",
		"http://{instnace}.test":            "placeholder",
		"http://{instance}.test/a b":        "space",
		"http:///{instance}":                "no host",
		"http://{instance}_x.test":          "no host",
		"http://{instance}.Upper.test":      "no host",
		"http://[::1]:80":                   "no host",
		"http://{instance}.test:{port}/%zz": "not a URL",
		"":                                  "http:// or https://",
	} {
		if err := checkTemplate(in); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: error %v, want %q", in, err, want)
		}
	}
}

func TestParseBackendTemplates(t *testing.T) {
	got, err := parseBackendTemplates("eks=http://{instance}.eks.hopcharlie.internal, gke=https://{instance}.gke.hopcharlie.internal/?a=b")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got["eks"] != "http://{instance}.eks.hopcharlie.internal" || got["gke"] != "https://{instance}.gke.hopcharlie.internal/?a=b" {
		t.Fatalf("%v", got)
	}
	for in, want := range map[string]string{
		"http://{instance}.test":                "not <env>=<template>",
		"eks=http://a.test,":                    "ends with a comma",
		"eks=http://a.test,,gke=http://b.test":  "not <env>=<template>",
		"=http://a.test":                        "environment name",
		"EKS=http://a.test":                     "environment name",
		"eks=http://a.test,eks=http://b.test":   "two templates",
		"eks={instance}.eks.test":               "eks: ",
		"eks=http://a.test,gke=http://{x}.test": "gke: ",
	} {
		if _, err := parseBackendTemplates(in); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: error %v, want %q", in, err, want)
		}
	}
}

// An environment in PLATFORM_APP_BACKEND_TEMPLATES gets its template; any
// other uses PLATFORM_APP_BACKEND_TEMPLATE, or reads its Service's load
// balancer when that is empty, so eks can move to the shared entry while gke
// keeps its load balancers. An image without TEMPLATES support ignores the
// new variable, so turning it on and rolling back are both safe.
func TestBackendsFromEnv(t *testing.T) {
	kube := kubefake.NewClientset(&corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "shop-web", Namespace: "shop--dev"},
		Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 80}}},
		Status:     corev1.ServiceStatus{LoadBalancer: corev1.LoadBalancerStatus{Ingress: []corev1.LoadBalancerIngress{{IP: "10.80.0.7"}}}},
	})
	var asked []string
	k := func(env string) (kubernetes.Interface, error) { asked = append(asked, env); return kube, nil }
	svc := appspec.Service{Name: "web", Port: 8080}
	ctx := context.Background()
	url := func(b Backends, env string) string {
		t.Helper()
		u, err := b.URL(ctx, "shop", "shop--dev", env, svc)
		if err != nil {
			t.Fatalf("%s: %v", env, err)
		}
		return u
	}

	b, err := backendsFromEnv("", "eks=http://{instance}.eks.hopcharlie.internal", k)
	if err != nil {
		t.Fatal(err)
	}
	if u := url(b, "eks"); u != "http://shop--dev.eks.hopcharlie.internal" {
		t.Errorf("eks: %s", u)
	}
	if u := url(b, "gke"); u != "http://10.80.0.7" {
		t.Errorf("gke without a template: %s", u)
	}
	if len(asked) != 1 || asked[0] != "gke" {
		t.Errorf("read the Services of %v, want only gke", asked)
	}

	if b, err = backendsFromEnv("http://{app}-{service}.{instance}.svc.cluster.local:{port}", "eks=http://{instance}.eks.hopcharlie.internal", k); err != nil {
		t.Fatal(err)
	}
	if u := url(b, "gke"); u != "http://shop-web.shop--dev.svc.cluster.local:8080" {
		t.Errorf("gke with the all-environment template: %s", u)
	}
	if b, err = backendsFromEnv("http://{instance}.{env}.test", "", k); err != nil {
		t.Fatal(err)
	}
	if u := url(b, "gke"); u != "http://shop--dev.gke.test" {
		t.Errorf("only the all-environment template: %s", u)
	}
	if b, err = backendsFromEnv("", "", k); err != nil {
		t.Fatal(err)
	}
	if _, ok := b.(*ServiceBackends); !ok {
		t.Errorf("neither: %T, want *ServiceBackends", b)
	}
	if _, err := backendsFromEnv("{instance}.test", "", k); err == nil || !strings.Contains(err.Error(), "PLATFORM_APP_BACKEND_TEMPLATE:") {
		t.Errorf("bad all-environment template: %v", err)
	}
	if _, err := backendsFromEnv("", "eks=http://a.test,", k); err == nil || !strings.Contains(err.Error(), "PLATFORM_APP_BACKEND_TEMPLATES:") {
		t.Errorf("bad list: %v", err)
	}
}

func TestWarnUnknownEnvs(t *testing.T) {
	var out strings.Builder
	log := slog.New(slog.NewTextHandler(&out, nil))
	b, err := backendsFromEnv("", "eks=http://{instance}.eks.test,ekss=http://{instance}.ekss.test", nil)
	if err != nil {
		t.Fatal(err)
	}
	warnUnknownEnvs(b, []string{"eks", "gke"}, log)
	if !strings.Contains(out.String(), "env=ekss") || strings.Contains(out.String(), "env=eks ") || strings.Count(out.String(), "\n") != 1 {
		t.Errorf("log: %q", out.String())
	}
	out.Reset()
	warnUnknownEnvs(TemplateBackends{Template: "http://{instance}.test"}, []string{"eks"}, log)
	if out.Len() != 0 {
		t.Errorf("the all-environment template logged %q", out.String())
	}
}
