package api

import (
	"slices"
	"testing"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/cnpg"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/deploy"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

func TestAddThirdDatabaseEnvironment(t *testing.T) {
	f := newEnvFixture(t, withAppDBEnvs("gke", "eks", "onprem"))
	f.register("gke", "eks")
	f.clusters.status["gke"] = &cnpg.Status{Self: "shop-db-gke", ReplicaPrimary: "shop-db-gke", ReplicaSource: "shop-db-eks", Phase: "Cluster in healthy state", Instances: 1, ReadyInstances: 1}
	before, err := f.store.GetAppDatabase(t.Context(), "shop")
	if err != nil {
		t.Fatal(err)
	}
	code, b := f.add("onprem")
	if code != 202 {
		t.Fatalf("add third: %d %s", code, b)
	}
	out := decode[apiv1.AppEnvironmentAdded](t, b)
	if !slices.Equal(out.App.Deploy.Environments, []string{"gke", "eks", "onprem"}) || len(out.Deployment.Environments) != 3 {
		t.Fatalf("third member missing: %+v", out)
	}
	after, err := f.store.GetAppDatabase(t.Context(), "shop")
	if err != nil || before.CACert != after.CACert || before.KeyCheck != after.KeyCheck || !slices.Equal(before.PasswordCiphertext, after.PasswordCiphertext) || before.PrimaryEnv != after.PrimaryEnv || before.PeerEnv != after.PeerEnv {
		t.Fatalf("adding a member changed the original database credentials or anchors: %v", err)
	}
	f.finish(out.Deployment.Id, deploy.EnvHealthy, nil)
}

func TestThirdDatabaseEnvironmentRequiresBootstrapPrimary(t *testing.T) {
	for _, st := range []*cnpg.Status{nil, {Self: "shop-db-gke", ReplicaPrimary: "shop-db-eks", ReplicaSource: "shop-db-eks", Phase: "Cluster in healthy state", Instances: 1, ReadyInstances: 1}, {Self: "shop-db-gke", ReplicaPrimary: "shop-db-gke", Instances: 1, ReadyInstances: 0}} {
		f := newEnvFixture(t, withAppDBEnvs("gke", "eks", "onprem"))
		f.register("gke", "eks")
		if st != nil {
			f.clusters.status["gke"] = st
		}
		if code, b := f.add("onprem"); code != 409 || errCode(t, b) != "db_bootstrap_primary_required" {
			t.Fatalf("unready bootstrap accepted: %d %s", code, b)
		}
		a, _ := f.store.GetApp(t.Context(), "shop")
		if !slices.Equal(a.Environments, []string{"gke", "eks"}) {
			t.Fatalf("membership changed on rejection: %v", a.Environments)
		}
	}
}

func TestGroupTopologyRejectsSplitBrainAndCycles(t *testing.T) {
	primary := &cnpg.Status{Self: "g", ReplicaPrimary: "g", ReplicaSource: "e"}
	eks := &cnpg.Status{Self: "e", ReplicaPrimary: "g", ReplicaSource: "g"}
	onprem := &cnpg.Status{Self: "o", ReplicaPrimary: "g", ReplicaSource: "g"}
	if err := groupTopology([]*cnpg.Status{primary, eks, onprem}); err != nil {
		t.Fatal(err)
	}
	// Cascading standbys remain valid observations, but the engine refuses
	// migrating directly from o to e while e still follows g.
	primary.ReplicaPrimary, primary.ReplicaSource = "o", "o"
	onprem.ReplicaPrimary = "o"
	if err := groupTopology([]*cnpg.Status{primary, eks, onprem}); err != nil {
		t.Fatal(err)
	}
	eks.ReplicaSource = "e"
	if err := groupTopology([]*cnpg.Status{primary, eks, onprem}); err == nil {
		t.Fatal("self cycle accepted")
	}
	eks.ReplicaSource, eks.ReplicaPrimary = "g", "e"
	if err := groupTopology([]*cnpg.Status{primary, eks, onprem}); err == nil {
		t.Fatal("two primaries accepted")
	}
}
