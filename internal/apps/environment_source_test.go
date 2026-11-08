package apps

import (
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/envs"
	"k8s.io/client-go/rest"
	"testing"
)

func TestCatalogSeesNewEnvironmentFromRegistry(t *testing.T) {
	svc, err := envs.New(envs.Config{Environments: []envs.EnvConfig{{Name: "static", Provider: "onprem", KubeContext: "static"}}}, envs.Options{})
	if err != nil {
		t.Fatal(err)
	}
	catalog := Empty(svc.Configs(), "")
	catalog.SetEnvironmentSource(svc.Configs)
	rc := &rest.Config{Host: "https://edge.example:6443", BearerToken: "token", TLSClientConfig: rest.TLSClientConfig{CAData: []byte("ca")}}
	if err = svc.RegisterConnection("edge", "owner", "uid", rc); err != nil {
		t.Fatal(err)
	}
	if cfg, ok := catalog.env("edge"); !ok || cfg.RESTConfig == nil {
		t.Fatal("catalog uses stale environment snapshot")
	}
	view := catalog.view(App{Name: "app", Environments: map[string]Environment{"edge": {CnpgCluster: "db", DBHost: "db.edge", DBPort: 5432}}})
	if len(view.Environments) != 1 || view.Environments[0].Env != "edge" {
		t.Fatal("new app environment omitted from view")
	}
}
