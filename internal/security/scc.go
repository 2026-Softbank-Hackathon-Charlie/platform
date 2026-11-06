package security

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/oauth2/google"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/envs"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

// SEC-03 (ADR 0034): active findings of Google Security Command Center for
// the GCP project the gke environment's storage uses, read with the server's
// Application Default Credentials (infra ADR 0066: findingsViewer only).

const (
	// EnvGCPProject is the environment of the project-level SCC source.
	EnvGCPProject = "gcp-project"
	// IssuerSCC is the severity issuer of SCC findings.
	IssuerSCC = "Google Security Command Center"
	sccBase   = "https://securitycenter.googleapis.com/v2"
)

var sccSource = Source{Name: "gcp-scc", Provider: apiv1.SecurityProviderGcp, Environment: EnvGCPProject}

// SCCProject is the GCP project of the first environment whose storage is
// the gcs driver, or "" when there is none.
func SCCProject(environments []envs.EnvConfig) string {
	for _, e := range environments {
		if e.Provider == "gcp" && e.Storage != nil && e.Storage.Project != "" {
			return e.Storage.Project
		}
	}
	return ""
}

// SCCCollector reads one project's SCC findings.
type SCCCollector struct {
	project string
	base    string
	client  func(context.Context) (*http.Client, error)
}

// NewSCCCollector reads project's findings with Application Default Credentials.
func NewSCCCollector(project string) *SCCCollector {
	return &SCCCollector{project: project, base: sccBase, client: func(ctx context.Context) (*http.Client, error) {
		c, err := google.DefaultClient(ctx, "https://www.googleapis.com/auth/cloud-platform")
		if err != nil {
			return nil, err
		}
		c.Timeout = 20 * time.Second
		return c, nil
	}}
}

func (c *SCCCollector) Sources() []Source { return []Source{sccSource} }

type sccList struct {
	Results []struct {
		Finding  sccFinding  `json:"finding"`
		Resource sccResource `json:"resource"`
	} `json:"listFindingsResults"`
	NextPageToken string `json:"nextPageToken"`
}

type sccFinding struct {
	Name         string    `json:"name"`
	ResourceName string    `json:"resourceName"`
	Category     string    `json:"category"`
	Severity     string    `json:"severity"`
	FindingClass string    `json:"findingClass"`
	Description  string    `json:"description"`
	NextSteps    string    `json:"nextSteps"`
	ExternalURI  string    `json:"externalUri"`
	EventTime    time.Time `json:"eventTime"`
}

type sccResource struct {
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	Type        string `json:"type"`
}

// sccHTTPError is a non-2xx answer, with Google's status name.
type sccHTTPError struct {
	code   int
	status string
	msg    string
}

func (e *sccHTTPError) Error() string { return fmt.Sprintf("HTTP %d %s", e.code, e.status) }

func (c *SCCCollector) Collect(ctx context.Context) []Report {
	p := newPartial(sccSource)
	hc, err := c.client(ctx)
	if err != nil {
		return []Report{{Source: sccSource, Status: apiv1.SecuritySourceStatusError,
			Message: "no Google Application Default Credentials: " + shorten(err.Error(), 160)}}
	}
	n := 0
	// Most severe first, as for Security Hub: one query per severity.
	for _, sev := range []string{"CRITICAL", "HIGH", "MEDIUM", "LOW"} {
		token := ""
		for n < maxVendorFinds {
			list, err := c.list(ctx, hc, sev, maxVendorFinds-n, token)
			if err != nil {
				st, msg := sccError(err)
				p.mu.Lock()
				p.errs, p.status = append(p.errs, msg), st
				p.mu.Unlock()
				return []Report{p.report()}
			}
			for _, r := range list.Results {
				if n == maxVendorFinds {
					break
				}
				p.add(sccToFinding(r.Finding, r.Resource))
				n++
			}
			if token = list.NextPageToken; token == "" {
				break
			}
		}
	}
	p.message = fmt.Sprintf("project %s: %s read (active, not muted, most severe first, at most %d)", c.project, many(n, "finding", "findings"), maxVendorFinds)
	return []Report{p.report()}
}

func (c *SCCCollector) list(ctx context.Context, hc *http.Client, severity string, size int, token string) (sccList, error) {
	q := url.Values{}
	q.Set("filter", fmt.Sprintf(`state="ACTIVE" AND NOT mute="MUTED" AND severity=%q`, severity))
	q.Set("pageSize", fmt.Sprint(size))
	if token != "" {
		q.Set("pageToken", token)
	}
	u := c.base + "/projects/" + url.PathEscape(c.project) + "/sources/-/locations/global/findings?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return sccList{}, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return sccList{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return sccList{}, err
	}
	if resp.StatusCode/100 != 2 {
		var e struct {
			Error struct {
				Status  string `json:"status"`
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(body, &e)
		return sccList{}, &sccHTTPError{code: resp.StatusCode, status: e.Error.Status, msg: e.Error.Message}
	}
	var out sccList
	if err := json.Unmarshal(body, &out); err != nil {
		return sccList{}, fmt.Errorf("unreadable response: %w", err)
	}
	return out, nil
}

// sccError turns a failed read into a status and a message that says what to
// fix. Nothing is counted for the source.
func sccError(err error) (apiv1.SecuritySourceStatus, string) {
	const op = "securitycenter findings.list"
	var he *sccHTTPError
	var ne net.Error
	switch {
	case errors.As(err, &he) && he.code == http.StatusForbidden && (strings.Contains(he.msg, "has not been used") || strings.Contains(he.msg, "is disabled")):
		return apiv1.SecuritySourceStatusNotConfigured, op + ": the Security Command Center API is disabled in the project (infra ADR 0066)"
	case errors.As(err, &he) && (he.code == http.StatusForbidden || he.code == http.StatusUnauthorized):
		return apiv1.SecuritySourceStatusError, fmt.Sprintf("%s: access denied (%s); the platform's GCP identity needs roles/securitycenter.findingsViewer, and SCC must be activated for the project: %s", op, he.Error(), shorten(he.msg, 160))
	// Not activated: Google answers 400 INVALID_ARGUMENT naming "Security
	// Command Center Legacy" (seen 2026-11-06), or FAILED_PRECONDITION.
	case errors.As(err, &he) && (he.status == "FAILED_PRECONDITION" || he.code == http.StatusNotFound ||
		he.code == http.StatusBadRequest && strings.Contains(he.msg, "Security Command Center Legacy")):
		return apiv1.SecuritySourceStatusNotConfigured, fmt.Sprintf("%s: Security Command Center is not activated for the project (%s): %s", op, he.Error(), shorten(he.msg, 160))
	case errors.As(err, &he):
		return apiv1.SecuritySourceStatusError, fmt.Sprintf("%s: %s: %s", op, he.Error(), shorten(he.msg, 160))
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled):
		return apiv1.SecuritySourceStatusUnavailable, op + ": timed out"
	case errors.As(err, &ne):
		return apiv1.SecuritySourceStatusUnavailable, op + ": " + shorten(ne.Error(), 160)
	}
	return apiv1.SecuritySourceStatusError, op + ": " + shorten(err.Error(), 160)
}

var sccSeverity = map[string]apiv1.SecuritySeverity{
	"CRITICAL": apiv1.SecuritySeverityCritical, "HIGH": apiv1.SecuritySeverityHigh,
	"MEDIUM": apiv1.SecuritySeverityMedium, "LOW": apiv1.SecuritySeverityLow,
}

func sccToFinding(f sccFinding, r sccResource) apiv1.SecurityFinding {
	basis := Basis(apiv1.SecuritySeverityBasisTypeVendor, IssuerSCC, "the finding's own severity")
	basis.SourceFindingId = &f.Name
	sev, ok := sccSeverity[f.Severity]
	if !ok {
		sev = apiv1.SecuritySeverityInfo
	}
	if f.Severity != "" {
		raw := f.Severity
		basis.SourceSeverity = &raw
	}
	resource := r.Name
	if resource == "" {
		resource = f.ResourceName
	}
	title := f.Category
	if r.DisplayName != "" {
		title += " on " + r.DisplayName
	}
	rec := strings.TrimSpace(f.NextSteps)
	if f.ExternalURI != "" {
		rec = strings.TrimSpace(rec + " " + f.ExternalURI)
	}
	if rec == "" {
		rec = "See the finding in Security Command Center."
	}
	return apiv1.SecurityFinding{
		SeverityBasis:  &basis,
		Id:             f.Name,
		Check:          "scc." + f.Category,
		Category:       sccCategory(f.FindingClass, r.Type),
		Severity:       sev,
		Title:          title,
		Resource:       resource,
		Detail:         shorten(f.Description, 400),
		Recommendation: shorten(rec, 400),
		ObservedAt:     f.EventTime.UTC(),
	}
}

// sccCategory maps a finding class and resource type to a category.
func sccCategory(class, resourceType string) apiv1.SecurityCategory {
	if class == "THREAT" {
		return apiv1.SecurityCategoryThreat
	}
	t := strings.ToLower(resourceType)
	has := func(parts ...string) bool {
		for _, p := range parts {
			if strings.Contains(t, p) {
				return true
			}
		}
		return false
	}
	switch {
	case has("iam.", "serviceaccount", "resourcemanager.project", "resourcemanager.organization"):
		return apiv1.SecurityCategoryIdentity
	case has("compute.firewall", "compute.network", "compute.subnetwork", "compute.address", "compute.forwardingrule", "dns."):
		return apiv1.SecurityCategoryNetwork
	case has("storage.", "cloud.sql.", "sqladmin.", "kms.", "bigquery.", "secretmanager.", "compute.disk"):
		return apiv1.SecurityCategoryData
	case has("logging.", "monitoring."):
		return apiv1.SecurityCategoryLogging
	}
	return apiv1.SecurityCategoryWorkload
}
