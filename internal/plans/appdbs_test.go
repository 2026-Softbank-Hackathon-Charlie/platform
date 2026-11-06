package plans

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"
)

// fakeAppDBs is AppDatabases over maps; set records what it was asked to
// put into the environment.
type fakeAppDBs struct {
	pw, apps map[string][]string
	err      error
	set      [][]string
}

func (f *fakeAppDBs) RegistryHosts(context.Context) (map[string][]string, map[string][]string, error) {
	return f.pw, f.apps, f.err
}

func (f *fakeAppDBs) SetRegistryPasswords(_ context.Context, refs []string) error {
	f.set = append(f.set, slices.Clone(refs))
	return nil
}

func TestWithRegisteredKeepsThePolicysEntries(t *testing.T) {
	p := Policy{HostPasswordEnvs: map[string][]string{"A_PGPASSWORD": {"a:5432"}}, AppDBHosts: map[string][]string{"moimroom": {"a:5432"}}}
	got := p.withRegistered(
		map[string][]string{"A_PGPASSWORD": {"evil:5432"}, "B_PGPASSWORD": {"b:5432"}},
		map[string][]string{"moimroom": {"evil:5432"}, "mig": {"b:5432"}})
	if want := map[string][]string{"A_PGPASSWORD": {"a:5432"}, "B_PGPASSWORD": {"b:5432"}}; !reflect.DeepEqual(got.HostPasswordEnvs, want) {
		t.Errorf("HostPasswordEnvs = %v", got.HostPasswordEnvs)
	}
	if want := map[string][]string{"moimroom": {"a:5432"}, "mig": {"b:5432"}}; !reflect.DeepEqual(got.AppDBHosts, want) {
		t.Errorf("AppDBHosts = %v", got.AppDBHosts)
	}
	if len(p.HostPasswordEnvs) != 1 || len(p.AppDBHosts) != 1 {
		t.Error("withRegistered changed the policy it was called on")
	}
}

func TestParseReadsAppDatabasesEveryTime(t *testing.T) {
	s := newService(DefaultPolicy())
	f := &fakeAppDBs{}
	s.SetAppDatabases(f)
	raw := []byte(`{"id": "mig-gke-to-eks",
		"source": {"env": "gke", "namespace": "mig", "cluster": "mig-db", "dsn": "postgres://app:${GKE_MIG_PGPASSWORD}@db-gke-mig.svc:5432/app"},
		"target": {"env": "eks", "namespace": "mig", "cluster": "mig-db", "dsn": "postgres://app:${EKS_MIG_PGPASSWORD}@db-eks-mig.svc:5432/app"},
		"gateway": {"mode": "none"}}`)
	if _, err := s.Parse(raw); !errors.Is(err, ErrInvalid) {
		t.Fatalf("before the app is known: %v, want ErrInvalid", err)
	}
	if len(f.set) != 0 {
		t.Fatalf("passwords set for a refused plan: %v", f.set)
	}
	f.pw = map[string][]string{"GKE_MIG_PGPASSWORD": {"db-gke-mig.svc:5432"}, "EKS_MIG_PGPASSWORD": {"db-eks-mig.svc:5432"}}
	if _, err := s.Parse(raw); err != nil {
		t.Fatalf("once the app is known: %v", err)
	}
	if want := [][]string{{"GKE_MIG_PGPASSWORD", "EKS_MIG_PGPASSWORD"}}; !reflect.DeepEqual(f.set, want) {
		t.Fatalf("passwords set %v, want %v", f.set, want)
	}
	f.err = errors.New("store down")
	if _, err := s.Parse(raw); err == nil || errors.Is(err, ErrInvalid) {
		t.Fatalf("store down: %v, want an error that is not ErrInvalid", err)
	}
}
