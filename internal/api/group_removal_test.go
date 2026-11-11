package api

import (
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/cnpg"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/deploy"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

// Removing a member of a three-environment database group (ADR 0064).

func member(env, primary, source string, ext ...string) *cnpg.Status {
	st := &cnpg.Status{Phase: cnpg.HealthyPhase, Instances: 1, ReadyInstances: 1, Self: "shop-db-" + env, ReplicaPrimary: "shop-db-" + primary, ReplicaSource: "shop-db-" + source}
	for _, e := range ext {
		st.ExternalClusters = append(st.ExternalClusters, "shop-db-"+e)
	}
	return st
}

// groupFixture has shop in gke, eks and onprem, born primary in gke with
// eks its peer, every deployment healthy, and the Clusters as the chart
// draws them: gke primary following eks once demoted, the others following
// gke.
func groupFixture(t *testing.T) *envFixture {
	t.Helper()
	f := newEnvFixture(t, withAppDBEnvs("gke", "eks", "onprem"))
	f.register("gke", "eks")
	f.clusters.status["gke"] = member("gke", "gke", "eks", "gke", "eks")
	code, b := f.add("onprem")
	if code != 202 {
		t.Fatalf("add third: %d %s", code, b)
	}
	f.finish(decode[apiv1.AppEnvironmentAdded](t, b).Deployment.Id, deploy.EnvHealthy, nil)
	f.clusters.status = map[string]*cnpg.Status{
		"gke":    member("gke", "gke", "eks", "gke", "eks", "onprem"),
		"eks":    member("eks", "gke", "gke", "eks", "gke", "onprem"),
		"onprem": member("onprem", "gke", "gke", "onprem", "gke", "eks"),
	}
	return f
}

func (f *envFixture) removeEnv(env string) (int, []byte) {
	afterNewest()
	return f.do("DELETE", "/v1/apps/shop/environments/"+env, opToken, nil)
}

func (f *envFixture) environments() []string {
	f.t.Helper()
	a, err := f.store.GetApp(f.t.Context(), "shop")
	if err != nil {
		f.t.Fatal(err)
	}
	return a.Environments
}

// TestRemoveGroupPeerRewiresPrimary: the peer goes, so the primary, which
// follows it once demoted, is set to follow the third member first.
func TestRemoveGroupPeerRewiresPrimary(t *testing.T) {
	f := groupFixture(t)
	code, b := f.removeEnv("eks")
	if code != 202 {
		t.Fatalf("remove eks: %d %s", code, b)
	}
	if !slices.Equal(f.clusters.rewired, []string{"gke:shop-db-onprem"}) {
		t.Fatalf("rewired %v", f.clusters.rewired)
	}
	if !slices.Equal(f.rm.removed, []string{"shop/eks"}) || !slices.Equal(f.environments(), []string{"gke", "onprem"}) {
		t.Fatalf("removed %v, environments %v", f.rm.removed, f.environments())
	}
	out := decode[apiv1.AppEnvironmentRemoved](t, b)
	if out.Deployment == nil || len(out.Deployment.Environments) != 2 {
		t.Fatalf("no deployment to the remaining two: %s", b)
	}
}

// TestRemoveGroupMemberKeepsSource: a member nobody follows goes without
// changing a Cluster, also while another member is the primary and the
// birth primary follows it.
func TestRemoveGroupMemberKeepsSource(t *testing.T) {
	t.Run("third", func(t *testing.T) {
		f := groupFixture(t)
		if code, b := f.removeEnv("onprem"); code != 202 {
			t.Fatalf("remove onprem: %d %s", code, b)
		}
		if len(f.clusters.rewired) != 0 || !slices.Equal(f.environments(), []string{"gke", "eks"}) {
			t.Fatalf("rewired %v, environments %v", f.clusters.rewired, f.environments())
		}
	})
	t.Run("primary moved to the peer", func(t *testing.T) {
		f := groupFixture(t)
		// After a migration gke to eks: onprem cascades through gke.
		f.clusters.status["eks"] = member("eks", "eks", "gke", "eks", "gke", "onprem")
		f.clusters.status["gke"] = member("gke", "eks", "eks", "gke", "eks", "onprem")
		if code, b := f.removeEnv("onprem"); code != 202 {
			t.Fatalf("remove onprem: %d %s", code, b)
		}
		if len(f.clusters.rewired) != 0 {
			t.Fatalf("rewired %v", f.clusters.rewired)
		}
	})
	t.Run("unreadable member", func(t *testing.T) {
		f := groupFixture(t)
		f.clusters.err["onprem"] = errors.New("dial tcp: i/o timeout")
		if code, b := f.removeEnv("onprem"); code != 202 {
			t.Fatalf("remove an unreadable onprem: %d %s", code, b)
		}
	})
}

// TestRemoveGroupMemberRefusals: nothing is removed or changed while the
// two that would remain are not one healthy primary and a replica
// streaming from it.
func TestRemoveGroupMemberRefusals(t *testing.T) {
	for _, c := range []struct {
		name, env string
		change    func(f *envFixture)
		code      int
		want      string
	}{
		{"remaining unhealthy", "eks", func(f *envFixture) { f.clusters.status["onprem"].ReadyInstances = 0 }, 409, "db_topology_unready"},
		{"remaining unreadable", "eks", func(f *envFixture) { f.clusters.err["onprem"] = errors.New("forbidden") }, 502, "db_topology_unknown"},
		{"removed is primary", "onprem", func(f *envFixture) {
			f.clusters.status["onprem"] = member("onprem", "onprem", "gke", "onprem", "gke", "eks")
		}, 409, "write_backend"},
		{"remaining cascades through the removed", "onprem", func(f *envFixture) {
			f.clusters.status["eks"] = member("eks", "gke", "onprem", "eks", "gke", "onprem")
		}, 409, "db_topology_unready"},
		{"no primary remains", "eks", func(f *envFixture) {
			f.clusters.status["gke"] = member("gke", "eks", "eks", "gke", "eks", "onprem")
		}, 409, "db_topology_unready"},
		{"two primaries remain", "eks", func(f *envFixture) {
			f.clusters.status["onprem"] = member("onprem", "onprem", "gke", "onprem", "gke", "eks")
		}, 409, "db_topology_unready"},
		{"primary cannot follow the third", "eks", func(f *envFixture) {
			f.clusters.status["gke"] = member("gke", "gke", "eks", "gke", "eks")
		}, 409, "db_topology_unready"},
		{"rewire fails", "eks", func(f *envFixture) { f.clusters.rewireErr = errors.New("connection reset") }, 502, "db_topology_unreachable"},
		{"rewire test fails", "eks", func(f *envFixture) {
			f.clusters.rewireErr = apierrors.NewInvalid(schema.GroupKind{Group: "postgresql.cnpg.io", Kind: "Cluster"}, "shop-db", field.ErrorList{field.Invalid(field.NewPath("metadata", "resourceVersion"), "1", "test failed")})
		}, 409, "db_topology_changed"},
		{"rewire refused by policy", "eks", func(f *envFixture) {
			f.clusters.rewireErr = fmt.Errorf("rewire: %w", apierrors.NewInvalid(schema.GroupKind{Group: "postgresql.cnpg.io", Kind: "Cluster"}, "shop-db", field.ErrorList{field.Forbidden(field.NewPath(""), "ValidatingAdmissionPolicy 'hopcharlie-platform-migration' denied request")}))
		}, 502, "db_topology_denied"},
		// The executor refuses under the locks before the check that changes
		// the primary's replica.source: eks still has a read weight.
		{"gateway still routes to it", "eks", func(f *envFixture) {
			f.rm.err = fmt.Errorf("%w: the traffic gateway sends the requests of shop to eks", deploy.ErrEnvironmentInUse)
		}, 409, "write_backend"},
		{"no cluster access", "eks", func(f *envFixture) { f.api.d.DBRewire = nil }, 503, "db_topology_unknown"},
		{"first environment", "gke", func(f *envFixture) {}, 409, "primary_environment"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := groupFixture(t)
			c.change(f)
			code, b := f.removeEnv(c.env)
			if code != c.code || errCode(t, b) != c.want {
				t.Fatalf("remove %s: %d %s, want %d %s", c.env, code, b, c.code, c.want)
			}
			if len(f.rm.removed) != 0 || len(f.clusters.rewired) != 0 || !slices.Equal(f.environments(), []string{"gke", "eks", "onprem"}) {
				t.Fatalf("changed on refusal: removed %v, rewired %v, environments %v", f.rm.removed, f.clusters.rewired, f.environments())
			}
		})
	}
}

// TestRemoveGroupMemberRechecksUnderTheLocks: what changed while the
// removal waited for the instance locks stops it before anything changes.
func TestRemoveGroupMemberRechecksUnderTheLocks(t *testing.T) {
	f := groupFixture(t)
	f.rm.beforeCheck = func() {
		f.clusters.mu.Lock()
		f.clusters.status["onprem"] = member("onprem", "onprem", "gke", "onprem", "gke", "eks")
		f.clusters.mu.Unlock()
	}
	if code, b := f.removeEnv("onprem"); code != 409 || errCode(t, b) != "write_backend" {
		t.Fatalf("remove a member promoted meanwhile: %d %s", code, b)
	}
	if !slices.Equal(f.environments(), []string{"gke", "eks", "onprem"}) {
		t.Fatalf("environments %v", f.environments())
	}
}
