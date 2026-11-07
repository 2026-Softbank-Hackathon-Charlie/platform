package probes

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/preflight/parity"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func TestServiceUsageFetcherScopeAndBodyLimit(t *testing.T) {
	large := false
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/api/v1/namespaces/shop/services/http:shop-web:8080/proxy/internal/secret-usage" || r.URL.Query().Get("challenge") != "nonce" {
			t.Errorf("unexpected request %s", r.URL)
		}
		if large {
			w.Write([]byte(strings.Repeat("x", 8193)))
		} else {
			w.Write([]byte("report"))
		}
	}))
	defer s.Close()
	k, err := kubernetes.NewForConfig(&rest.Config{Host: s.URL})
	if err != nil {
		t.Fatal(err)
	}
	f := ServiceUsageFetcher(func(env string) (rest.Interface, error) {
		if env != "eks" {
			t.Fatal("wrong environment")
		}
		return k.CoreV1().RESTClient(), nil
	})
	target := parity.Target{Environment: "eks", Namespace: "shop", Application: "shop"}
	if raw, err := f(context.Background(), target, SecretUsage{Service: "shop-web", Port: 8080}, "nonce"); err != nil || string(raw) != "report" {
		t.Fatalf("fetch: %v", err)
	}
	large = true
	if _, err := f(context.Background(), target, SecretUsage{Service: "shop-web", Port: 8080}, "nonce"); err == nil {
		t.Fatal("oversized body accepted")
	}
}
