package paritygate

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	authorizationclient "k8s.io/client-go/kubernetes/typed/authorization/v1"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/preflight/parity"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/preflight/probes"
)

// Timeout bounds one whole check, so recording parity never holds a start
// for long.
const Timeout = 30 * time.Second

// ErrNoApp: the plan's target namespace has no app in the parity file.
var ErrNoApp = parity.ErrNotConfigured

// Every capability is required. Source discovery does not exist yet; making
// all five mandatory means a missing discovery cannot drop a requirement, so
// the baseline may be marked complete. Capabilities without a probe are
// reported as probe_not_configured, which keeps Passed false: an honest
// record, and the reason this package never blocks.
var mandatory = []parity.Capability{parity.Identity, parity.Secret, parity.Audit, parity.Observability, parity.BackupRestore}

// SARFactory returns the SubjectAccessReview client of an environment's
// cluster, with the platform's own credentials for that cluster.
type SARFactory func(env string) (authorizationclient.SubjectAccessReviewInterface, error)

// Gate builds the probes for a plan's target and evaluates them.
type Gate struct {
	cfg  Config
	sar  SARFactory
	http *http.Client
}

// New returns a gate. sar may be nil (no identity probe); httpClient nil
// uses a client with a 10 second timeout.
func New(cfg Config, sar SARFactory, httpClient *http.Client) *Gate {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}
	return &Gate{cfg: cfg, sar: sar, http: httpClient}
}

// Check evaluates parity for starting p. The report is a record: callers
// must not turn Passed=false into a denial while the mode is audit.
func (g *Gate) Check(ctx context.Context, p *plan.Plan) (parity.Report, error) {
	if g == nil || p == nil {
		return parity.Report{Results: []parity.Result{}}, parity.ErrInvalidRequest
	}
	app, ok := g.cfg.appFor(p.Target.Namespace)
	if !ok {
		return parity.Report{Results: []parity.Result{}}, ErrNoApp
	}
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	target := parity.Target{Environment: p.Target.Env, Namespace: p.Target.Namespace, Application: app.Name}
	req := parity.Request{
		Source:    parity.Baseline{Target: parity.Target{Environment: p.Source.Env, Namespace: p.Source.Namespace, Application: app.Name}, Complete: true},
		Target:    target,
		Mandatory: mandatory,
	}
	return parity.Evaluate(ctx, req, g.probes(target, app))
}

// probes builds the configured probes for target. A probe that cannot be
// built is a failing probe (probe_failed), not a missing one, so the record
// says it was configured but could not run.
func (g *Gate) probes(target parity.Target, app App) map[parity.Capability]parity.Probe {
	out := map[parity.Capability]parity.Probe{}
	if len(app.RBAC) > 0 && g.sar != nil {
		out[parity.Identity] = g.rbac(target, app)
	}
	if e, ok := g.cfg.Environments[target.Environment]; ok && e.Prometheus != nil && app.PrometheusJob != "" {
		maxAge, _ := e.Prometheus.maxAge() // validated
		if p, err := probes.NewPrometheus(target, e.Prometheus.URL, app.PrometheusJob, g.http, maxAge); err == nil {
			out[parity.Observability] = p
		} else {
			out[parity.Observability] = failing{}
		}
	}
	return out
}

func (g *Gate) rbac(target parity.Target, app App) parity.Probe {
	client, err := g.sar(target.Environment)
	if err != nil || client == nil {
		return failing{}
	}
	rules := make([]probes.AccessRule, 0, len(app.RBAC))
	for _, r := range app.RBAC {
		ns := r.Namespace
		if ns == "" {
			ns = target.Namespace
		}
		rules = append(rules, probes.AccessRule{ID: r.ID, WantAllowed: *r.Allowed, Attributes: authorizationv1.ResourceAttributes{
			Namespace: ns, Verb: r.Verb, Group: r.Group, Resource: r.Resource, Subresource: r.Subresource, Name: r.Name,
		}})
	}
	p, err := probes.NewRBAC(target, client, app.ServiceAccount, rules)
	if err != nil {
		return failing{}
	}
	return p
}

// failing is a configured probe that could not be built.
type failing struct{}

func (failing) Check(context.Context, parity.Target) (parity.Result, error) {
	return parity.Result{}, errors.New("probe could not be built")
}

// Lines renders a report for the audit log, one line per capability:
// "identity pass rbac_verified: read-own-config=pass, deny-secrets=pass".
func Lines(r parity.Report) []string {
	out := make([]string, 0, len(r.Results))
	for _, res := range r.Results {
		line := fmt.Sprintf("%s %s %s", res.Capability, res.Status, res.Code)
		for i, c := range res.Checks {
			sep := ", "
			if i == 0 {
				sep = ": "
			}
			line += fmt.Sprintf("%s%s=%s(%s)", sep, c.ID, c.Status, c.Code)
		}
		out = append(out, line)
	}
	return out
}
