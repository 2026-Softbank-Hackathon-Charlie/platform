package security

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

// AuditReader is the part of the store the platform collector reads.
type AuditReader interface {
	ListAudit(ctx context.Context, before int64, limit int) ([]store.Audit, error)
}

// Limits of the audit log scan.
const (
	deniedWindow    = 24 * time.Hour
	auditPage       = 500
	auditMaxEntries = 20000 // a scan stops here and says so
)

// PlatformCollector reports the platform's own governance signals: requests
// the role model denied (from the audit log) and server settings that leave
// a guard off.
type PlatformCollector struct {
	audit           AuditReader
	dsnHostSuffixes []string
	gateway         bool
	now             func() time.Time
}

// NewPlatformCollector reads audit and the server's policy settings.
func NewPlatformCollector(audit AuditReader, dsnHostSuffixes []string, gatewayConfigured bool) *PlatformCollector {
	return &PlatformCollector{audit: audit, dsnHostSuffixes: dsnHostSuffixes, gateway: gatewayConfigured,
		now: func() time.Time { return time.Now().UTC() }}
}

func platformSource(name string) Source {
	return Source{Name: name, Provider: apiv1.SecurityProviderPlatform, Environment: EnvPlatform}
}

// Sources are the audit log and the server configuration.
func (c *PlatformCollector) Sources() []Source {
	return []Source{platformSource("platform-audit"), platformSource("platform-config")}
}

// Collect reads both sources.
func (c *PlatformCollector) Collect(ctx context.Context) []Report {
	return []Report{c.denied(ctx), c.config()}
}

type deniedKey struct {
	actor, role, action string
	status              int
}

type deniedGroup struct {
	deniedKey
	n           int
	first, last time.Time
	targets     []string
}

// denied groups the requests answered 401 or 403 in the last 24 hours by
// actor, role, action and status.
func (c *PlatformCollector) denied(ctx context.Context) Report {
	rep := Report{Source: platformSource("platform-audit"), Status: apiv1.SecuritySourceStatusOk}
	if c.audit == nil {
		rep.Status, rep.Message = apiv1.SecuritySourceStatusNotConfigured, "no audit log store"
		return rep
	}
	since := c.now().Add(-deniedWindow)
	groups := map[deniedKey]*deniedGroup{}
	scanned, complete := 0, false
	var before int64
	for !complete && scanned < auditMaxEntries {
		rows, err := c.audit.ListAudit(ctx, before, auditPage)
		if err != nil {
			rep.Status, rep.Message = apiv1.SecuritySourceStatusError, "read audit log: "+shorten(err.Error(), 160)
			return rep
		}
		for _, a := range rows {
			if a.Time.Before(since) {
				complete = true
				break
			}
			scanned++
			if a.Status != http.StatusUnauthorized && a.Status != http.StatusForbidden {
				continue
			}
			k := deniedKey{a.Actor, a.Role, a.Action, a.Status}
			g := groups[k]
			if g == nil {
				g = &deniedGroup{deniedKey: k, first: a.Time, last: a.Time}
				groups[k] = g
			}
			g.n++
			g.first = a.Time // newest first, so the last one seen is the earliest
			if len(g.targets) < 3 && !slices.Contains(g.targets, a.Target) {
				g.targets = append(g.targets, a.Target)
			}
		}
		if len(rows) < auditPage {
			complete = true
		} else if len(rows) > 0 {
			before = rows[len(rows)-1].ID
		}
	}
	list := make([]*deniedGroup, 0, len(groups))
	for _, g := range groups {
		list = append(list, g)
	}
	slices.SortFunc(list, func(a, b *deniedGroup) int { return cmp.Compare(b.n, a.n) })
	for _, g := range list {
		rep.Findings = append(rep.Findings, deniedFinding(g))
	}
	rep.Message = many(scanned, "audit entry", "audit entries") + " from the last 24h read"
	if !complete {
		rep.Message = "the newest " + many(scanned, "audit entry", "audit entries") + " read; older entries of the last 24h were not"
	}
	return rep
}

func deniedFinding(g *deniedGroup) apiv1.SecurityFinding {
	who := g.actor
	if g.role != "" {
		who += " (" + g.role + ")"
	}
	times := "once"
	if g.n > 1 {
		times = fmt.Sprintf("%d times", g.n)
	}
	f := apiv1.SecurityFinding{
		Check:      fmt.Sprintf("platform.denied-%d", g.status),
		Category:   apiv1.SecurityCategoryIdentity,
		Resource:   g.actor + ": " + g.action,
		ObservedAt: g.last,
		Detail: fmt.Sprintf("%s tried %s %s in the last 24h and got HTTP %d each time (first %s, last %s). Targets: %s.",
			who, g.action, times, g.status, g.first.Format(time.RFC3339), g.last.Format(time.RFC3339), strings.Join(g.targets, ", ")),
	}
	if g.status == http.StatusForbidden {
		f.Severity = apiv1.SecuritySeverityMedium
		f.Title = fmt.Sprintf("%s tried %s %s; blocked", who, g.action, times)
		f.Recommendation = "The role check held. If this caller should not attempt it, review the agent's tools or prompt; if it should, give it a token with the needed role."
	} else {
		f.Severity = apiv1.SecuritySeverityLow
		f.Title = fmt.Sprintf("%s called %s without a valid token %s; blocked", who, g.action, times)
		f.Recommendation = "Find the caller sending a missing or wrong token; rotate tokens if one may have leaked."
	}
	return f
}

// config reports guards that the server's settings leave off.
func (c *PlatformCollector) config() Report {
	rep := Report{Source: platformSource("platform-config"), Status: apiv1.SecuritySourceStatusOk, Message: "server settings read"}
	if len(c.dsnHostSuffixes) == 0 {
		rep.Findings = append(rep.Findings, apiv1.SecurityFinding{Check: "platform.dsn-hosts", Category: apiv1.SecurityCategoryData, Severity: apiv1.SecuritySeverityMedium,
			Title: "Plan database hosts are not restricted", Resource: "PLATFORM_DSN_HOST_SUFFIXES",
			Detail:         "PLATFORM_DSN_HOST_SUFFIXES is empty, so a submitted plan may point its DSN at any host and the server would send the database password there. Approvers must check dbHost in each plan.",
			Recommendation: "Set PLATFORM_DSN_HOST_SUFFIXES to the database host suffixes in use, e.g. .ts.net."})
	}
	if !c.gateway {
		rep.Findings = append(rep.Findings, apiv1.SecurityFinding{Check: "platform.gateway", Category: apiv1.SecurityCategoryNetwork, Severity: apiv1.SecuritySeverityInfo,
			Title: "No traffic gateway configured", Resource: "PLATFORM_GATEWAY_URL",
			Detail:         "PLATFORM_GATEWAY_URL is empty: canary reads, request counts and gateway-mode plans are off.",
			Recommendation: "Set PLATFORM_GATEWAY_URL and GATEWAY_TOKEN to the gateway admin API."})
	}
	return rep
}
