package deploy

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	kubefake "k8s.io/client-go/kubernetes/fake"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/appspec"
)

func TestParseBackendTemplates(t *testing.T) {
	for _, tc := range []struct {
		name, in, all, err string
		perEnv             map[string]string
	}{
		{name: "one template for every environment", in: "http://{app}-{service}.{instance}.svc.cluster.local:{port}", all: "http://{app}-{service}.{instance}.svc.cluster.local:{port}"},
		{name: "a plain template with = in its query", in: "http://{instance}.test/?env={env}", all: "http://{instance}.test/?env={env}"},
		{name: "one environment", in: "eks=http://{instance}.eks.hopcharlie.internal", perEnv: map[string]string{"eks": "http://{instance}.eks.hopcharlie.internal"}},
		{name: "two environments with spaces", in: "eks=http://{instance}.eks.hopcharlie.internal, gke=https://{instance}.gke.hopcharlie.internal", perEnv: map[string]string{
			"eks": "http://{instance}.eks.hopcharlie.internal", "gke": "https://{instance}.gke.hopcharlie.internal"}},
		{name: "a plain entry after an environment", in: "eks=http://{instance}.eks.test,http://{instance}.test", err: "not <env>=<template>"},
		{name: "an environment twice", in: "eks=http://a.test,eks=http://b.test", err: "two templates"},
		{name: "no scheme", in: "eks={instance}.eks.test", err: "http:// or https://"},
		{name: "empty template", in: "eks=", err: "http:// or https://"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			all, perEnv, err := ParseBackendTemplates(tc.in)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("error %v, want %q", err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if all != tc.all || len(perEnv) != len(tc.perEnv) {
				t.Fatalf("got %q %v, want %q %v", all, perEnv, tc.all, tc.perEnv)
			}
			for env, want := range tc.perEnv {
				if perEnv[env] != want {
					t.Errorf("%s: %q, want %q", env, perEnv[env], want)
				}
			}
		})
	}
}

// An environment with a template gets it; one without reads its Service's
// load balancer as before, so eks can move to the shared entry while gke
// keeps its load balancers.
func TestBackendsFromEnvPerEnvironment(t *testing.T) {
	kube := kubefake.NewClientset(&corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "shop-web", Namespace: "shop--dev"},
		Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 80}}},
		Status:     corev1.ServiceStatus{LoadBalancer: corev1.LoadBalancerStatus{Ingress: []corev1.LoadBalancerIngress{{IP: "10.80.0.7"}}}},
	})
	var asked []string
	k := func(env string) (kubernetes.Interface, error) { asked = append(asked, env); return kube, nil }
	svc := appspec.Service{Name: "web", Port: 8080}
	ctx := context.Background()

	b, err := backendsFromEnv("eks=http://{instance}.eks.hopcharlie.internal", k)
	if err != nil {
		t.Fatal(err)
	}
	if u, err := b.URL(ctx, "shop", "shop--dev", "eks", svc); err != nil || u != "http://shop--dev.eks.hopcharlie.internal" {
		t.Errorf("eks: %q %v", u, err)
	}
	if u, err := b.URL(ctx, "shop", "shop--dev", "gke", svc); err != nil || u != "http://10.80.0.7" {
		t.Errorf("gke: %q %v", u, err)
	}
	if len(asked) != 1 || asked[0] != "gke" {
		t.Errorf("read the Services of %v, want only gke", asked)
	}

	// The plain form still serves every environment.
	if b, err = backendsFromEnv("http://{instance}.{env}.test", k); err != nil {
		t.Fatal(err)
	}
	if u, _ := b.URL(ctx, "shop", "shop", "gke", svc); u != "http://shop.gke.test" {
		t.Errorf("plain: %q", u)
	}
	if b, err = backendsFromEnv("", k); err != nil {
		t.Fatal(err)
	}
	if _, ok := b.(*ServiceBackends); !ok {
		t.Errorf("empty: %T, want *ServiceBackends", b)
	}
	if _, err := backendsFromEnv("eks=eks.test", k); err == nil {
		t.Error("a template without a scheme was accepted")
	}
}
