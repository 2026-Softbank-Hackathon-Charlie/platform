package security

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/envs"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/storage"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

func sccTestCollector(t *testing.T, h http.HandlerFunc) *SCCCollector {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &SCCCollector{project: "proj-1", base: srv.URL + "/v2", client: func(context.Context) (*http.Client, error) { return srv.Client(), nil }}
}

const sccHighPage = `{"listFindingsResults": [{
  "finding": {"name": "projects/1/sources/2/locations/global/findings/f1", "resourceName": "//storage.googleapis.com/b1",
    "category": "PUBLIC_BUCKET_ACL", "severity": "HIGH", "findingClass": "MISCONFIGURATION",
    "description": "Bucket is public.", "nextSteps": "Remove allUsers.", "externalUri": "https://console.cloud.google.com/x",
    "eventTime": "2026-11-06T01:02:03Z"},
  "resource": {"name": "//storage.googleapis.com/b1", "displayName": "b1", "type": "google.cloud.storage.Bucket"}}]}`

func TestSCCReadsMostSevereFirstAndKeepsTheSourceSeverity(t *testing.T) {
	var filters []string
	c := sccTestCollector(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/projects/proj-1/sources/-/locations/global/findings" {
			t.Errorf("path %s", r.URL.Path)
		}
		f := r.URL.Query().Get("filter")
		filters = append(filters, f)
		if strings.Contains(f, `severity="HIGH"`) {
			fmt.Fprint(w, sccHighPage)
			return
		}
		fmt.Fprint(w, `{}`)
	})
	reps := c.Collect(t.Context())
	if len(reps) != 1 || reps[0].Status != apiv1.SecuritySourceStatusOk || reps[0].Source != sccSource {
		t.Fatalf("report: %+v", reps)
	}
	if len(filters) != 4 || !strings.Contains(filters[0], `severity="CRITICAL"`) || !strings.Contains(filters[3], `severity="LOW"`) ||
		!strings.Contains(filters[0], `state="ACTIVE" AND NOT mute="MUTED"`) {
		t.Fatalf("filters: %q", filters)
	}
	if len(reps[0].Findings) != 1 {
		t.Fatalf("findings: %+v", reps[0].Findings)
	}
	f := reps[0].Findings[0]
	if f.Severity != apiv1.SecuritySeverityHigh || f.Category != apiv1.SecurityCategoryData || f.Check != "scc.PUBLIC_BUCKET_ACL" ||
		f.Resource != "//storage.googleapis.com/b1" || f.Title != "PUBLIC_BUCKET_ACL on b1" || f.ObservedAt.IsZero() ||
		f.Recommendation != "Remove allUsers. https://console.cloud.google.com/x" {
		t.Fatalf("finding: %+v", f)
	}
	b := f.SeverityBasis
	if b == nil || b.Type != apiv1.SecuritySeverityBasisTypeVendor || b.Issuer != IssuerSCC || b.SourceSeverity == nil || *b.SourceSeverity != "HIGH" ||
		b.SourceFindingId == nil || *b.SourceFindingId != "projects/1/sources/2/locations/global/findings/f1" {
		t.Fatalf("basis: %+v", b)
	}
	if !strings.Contains(reps[0].Message, "1 finding read") {
		t.Fatalf("message: %s", reps[0].Message)
	}
}

func TestSCCStopsAtTheCapAcrossPages(t *testing.T) {
	calls := 0
	c := sccTestCollector(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if !strings.Contains(r.URL.Query().Get("filter"), "CRITICAL") {
			t.Errorf("read past the cap: %s", r.URL.RawQuery)
		}
		var b strings.Builder
		b.WriteString(`{"nextPageToken": "more", "listFindingsResults": [`)
		for i := range 30 {
			if i > 0 {
				b.WriteString(",")
			}
			fmt.Fprintf(&b, `{"finding": {"name": "f%d-%s", "category": "X", "severity": "CRITICAL"}, "resource": {}}`, i, r.URL.Query().Get("pageToken"))
		}
		b.WriteString("]}")
		fmt.Fprint(w, b.String())
	})
	reps := c.Collect(t.Context())
	if len(reps[0].Findings) != maxVendorFinds || calls != 2 {
		t.Fatalf("findings=%d calls=%d", len(reps[0].Findings), calls)
	}
}

func TestSCCFailuresAreNeverZeroFindings(t *testing.T) {
	for name, tc := range map[string]struct {
		code   int
		body   string
		status apiv1.SecuritySourceStatus
		want   string
	}{
		"api disabled": {403, `{"error": {"status": "PERMISSION_DENIED", "message": "Security Command Center API has not been used in project 1 before or it is disabled."}}`,
			apiv1.SecuritySourceStatusNotConfigured, "API is disabled"},
		"no role": {403, `{"error": {"status": "PERMISSION_DENIED", "message": "The caller does not have permission"}}`,
			apiv1.SecuritySourceStatusError, "findingsViewer"},
		"not activated": {400, `{"error": {"status": "FAILED_PRECONDITION", "message": "Security Command Center is not activated"}}`,
			apiv1.SecuritySourceStatusNotConfigured, "not activated"},
		"legacy, not activated": {400, `{"error": {"status": "INVALID_ARGUMENT", "message": "Security Command Center Legacy has been permanently disabled as of June 7, 2021. Migrate to Security Command Center's Standard tier or Premium tier."}}`,
			apiv1.SecuritySourceStatusNotConfigured, "not activated"},
		"server error": {500, `{"error": {"status": "INTERNAL", "message": "boom"}}`, apiv1.SecuritySourceStatusError, "HTTP 500"},
	} {
		t.Run(name, func(t *testing.T) {
			c := sccTestCollector(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.code)
				fmt.Fprint(w, tc.body)
			})
			rep := c.Collect(t.Context())[0]
			if rep.Status != tc.status || !strings.Contains(rep.Message, tc.want) || len(rep.Findings) != 0 {
				t.Fatalf("%+v", rep)
			}
		})
	}
	c := &SCCCollector{project: "p", base: "x", client: func(context.Context) (*http.Client, error) { return nil, fmt.Errorf("no adc") }}
	if rep := c.Collect(t.Context())[0]; rep.Status != apiv1.SecuritySourceStatusError || !strings.Contains(rep.Message, "Application Default Credentials") {
		t.Fatalf("%+v", rep)
	}
}

func TestSCCProjectComesFromTheGCPStorage(t *testing.T) {
	envsWith := []envs.EnvConfig{
		{Name: "eks", Provider: "aws", Storage: &storage.Config{Driver: "aws"}},
		{Name: "gke", Provider: "gcp", Storage: &storage.Config{Driver: "gcs", Project: "proj-9"}},
	}
	if got := SCCProject(envsWith); got != "proj-9" {
		t.Fatalf("project %q", got)
	}
	if got := SCCProject(envsWith[:1]); got != "" {
		t.Fatalf("project %q", got)
	}
	if _, ok := sccCollector(envsWith[:1]).(notConfigured); !ok {
		t.Fatal("no project must stay not configured")
	}
	if c, ok := sccCollector(envsWith).(*SCCCollector); !ok || c.project != "proj-9" {
		t.Fatal("the gke storage project must be read")
	}
}

func TestSCCCategory(t *testing.T) {
	for in, want := range map[[2]string]apiv1.SecurityCategory{
		{"THREAT", "google.cloud.storage.Bucket"}:                    apiv1.SecurityCategoryThreat,
		{"MISCONFIGURATION", "google.iam.ServiceAccount"}:            apiv1.SecurityCategoryIdentity,
		{"MISCONFIGURATION", "google.compute.Firewall"}:              apiv1.SecurityCategoryNetwork,
		{"MISCONFIGURATION", "google.cloud.sql.Instance"}:            apiv1.SecurityCategoryData,
		{"VULNERABILITY", "google.container.Cluster"}:                apiv1.SecurityCategoryWorkload,
		{"MISCONFIGURATION", "google.logging.LogSink"}:               apiv1.SecurityCategoryLogging,
		{"MISCONFIGURATION", "google.cloud.kms.CryptoKey"}:           apiv1.SecurityCategoryData,
		{"MISCONFIGURATION", "google.cloud.storage.Bucket"}:          apiv1.SecurityCategoryData,
		{"MISCONFIGURATION", "google.cloud.resourcemanager.Project"}: apiv1.SecurityCategoryIdentity,
	} {
		if got := sccCategory(in[0], in[1]); got != want {
			t.Errorf("%v: %s, want %s", in, got, want)
		}
	}
}
