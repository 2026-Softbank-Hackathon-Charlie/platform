package api

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/cnpg"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/deploy"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/gateway"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/appspec"
)

// DBClusterTimeout bounds the read of one environment's CNPG Cluster, and
// of the traffic gateway, for the readiness of a database pair (ADR 0037).
// The reads run in parallel, so a GET waits at most this long for them.
var DBClusterTimeout = 3 * time.Second

// ReadinessCacheTTL is how long the readiness of a database pair reuses
// what it read from the gateway and the workload clusters for an app (ADR
// 0037). The console reads the app every few seconds; the deployments and
// migration runs are read again every time.
var ReadinessCacheTTL = 10 * time.Second

// DBClusterReader reads an app database's CNPG Cluster in an environment's
// workload cluster (cnpg.Client.Get). A Cluster that does not exist is an
// error that wraps cnpg.ErrNotFound.
type DBClusterReader func(ctx context.Context, env, namespace, name string) (*cnpg.Status, error)

// Readiness reasons (apiv1.AppDBReadiness.reason).
const (
	readyReason           = "ready"
	reasonReplicaMissing  = "replica_missing"
	reasonMigration       = "migration_in_progress"
	reasonDeployPending   = "deploy_pending"
	reasonAppDegraded     = "app_degraded"
	reasonDeployFailed    = "deploy_failed"
	reasonGatewayDown     = "gateway_unreachable"
	reasonGatewayMissing  = "gateway_backend_missing"
	reasonClusterUnread   = "cluster_unreadable"
	reasonClusterUnhealth = "cluster_not_healthy"
	reasonTopology        = "topology_mismatch"
	reasonWriteBackend    = "write_backend_mismatch"
)

// readinessReads is what the readiness of a pair reads outside the store:
// the gateway's app (when it is read) and each environment's Cluster.
type readinessReads struct {
	gateway   bool // the gateway was asked
	gatewayOK bool // and answered
	gwErr     error
	gwApp     *gateway.AppInfo // the app's, when the gateway has it
	status    []*cnpg.Status
	errs      []error
	at        time.Time
}

// readinessCache keeps readinessReads by app for ReadinessCacheTTL.
type readinessCache struct {
	mu    sync.Mutex
	reads map[string]readinessReads
}

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
	inApp := make([]bool, len(pair))
	for i, env := range pair {
		inApp[i] = slices.Contains(a.Environments, env)
		e := apiv1.AppDBReadinessEnvironment{Env: env, InApp: inApp[i], Role: apiv1.AppDBReadinessEnvironmentRoleUnknown, GatewayBackend: true}
		if !e.InApp {
			fail(reasonReplicaMissing, fmt.Sprintf("%s is not one of the app's environments; add it with POST /v1/apps/%s/environments", env, a.Name))
		}
		out.Environments[i] = e
	}
	cluster := ""
	if i := slices.IndexFunc(spec.Resources, func(r appspec.Resource) bool { return r.Type == appspec.TypePostgres }); i >= 0 {
		cluster = deploy.ClusterName(a.Name, spec.Resources[i].Name)
	}
	_, public := publicSpecService(spec)

	// A migration of the app moves its database and traffic; the pair is
	// not ready for another until it is done. What was read before a run
	// ended is not reused: the run may have moved the primary.
	var ended time.Time
	runs, err := s.appRuns(ctx, []string{a.Name})
	if err != nil {
		fail(reasonMigration, "could not read the app's migration runs: "+err.Error())
	} else if i := slices.IndexFunc(runs, s.runActive); i >= 0 {
		fail(reasonMigration, fmt.Sprintf("migration run %s of %s is %s", runs[i].RunId, a.Name, runs[i].Status))
	}
	for _, r := range runs {
		if r.EndedAt != nil && r.EndedAt.After(ended) {
			ended = *r.EndedAt
		}
	}
	reads := s.readinessReads(ctx, a.Name, pair, inApp, cluster, public && s.d.Gateway != nil, ended)
	if reads.gwApp != nil && reads.gwApp.WritesHeld {
		fail(reasonMigration, fmt.Sprintf("the traffic gateway holds the writes of %s", a.Name))
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
		st := ""
		if d != nil {
			st = envStatus(*d, env)
		}
		e.Deployed = d != nil && st == deploy.EnvHealthy
		switch {
		case e.Deployed:
		case d == nil:
			deployReasons[reasonDeployPending] = cmp.Or(deployReasons[reasonDeployPending], fmt.Sprintf("no deployment of %s is going to %s or went there; POST /v1/apps/%s/environments with %s again", a.Name, env, a.Name, env))
		case !store.DeploymentFinished(d.Status):
			deployReasons[reasonDeployPending] = cmp.Or(deployReasons[reasonDeployPending], fmt.Sprintf("deployment %s to %s is %s", d.ID, env, d.Status))
		case d.Status == store.DeploySuperseded:
			deployReasons[reasonDeployPending] = cmp.Or(deployReasons[reasonDeployPending], fmt.Sprintf("deployment %s to %s was superseded before it was healthy there; POST /v1/apps/%s/environments with %s again", d.ID, env, a.Name, env))
		case st == deploy.EnvDegraded:
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

	if reads.gateway {
		for i := range out.Environments {
			e := &out.Environments[i]
			e.GatewayBackend = reads.gatewayOK && reads.gwApp != nil && reads.gwApp.Backends[e.Env] != ""
			switch {
			case !reads.gatewayOK:
				fail(reasonGatewayDown, "could not read the traffic gateway: "+reads.gwErr.Error())
			case !e.GatewayBackend:
				fail(reasonGatewayMissing, fmt.Sprintf("the traffic gateway has no %s backend for %s", e.Env, a.Name))
			}
		}
	}

	for i, st := range reads.status {
		e := &out.Environments[i]
		if !e.InApp {
			continue
		}
		if reads.errs[i] != nil || st == nil {
			fail(reasonClusterUnread, fmt.Sprintf("could not read Cluster %s/%s in %s: %v", deploy.Namespace(a.Name), cluster, e.Env, reads.errs[i]))
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
		if err := pairTopology(reads.status[0], reads.status[1]); err != nil {
			fail(reasonTopology, err.Error())
		}
	}
	// The gateway writes where the database is primary, or a migration
	// would start from the wrong side.
	if reason == "" && reads.gwApp != nil {
		primary := ""
		for _, e := range out.Environments {
			if e.Role == apiv1.AppDBReadinessEnvironmentRolePrimary {
				primary = e.Env
			}
		}
		if reads.gwApp.WriteBackend != primary {
			fail(reasonWriteBackend, fmt.Sprintf("the traffic gateway writes %s to %q, but its database is primary in %s", a.Name, reads.gwApp.WriteBackend, primary))
		}
	}
	if reason == "" {
		reason, message = readyReason, "both databases are healthy and the replica follows the primary"
	}
	out.Ready, out.Reason, out.Message = reason == readyReason, reason, message
	return out
}

// readinessReads reads the gateway (with readGateway) and the Cluster of
// each environment of pair that is inApp, all in parallel and each within
// DBClusterTimeout, or returns what it read for the same question less than
// ReadinessCacheTTL ago and after notBefore (when a migration run of the
// app last ended). What it read while ctx ended (the client went away) is
// not kept, since its errors say nothing about the clusters.
func (s *Server) readinessReads(ctx context.Context, app string, pair []string, inApp []bool, cluster string, readGateway bool, notBefore time.Time) readinessReads {
	key := fmt.Sprintf("%s|%s|%v|%s|%t", app, strings.Join(pair, ","), inApp, cluster, readGateway)
	now := time.Now()
	s.ready.mu.Lock()
	if r, ok := s.ready.reads[key]; ok && now.Sub(r.at) < ReadinessCacheTTL && r.at.After(notBefore) {
		s.ready.mu.Unlock()
		return r
	}
	s.ready.mu.Unlock()

	out := readinessReads{gateway: readGateway, status: make([]*cnpg.Status, len(pair)), errs: make([]error, len(pair)), at: now}
	var wg sync.WaitGroup
	if readGateway {
		wg.Go(func() {
			gctx, cancel := context.WithTimeout(ctx, DBClusterTimeout)
			defer cancel()
			list, err := s.d.Gateway.Apps(gctx)
			if err != nil {
				out.gwErr = err
				return
			}
			out.gatewayOK = true
			for _, ga := range list.Apps {
				if ga.Name == app && !ga.Default {
					out.gwApp = &ga
				}
			}
		})
	}
	for i := range pair {
		if !inApp[i] {
			continue
		}
		wg.Go(func() {
			if s.d.DBCluster == nil || cluster == "" {
				out.errs[i] = fmt.Errorf("this server does not read the workload clusters")
				return
			}
			cctx, cancel := context.WithTimeout(ctx, DBClusterTimeout)
			defer cancel()
			out.status[i], out.errs[i] = s.d.DBCluster(cctx, pair[i], deploy.Namespace(app), cluster)
		})
	}
	wg.Wait()
	if ctx.Err() != nil {
		return out
	}

	s.ready.mu.Lock()
	defer s.ready.mu.Unlock()
	if s.ready.reads == nil {
		s.ready.reads = map[string]readinessReads{}
	}
	for k, r := range s.ready.reads { // drop what expired, so the map stays small
		if now.Sub(r.at) >= ReadinessCacheTTL {
			delete(s.ready.reads, k)
		}
	}
	s.ready.reads[key] = out
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
