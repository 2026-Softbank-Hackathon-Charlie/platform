package envs

import (
	"context"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"k8s.io/client-go/rest"
)

func TestConnectionOwnershipAndRefresh(t *testing.T) {
	s, err := New(Config{Environments: []EnvConfig{{Name: "static", Provider: "onprem", KubeContext: "static"}}}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	rc := &rest.Config{Host: "https://cluster.example:6443", BearerToken: "token-one", TLSClientConfig: rest.TLSClientConfig{CAData: []byte("test-ca"), ServerName: "cluster.example"}}
	if err = s.RegisterConnection("static", "owner", "uid", rc); !errors.Is(err, ErrConnectionConflict) {
		t.Fatal("static environment overwritten", err)
	}
	if err = s.RegisterConnection("edge", "owner", "uid", rc); err != nil {
		t.Fatal(err)
	}
	if err = s.RegisterConnection("edge", "other", "uid", rc); !errors.Is(err, ErrConnectionConflict) {
		t.Fatal("different owner accepted", err)
	}
	if err = s.RegisterConnection("edge", "owner", "other", rc); !errors.Is(err, ErrConnectionConflict) {
		t.Fatal("different UID accepted", err)
	}
	rc.BearerToken = "token-two"
	if err = s.RegisterConnection("edge", "owner", "uid", rc); err != nil {
		t.Fatal(err)
	}
	cfg, ok := s.Config("edge")
	if !ok || cfg.RESTConfig.BearerToken != "token-two" {
		t.Fatal("token refresh absent")
	}
	cfg.RESTConfig.CAData[0] = 'x'
	again, _ := s.Config("edge")
	if string(again.RESTConfig.CAData) != "test-ca" {
		t.Fatal("caller mutated registry credentials")
	}
	if _, err = s.Storage("edge"); !errors.Is(err, ErrStorageNotConfigured) {
		t.Fatal("invented storage", err)
	}
	if _, err = s.Sealer("edge"); !errors.Is(err, ErrSealingNotConfigured) {
		t.Fatal("invented sealing", err)
	}
	if err = s.RemoveConnection("edge", "other"); !errors.Is(err, ErrConnectionConflict) {
		t.Fatal("other owner removed environment", err)
	}
	if err = s.RemoveConnection("edge", "owner"); err != nil {
		t.Fatal(err)
	}
	if _, ok = s.Config("edge"); ok {
		t.Fatal("environment not removed")
	}
}

func TestConnectionRegistryConcurrentSnapshots(t *testing.T) {
	s, err := New(Config{Environments: []EnvConfig{{Name: "static", Provider: "onprem", KubeContext: "static"}}}, Options{NewProber: func(EnvConfig) (Prober, error) { return &fakeProber{}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	rc := &rest.Config{Host: "https://cluster.example:6443", BearerToken: "token", TLSClientConfig: rest.TLSClientConfig{CAData: []byte("ca"), ServerName: "cluster.example"}}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 20 {
				if err := s.RegisterConnection("edge", "owner", "uid", rc); err != nil {
					t.Error(err)
				}
				s.Configs()
				s.Names()
				s.Config("edge")
				s.Storage("edge")
				s.Sealer("edge")
				s.List(context.Background())
				s.Get(context.Background(), "edge")
			}
		}()
	}
	wg.Wait()
	if len(s.Names()) != 2 {
		t.Fatal("duplicate registry entries")
	}
}

func TestConnectionRefreshEvictsProbeCache(t *testing.T) {
	s, err := New(Config{Environments: []EnvConfig{{Name: "static", Provider: "onprem", KubeContext: "static"}}}, Options{CacheTTL: time.Hour, NewProber: func(c EnvConfig) (Prober, error) {
		if c.RESTConfig == nil {
			return &fakeProber{version: "static"}, nil
		}
		return &fakeProber{version: c.RESTConfig.BearerToken}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	rc := &rest.Config{Host: "https://cluster.example:6443", BearerToken: "token-one", TLSClientConfig: rest.TLSClientConfig{CAData: []byte("ca")}}
	if err = s.RegisterConnection("edge", "owner", "uid", rc); err != nil {
		t.Fatal(err)
	}
	first, err := s.Get(context.Background(), "edge")
	if err != nil || first.Status.KubernetesVersion == nil || *first.Status.KubernetesVersion != "token-one" {
		t.Fatal("initial probe", err)
	}
	rc.BearerToken = "token-two"
	if err = s.RegisterConnection("edge", "owner", "uid", rc); err != nil {
		t.Fatal(err)
	}
	second, err := s.Get(context.Background(), "edge")
	if err != nil || second.Status.KubernetesVersion == nil || *second.Status.KubernetesVersion != "token-two" {
		t.Fatal("stale probe after credential refresh", err)
	}
}

func TestDynamicClientHonorsTLSAndToken(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer registration-token" {
			t.Error("token absent")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"major":"1","minor":"36","gitVersion":"v1.36.5"}`))
	}))
	defer server.Close()
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	cfg := EnvConfig{Name: "edge", KubeContext: "not-in-any-file", RESTConfig: &rest.Config{Host: server.URL, BearerToken: "registration-token", TLSClientConfig: rest.TLSClientConfig{CAData: ca}}}
	client, err := NewKubeClient(cfg, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = client.Discovery().ServerVersionWithContext(context.Background()); err != nil {
		t.Fatal("dynamic config not used", err)
	}
	cfg.RESTConfig.ServerName = "wrong-host.example"
	client, err = NewKubeClient(cfg, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = client.Discovery().ServerVersionWithContext(context.Background()); err == nil {
		t.Fatal("TLS hostname validation bypassed")
	}
}
