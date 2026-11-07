package paritygate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	"k8s.io/client-go/dynamic"
	authorizationclient "k8s.io/client-go/kubernetes/typed/authorization/v1"
	"k8s.io/client-go/metadata"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/preflight/parity"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/preflight/probes"
)

// Timeout bounds one whole check, so recording parity never holds a start
// for long.
const Timeout = 30 * time.Second

// ErrNoApp: the plan's target namespace has no app in the parity file.
var ErrNoApp = parity.ErrNotConfigured

// Every capability is required, so missing discovery cannot drop a requirement.
// This fixed minimum is evaluated to preserve partial probe observations; the
// separately checked operator baseline must also be current before Passed can
// be true. Neither is a claim of automatic source dependency discovery.
var mandatory = []parity.Capability{parity.Identity, parity.Secret, parity.Audit, parity.Observability, parity.BackupRestore}

// SARFactory returns the SubjectAccessReview client of an environment's
// cluster, with the platform's own credentials for that cluster.
type SARFactory func(env string) (authorizationclient.SubjectAccessReviewInterface, error)

// ResourceFactory binds read clients to a registered environment's kubeconfig.
// It must not choose a cluster from plan-supplied kubeconfig or context fields.
type ResourceFactory func(env string) (metadata.Interface, dynamic.Interface, error)

// Gate builds the probes for a plan's target and evaluates them.
type Gate struct {
	digest     string
	cfg        Config
	sar        SARFactory
	http       *http.Client
	resources  ResourceFactory
	usageFetch probes.UsageFetch
}

func (g *Gate) SetUsageFetcher(fetch probes.UsageFetch) { g.usageFetch = fetch }

// New returns a gate. sar may be nil (no identity probe); httpClient nil
// uses a client with a 10 second timeout.
func New(cfg Config, sar SARFactory, httpClient *http.Client) *Gate {
	// Own a deep snapshot: later caller mutations cannot change the policy
	// behind an already recorded fingerprint.
	raw, _ := json.Marshal(cfg)
	var owned Config
	_ = json.Unmarshal(raw, &owned)
	sum := sha256.Sum256(raw)
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}
	return &Gate{cfg: owned, sar: sar, http: httpClient, digest: hex.EncodeToString(sum[:])}
}

// NewWithResources adds metadata-only Secret and dynamic resource readers.
// New remains available for callers that only configure RBAC and Prometheus.
func NewWithResources(cfg Config, sar SARFactory, httpClient *http.Client, resources ResourceFactory) *Gate {
	g := New(cfg, sar, httpClient)
	g.resources = resources
	return g
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
	report, err := parity.Evaluate(ctx, req, g.probes(target, app, p.Target.Cluster))
	report.ConfigDigest = g.digest
	report.BaselineStatus = "not_configured"
	if b, ok := app.Baselines[p.Source.Env]; ok {
		report.BaselineStatus = "expired_or_future"
		now := time.Now().UTC()
		if !b.ReviewedAt.After(now) && now.Before(b.ExpiresAt) {
			report.BaselineStatus = "operator_reviewed"
			if b.ExpiresAt.Before(report.ExpiresAt) {
				report.ExpiresAt = b.ExpiresAt
			}
		}
	}
	if p.Source.Namespace != app.Namespace {
		report.BaselineStatus = "source_scope_mismatch"
	}
	if report.BaselineStatus != "operator_reviewed" {
		report.Passed = false
	}
	return report, err
}

// probes builds the configured probes for target. A probe that cannot be
// built is a failing probe (probe_failed), not a missing one, so the record
// says it was configured but could not run.
func (g *Gate) probes(target parity.Target, app App, targetCluster string) map[parity.Capability]parity.Probe {
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
	resources := app.Environments[target.Environment]
	collector := g.cfg.Environments[target.Environment].AuditCollector
	if len(resources.Secrets) == 0 && resources.Backup == nil && collector == nil {
		return out
	}
	var meta metadata.Interface
	var dyn dynamic.Interface
	if g.resources != nil {
		var err error
		meta, dyn, err = g.resources(target.Environment)
		if err != nil {
			meta, dyn = nil, nil
		}
	}
	if len(resources.Secrets) > 0 {
		out[parity.Secret] = failing{}
		if p, err := probes.NewSecrets(target, meta, resources.Secrets); err == nil {
			if resources.SecretUsage != nil {
				if err := p.WithUsage(*resources.SecretUsage, g.usageFetch); err != nil {
					return out
				}
			}
			out[parity.Secret] = p
		}
	}
	if collector != nil {
		out[parity.Audit] = failing{}
		if p, err := probes.NewAuditCollector(target, dyn, collector.Namespace, collector.Deployment); err == nil {
			out[parity.Audit] = p
		}
	}
	if b := resources.Backup; b != nil {
		out[parity.BackupRestore] = failing{}
		age, _ := b.maxAge()
		if b.Cluster != targetCluster {
			out[parity.BackupRestore] = backupTargetMismatch{}
		} else if p, err := probes.NewBackup(target, dyn, b.Cluster, age); err == nil {
			out[parity.BackupRestore] = p
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

type backupTargetMismatch struct{}

func (backupTargetMismatch) Check(_ context.Context, target parity.Target) (parity.Result, error) {
	return parity.Result{Capability: parity.BackupRestore, Target: target, Status: parity.Unknown,
		Code: "backup_target_mismatch", ObservedAt: time.Now().UTC(), Checks: []parity.Check{
			{ID: "backup-config", Status: parity.Unknown, Code: "backup_target_mismatch"},
		}}, nil
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
