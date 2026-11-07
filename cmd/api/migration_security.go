package main

import (
	"bytes"
	"context"
	"slices"
	"strings"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/envs"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/plans"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/runs"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/security"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
	"k8s.io/apimachinery/pkg/util/validation"
)

// The plan policy already binds the gateway scope to the app. Confirm the
// registered namespace and cluster mapping before reading any workloads.
func migrationSecurityAssessor(
	ctx context.Context,
	p *plan.Plan,
	doc apiv1.Run,
	gatewayRoot string,
	lookup func(context.Context, string) (apiv1.App, bool, error),
	envConfig func(string) (envs.EnvConfig, bool),
) runs.SecurityAssessor {
	if gatewayRoot == "" || p.Gateway.Mode != "http" {
		return nil
	}
	id, scoped := strings.CutPrefix(strings.TrimRight(p.Gateway.URL, "/"), plans.GatewayAppURL(gatewayRoot, ""))
	if !scoped || len(validation.IsDNS1123Label(id)) != 0 {
		return nil
	}
	app, found, err := lookup(ctx, id)
	if err != nil || !found || app.Name != id || app.Namespace == "" {
		return nil
	}
	stateless := plans.NoDatabase(p)
	bind := func(endpoint apiv1.PlanEndpoint, backend string) (envs.EnvConfig, bool) {
		if endpoint.Namespace != app.Namespace && !(stateless && endpoint.Namespace == "" && doc.App != nil && *doc.App == id) {
			return envs.EnvConfig{}, false
		}
		if !slices.ContainsFunc(app.Environments, func(e apiv1.AppEnvironment) bool { return e.Env == endpoint.Env }) {
			return envs.EnvConfig{}, false
		}
		cfg, ok := envConfig(endpoint.Env)
		if !ok || cfg.GatewayBackend != backend {
			return envs.EnvConfig{}, false
		}
		if !stateless && (endpoint.Context == nil || *endpoint.Context == "" || *endpoint.Context != cfg.KubeContext) {
			return envs.EnvConfig{}, false
		}
		return cfg, true
	}
	source, sourceOK := bind(doc.Source, p.Gateway.SourceBackend)
	target, targetOK := bind(doc.Target, p.Gateway.TargetBackend)
	if !sourceOK || !targetOK {
		return nil
	}
	return func(ctx context.Context, endpoint apiv1.PlanEndpoint) apiv1.RunSecurityAssessment {
		bound := target
		if endpoint.Env == doc.Source.Env {
			bound = source
		}
		// Dynamic on-prem connections rotate their read credentials during a run.
		cfg, found := envConfig(endpoint.Env)
		if !found || !sameMigrationCluster(bound, cfg) {
			return security.AssessMigrationPrivileged(ctx, nil, app.Namespace, id)
		}
		kube, _ := envs.NewKubeClient(cfg, security.KubeRequestTimeout)
		return security.AssessMigrationPrivileged(ctx, kube, app.Namespace, id)
	}
}

func sameMigrationCluster(bound, current envs.EnvConfig) bool {
	if bound.KubeContext != current.KubeContext || bound.Kubeconfig != current.Kubeconfig || bound.GatewayBackend != current.GatewayBackend {
		return false
	}
	a, b := bound.RESTConfig, current.RESTConfig
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Host == b.Host && a.ServerName == b.ServerName && a.Insecure == b.Insecure &&
		a.CAFile == b.CAFile && bytes.Equal(a.CAData, b.CAData)
}
