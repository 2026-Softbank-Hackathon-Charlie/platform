package api

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/cnpg"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/deploy"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/appspec"
)

// DBClusterTimeout bounds the read of one environment's CNPG Cluster for
// the readiness of a database pair (ADR 0037).
var DBClusterTimeout = 3 * time.Second

// DBClusterReader reads an app database's CNPG Cluster in an environment's
// workload cluster (cnpg.Client.Get). A Cluster that does not exist is an
// error that wraps cnpg.ErrNotFound.
type DBClusterReader func(ctx context.Context, env, namespace, name string) (*cnpg.Status, error)

// Readiness reasons (apiv1.AppDBReadiness.reason).
const (
	readyReason           = "ready"
	reasonReplicaMissing  = "replica_missing"
	reasonDeployPending   = "deploy_pending"
	reasonAppDegraded     = "app_degraded"
	reasonDeployFailed    = "deploy_failed"
	reasonGatewayDown     = "gateway_unreachable"
	reasonGatewayMissing  = "gateway_backend_missing"
	reasonClusterUnread   = "cluster_unreadable"
	reasonClusterUnhealth = "cluster_not_healthy"
	reasonTopology        = "topology_mismatch"
)

// dbReadiness says whether the app's database pair can be migrated now
// (apiv1.AppDBReadiness): the checks in that order, the first failure
// being the reason. deps are the default instance's deployments, newest
// first; spec is the default instance's platform.yaml.
func (s *Server) dbReadiness(ctx context.Context, a store.App, db store.AppDatabase, deps []store.Deployment, spec appspec.Spec) apiv1.AppDBReadiness {
	pair := []string{db.PrimaryEnv, db.PeerEnv}
	out := apiv1.AppDBReadiness{Environments: make([]apiv1.AppDBReadinessEnvironment, len(pair))}
	reason, message := "", ""
	fail := func(r, m string) {
		if reason == "" {
			reason, message = r, m
		}
	}
	for i, env := range pair {
		e := apiv1.AppDBReadinessEnvironment{Env: env, InApp: slices.Contains(a.Environments, env), Role: apiv1.AppDBReadinessEnvironmentRoleUnknown, GatewayBackend: true}
		if !e.InApp {
			fail(reasonReplicaMissing, fmt.Sprintf("%s is not one of the app's environments; add it with POST /v1/apps/%s/environments", env, a.Name))
		}
		out.Environments[i] = e
	}
	// Of the deployment reasons, one in progress comes first (it changes
	// what the others would say), then a Degraded environment, which says
	// the most about why.
	deployReasons := map[string]string{}
	for i, env := range pair {
		e := &out.Environments[i]
		if !e.InApp {
			continue
		}
		d := newestTo(deps, env)
		e.Deployed = d != nil && d.Status == store.DeployHealthy
		switch {
		case e.Deployed:
		case d == nil || !store.DeploymentFinished(d.Status) || d.Status == store.DeploySuperseded:
			deployReasons[reasonDeployPending] = cmp.Or(deployReasons[reasonDeployPending], fmt.Sprintf("no deployment of %s has gone to %s healthy yet", a.Name, env))
		case envStatus(*d, env) == deploy.EnvDegraded:
			deployReasons[reasonAppDegraded] = cmp.Or(deployReasons[reasonAppDegraded], fmt.Sprintf("deployment %s left %s Degraded: the app probably writes to its database when it starts, which the replica there refuses; migrate it with the engine's cold mode", d.ID, env))
		default:
			deployReasons[reasonDeployFailed] = cmp.Or(deployReasons[reasonDeployFailed], fmt.Sprintf("deployment %s to %s did not end healthy: %s", d.ID, env, d.Error))
		}
	}
	for _, r := range []string{reasonDeployPending, reasonAppDegraded, reasonDeployFailed} {
		if m, ok := deployReasons[r]; ok {
			fail(r, m)
		}
	}

	if _, public := publicSpecService(spec); public && s.d.Gateway != nil {
		gctx, cancel := context.WithTimeout(ctx, DBClusterTimeout)
		list, err := s.d.Gateway.Apps(gctx)
		cancel()
		var backends map[string]string
		for _, ga := range list.Apps {
			if ga.Name == a.Name && !ga.Default {
				backends = ga.Backends
			}
		}
		for i := range out.Environments {
			e := &out.Environments[i]
			e.GatewayBackend = err == nil && backends[e.Env] != ""
			switch {
			case err != nil:
				fail(reasonGatewayDown, "could not read the traffic gateway: "+err.Error())
			case !e.GatewayBackend:
				fail(reasonGatewayMissing, fmt.Sprintf("the traffic gateway has no %s backend for %s", e.Env, a.Name))
			}
		}
	}

	cluster := ""
	if i := slices.IndexFunc(spec.Resources, func(r appspec.Resource) bool { return r.Type == appspec.TypePostgres }); i >= 0 {
		cluster = deploy.ClusterName(a.Name, spec.Resources[i].Name)
	}
	status := make([]*cnpg.Status, len(pair))
	errs := make([]error, len(pair))
	var wg sync.WaitGroup
	for i := range pair {
		if !out.Environments[i].InApp {
			continue
		}
		wg.Go(func() {
			if s.d.DBCluster == nil || cluster == "" {
				errs[i] = fmt.Errorf("this server does not read the workload clusters")
				return
			}
			cctx, cancel := context.WithTimeout(ctx, DBClusterTimeout)
			defer cancel()
			status[i], errs[i] = s.d.DBCluster(cctx, pair[i], deploy.Namespace(a.Name), cluster)
		})
	}
	wg.Wait()
	for i, st := range status {
		e := &out.Environments[i]
		if !e.InApp {
			continue
		}
		if errs[i] != nil || st == nil {
			fail(reasonClusterUnread, fmt.Sprintf("could not read Cluster %s/%s in %s: %v", deploy.Namespace(a.Name), cluster, e.Env, errs[i]))
			continue
		}
		e.Phase = optional(st.Phase)
		e.Healthy = st.Healthy()
		e.Role = apiv1.AppDBReadinessEnvironmentRoleReplica
		if st.IsPrimary() {
			e.Role = apiv1.AppDBReadinessEnvironmentRolePrimary
		}
		if !e.Healthy {
			fail(reasonClusterUnhealth, fmt.Sprintf("Cluster %s in %s is not healthy (%s, %d of %d instances ready)", cluster, e.Env, st.Phase, st.ReadyInstances, st.Instances))
		}
	}
	if reason == "" {
		if err := pairTopology(status[0], status[1]); err != nil {
			fail(reasonTopology, err.Error())
		}
	}
	if reason == "" {
		reason, message = readyReason, "both databases are healthy and the replica follows the primary"
	}
	out.Ready, out.Reason, out.Message = reason == readyReason, reason, message
	return out
}

// pairTopology checks that one of the two Clusters is the primary and the
// other follows it, with the rules the migration engine's precheck applies
// to a source and a target (migration-engine pkg/engine checkTopology).
func pairTopology(a, b *cnpg.Status) error {
	src, dst := a, b
	switch {
	case a.IsPrimary() && b.IsPrimary():
		return fmt.Errorf("both Clusters %s and %s are primaries", a.Self, b.Self)
	case !a.IsPrimary() && !b.IsPrimary():
		return fmt.Errorf("neither Cluster %s nor %s is the primary", a.Self, b.Self)
	case b.IsPrimary():
		src, dst = b, a
	}
	switch {
	case dst.Self == src.Self:
		return fmt.Errorf("both Clusters have replica.self %q", src.Self)
	case dst.ReplicaPrimary != src.Self:
		return fmt.Errorf("the replica's replica.primary is %q, not the primary %q", dst.ReplicaPrimary, src.Self)
	case dst.ReplicaSource != src.Self:
		return fmt.Errorf("the replica's replica.source is %q, not the primary %q", dst.ReplicaSource, src.Self)
	case src.ReplicaSource != dst.Self:
		return fmt.Errorf("the primary's replica.source is %q, not the replica %q", src.ReplicaSource, dst.Self)
	}
	return nil
}

// envStatus is the status deployment d recorded for env.
func envStatus(d store.Deployment, env string) string {
	for _, e := range d.Environments {
		if e.Env == env {
			return e.Status
		}
	}
	return ""
}

// publicSpecService returns the spec's public service.
func publicSpecService(spec appspec.Spec) (appspec.Service, bool) {
	for _, sv := range spec.Services {
		if sv.Public {
			return sv, true
		}
	}
	return appspec.Service{}, false
}
