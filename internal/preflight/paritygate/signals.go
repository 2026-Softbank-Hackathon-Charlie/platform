package paritygate

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"sync"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/preflight/parity"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/preflight/probes"
)

func (g *Gate) SetSignalReader(reader parity.SignalReader) { g.signalReader = reader }

// Database-free plans have no CNPG namespace. Their gateway route has already
// passed plans.Policy; bind its exact app name to the server-owned namespace.
func (g *Gate) scopedPlan(p *plan.Plan) *plan.Plan {
	if p == nil || !p.Stateless() || p.Source.Namespace != "" || p.Target.Namespace != "" {
		return p
	}
	u, err := url.Parse(p.Gateway.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return p
	}
	for _, app := range g.cfg.Apps {
		if u.Path == "/apps/"+app.Name || u.Path == "/apps/"+app.Name+"/" {
			copy := *p
			copy.Source.Namespace, copy.Target.Namespace = app.Namespace, app.Namespace
			return &copy
		}
	}
	return p
}

func (g *Gate) ObservabilityConfigured(p *plan.Plan) bool {
	if g == nil || p == nil {
		return false
	}
	p = g.scopedPlan(p)
	app, ok := g.cfg.appFor(p.Target.Namespace)
	return ok && app.Observability != nil
}

// ObservabilityDecision only enforces the explicit per-app signal opt-in.
// Audit-only identity/secret/backup checks and untested alert delivery retain
// their existing unknown semantics. A rollback exemption is owned by plans.
func (g *Gate) ObservabilityDecision(p *plan.Plan, report *parity.Report) (bool, bool) {
	if g == nil || p == nil {
		return false, true
	}
	p = g.scopedPlan(p)
	app, ok := g.cfg.appFor(p.Target.Namespace)
	if !ok || app.Observability == nil || !app.Observability.Enforce {
		return false, true
	}
	if report == nil || report.ConfigDigest != g.digest || report.BaselineStatus != "operator_reviewed" || p.Source.Namespace != app.Namespace || !time.Now().Before(report.ExpiresAt) || report.ObservedAt.After(time.Now().Add(5*time.Second)) || time.Since(report.ObservedAt) > time.Minute {
		return true, false
	}
	for _, res := range report.Results {
		if res.Capability != parity.Observability {
			continue
		}
		if res.Target != (parity.Target{Environment: p.Target.Env, Namespace: p.Target.Namespace, Application: app.Name}) {
			return true, false
		}
		for _, side := range []string{"source", "target"} {
			for _, signal := range app.Observability.RequiredSignals {
				found := 0
				for _, check := range res.Checks {
					if check.ID == side+"-"+signal+"-ingestion" {
						found++
						if check.Status != parity.Pass {
							return true, false
						}
					}
				}
				if found != 1 {
					return true, false
				}
			}
		}
		return true, true
	}
	return true, false
}

type observationProbe struct {
	gate           *Gate
	app            App
	source, target parity.Target
}

func (p observationProbe) Check(ctx context.Context, target parity.Target) (parity.Result, error) {
	now := time.Now().UTC()
	out := parity.Result{Capability: parity.Observability, Target: target, Status: parity.Unknown, Code: "observability_incomplete", ObservedAt: now, Checks: []parity.Check{}}
	if target != p.target {
		return out, probes.ErrWrongTarget
	}
	type sideEvidence struct {
		metrics parity.Check
		signals []parity.SignalCount
		err     error
	}
	var evidence [2]sideEvidence
	scopes := []parity.Target{p.source, p.target}
	var pending sync.WaitGroup
	for i, scope := range scopes {
		if slices.Contains(p.app.Observability.RequiredSignals, "metrics") {
			pending.Go(func() { evidence[i].metrics = p.metrics(ctx, scope) })
		}
		if slices.Contains(p.app.Observability.RequiredSignals, "logs") || slices.Contains(p.app.Observability.RequiredSignals, "traces") {
			pending.Go(func() {
				cfg := p.gate.cfg.Environments[scope.Environment].Telemetry
				if cfg == nil || p.gate.signalReader == nil {
					return
				}
				age, _ := cfg.maxAge()
				evidence[i].signals, evidence[i].err = p.gate.signalReader.SignalCounts(ctx, parity.SignalScope{Target: scope, Cluster: cfg.ClusterName, DeploymentEnvironment: cfg.DeploymentEnvironment, Services: slices.Clone(p.app.Observability.ServiceNames)}, now.Add(-age), now)
			})
		}
	}
	pending.Wait()
	for i, side := range []string{"source", "target"} {
		for _, signal := range p.app.Observability.RequiredSignals {
			check := parity.Check{ID: side + "-" + signal + "-ingestion", Status: parity.Unknown, Code: "signal_not_configured"}
			if signal == "metrics" {
				check = evidence[i].metrics
				check.ID = side + "-metrics-ingestion"
			} else {
				cfg := p.gate.cfg.Environments[scopes[i].Environment].Telemetry
				if evidence[i].err != nil {
					check.Code = "signal_query_failed"
				} else if cfg != nil && p.gate.signalReader != nil {
					check.Code = "signal_missing"
					matched := 0
					for _, record := range evidence[i].signals {
						if record.Signal == signal {
							matched++
							age, _ := cfg.maxAge()
							if record.Code != "signal_counted" || record.Samples < 0 {
								check.Code = "signal_query_failed"
								continue
							}
							if record.Samples == 0 {
								continue
							}
							if record.LastSeen.IsZero() || record.LastSeen.After(now.Add(5*time.Second)) || now.Sub(record.LastSeen) > age {
								check.Code = "signal_stale"
								continue
							}
							check.Status, check.Code = parity.Pass, "signal_received"
							check.Evidence = fmt.Sprintf("source=hyperdx environment=%s namespace=%s app=%s cluster=%s samples=%d lastSeen=%s", scopes[i].Environment, scopes[i].Namespace, p.app.Name, cfg.ClusterName, record.Samples, record.LastSeen.UTC().Format(time.RFC3339))
						}
					}
					if matched != 1 {
						check.Status, check.Code, check.Evidence = parity.Unknown, "signal_incomplete", ""
					}
				}
			}
			if check.Status == parity.Fail {
				out.Status, out.Code = parity.Fail, "observability_failed"
			}
			out.Checks = append(out.Checks, check)
		}
	}
	out.Checks = append(out.Checks, parity.Check{ID: "alert-delivery", Status: parity.Unknown, Code: "alert_probe_not_implemented"})
	return out, nil
}

func (p observationProbe) metrics(ctx context.Context, scope parity.Target) parity.Check {
	unknown := parity.Check{Status: parity.Unknown, Code: "metrics_environment_binding_missing"}
	cfg := p.gate.cfg.Environments[scope.Environment]
	if cfg.Prometheus == nil || p.app.PrometheusJob == "" {
		unknown.Code = "metrics_not_configured"
		return unknown
	}
	labels := cfg.Prometheus.Labels
	if len(labels) == 0 || (labels["environment"] != "" && labels["environment"] != scope.Environment) || (labels["cluster"] != "" && (cfg.Telemetry == nil || labels["cluster"] != cfg.Telemetry.ClusterName)) {
		return unknown
	}
	age, _ := cfg.Prometheus.maxAge()
	probe, err := probes.NewPrometheusWithLabels(scope, cfg.Prometheus.URL, p.app.PrometheusJob, p.gate.http, age, labels)
	if err != nil {
		return unknown
	}
	result, err := probe.Check(ctx, scope)
	if err != nil || len(result.Checks) == 0 {
		unknown.Code = "metrics_unavailable"
		return unknown
	}
	check := result.Checks[0]
	if check.Status == parity.Pass {
		check.Evidence = fmt.Sprintf("environment=%s namespace=%s; %s", scope.Environment, scope.Namespace, check.Evidence)
	}
	return check
}
