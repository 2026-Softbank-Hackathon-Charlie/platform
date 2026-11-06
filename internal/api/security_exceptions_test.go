package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/runs"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/security"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

// kevServer serves a KEV catalog that lists the CVE of scanReport and an
// EPSS API with no scores.
func kevServer(t *testing.T) *security.ExploitIntel {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/kev":
			fmt.Fprint(w, `{"catalogVersion": "2026.11.04", "vulnerabilities": [{"cveID": "CVE-2024-6387", "dateAdded": "2026-11-01", "dueDate": "2026-11-18"}]}`)
		default:
			fmt.Fprint(w, `{"status": "OK", "data": []}`)
		}
	}))
	t.Cleanup(srv.Close)
	return security.NewExploitIntelFrom(srv.URL+"/kev", srv.URL+"/epss", srv.Client())
}

func exceptionRequest(target apiv1.SecurityExceptionTarget, id, reason string, expires time.Duration) map[string]any {
	return map[string]any{"targetType": target, "targetId": id, "reason": reason, "expiresAt": time.Now().Add(expires).UTC()}
}

func TestSecurityExceptions(t *testing.T) {
	digest := "sha256:" + strings.Repeat("1", 64)
	h := newHarness(t, func(h *harness, _ *runs.Config, d *Deps) {
		d.Security = security.New([]security.Collector{
			imagesCollector{src: security.Source{Name: "kubernetes", Provider: apiv1.SecurityProviderAws, Environment: "eks"},
				images: []apiv1.SecurityRunningImage{{Environment: "eks", Namespace: "shop", Workload: "Deployment/shop-web", Container: "web", Image: "r/web:v1", Digest: &digest}}},
			clusterCollector{src: security.Source{Name: "kubernetes", Provider: apiv1.SecurityProviderGcp, Environment: "gke"},
				findings: []apiv1.SecurityFinding{{Check: "k8s.privileged", Category: apiv1.SecurityCategoryWorkload, Severity: apiv1.SecuritySeverityHigh,
					Title: "Privileged container", Resource: "shop/Deployment/web/app", Detail: "d", Recommendation: "r"}}},
		}, security.Options{})
		d.ImageScans = security.StoredScans{Store: h.store}
		d.Exploits = kevServer(t)
	})
	scan := map[string]any{"image": "r/web@" + digest, "digest": digest, "status": "succeeded", "report": json.RawMessage(scanReport)}
	if code, b := h.do("POST", "/v1/security/scans", agentToken, scan); code != 201 {
		t.Fatalf("scan: %d %s", code, b)
	}
	// Building the report reads the KEV catalog into the cache.
	_, b := h.do("GET", "/v1/security/vulnerabilities", viewerToken, nil)
	vulns := decode[apiv1.SecurityVulnerabilityReport](t, b).Vulnerabilities
	if len(vulns) != 1 || vulns[0].Exploitation == nil || vulns[0].Exploitation.Kev.State != apiv1.SecurityKEVStateListed || vulns[0].Exception != nil {
		t.Fatalf("vulnerabilities: %s", b)
	}
	vulnID := vulns[0].Id
	_, b = h.do("GET", "/v1/security", viewerToken, nil)
	findings := decode[apiv1.SecurityReport](t, b).Findings
	if len(findings) != 1 {
		t.Fatalf("findings: %s", b)
	}
	findingID := findings[0].Id

	vuln := apiv1.SecurityExceptionTargetVulnerability
	ok := exceptionRequest(vuln, vulnID, "  patched in the next release  ", 7*24*time.Hour-time.Minute)
	for _, tok := range []string{viewerToken, agentToken} {
		if code, _ := h.do("POST", "/v1/security/exceptions", tok, ok); code != 403 {
			t.Fatalf("POST as non-operator: %d", code)
		}
		if code, _ := h.do("DELETE", "/v1/security/exceptions/sx_0000000000000000", tok, nil); code != 403 {
			t.Fatalf("DELETE as non-operator: %d", code)
		}
	}
	for name, bad := range map[string]map[string]any{
		"empty reason":          exceptionRequest(vuln, vulnID, "   ", time.Hour),
		"long reason":           exceptionRequest(vuln, vulnID, strings.Repeat("가", 501), time.Hour),
		"past":                  exceptionRequest(vuln, vulnID, "r", -time.Minute),
		"31 days":               exceptionRequest(apiv1.SecurityExceptionTargetFinding, findingID, "r", 31*24*time.Hour),
		"KEV 8 days":            exceptionRequest(vuln, vulnID, "r", 8*24*time.Hour),
		"unknown vulnerability": exceptionRequest(vuln, "v_missing", "r", time.Hour),
		"unknown target type":   exceptionRequest("image", vulnID, "r", time.Hour),
		"no target id":          exceptionRequest(apiv1.SecurityExceptionTargetFinding, "", "r", time.Hour),
	} {
		if code, b := h.do("POST", "/v1/security/exceptions", opToken, bad); code != 400 || !strings.Contains(string(b), "invalid_request") {
			t.Errorf("%s: %d %s", name, code, b)
		}
	}
	// 500 characters after trimming is allowed (a finding allows 30 days).
	code, b := h.do("POST", "/v1/security/exceptions", opToken, exceptionRequest(apiv1.SecurityExceptionTargetFinding, findingID, " "+strings.Repeat("가", 500)+" ", 30*24*time.Hour-time.Minute))
	if code != 201 {
		t.Fatalf("finding: %d %s", code, b)
	}
	code, b = h.do("POST", "/v1/security/exceptions", opToken, ok)
	created := decode[apiv1.SecurityException](t, b)
	if code != 201 || !strings.HasPrefix(created.Id, "sx_") || len(created.Id) != 19 || created.Reason != "patched in the next release" ||
		created.CreatedBy != "op" || created.TargetId != vulnID || created.RevokedAt != nil {
		t.Fatalf("create: %d %s", code, b)
	}
	if code, b := h.do("POST", "/v1/security/exceptions", opToken, ok); code != 409 {
		t.Fatalf("duplicate: %d %s", code, b)
	}

	// Reports show the active exceptions; severity stays.
	_, b = h.do("GET", "/v1/security/vulnerabilities", viewerToken, nil)
	v := decode[apiv1.SecurityVulnerabilityReport](t, b).Vulnerabilities[0]
	if v.Exception == nil || v.Exception.Id != created.Id || v.Severity != apiv1.SecurityVulnerabilitySeverityHigh {
		t.Fatalf("vulnerability exception: %s", b)
	}
	_, b = h.do("GET", "/v1/security", viewerToken, nil)
	if f := decode[apiv1.SecurityReport](t, b).Findings[0]; f.Exception == nil || f.Severity != apiv1.SecuritySeverityHigh {
		t.Fatalf("finding exception: %s", b)
	}
	_, b = h.do("GET", "/v1/security/ocsf", viewerToken, nil)
	var exp struct{ Findings []security.OCSFFinding }
	if err := json.Unmarshal(b, &exp); err != nil || len(exp.Findings) != 1 || exp.Findings[0].StatusID != 3 || exp.Findings[0].Status != "Suppressed" ||
		exp.Findings[0].StatusDetail != strings.Repeat("가", 500) {
		t.Fatalf("ocsf: %s", b)
	}
	code, b = h.do("GET", "/v1/security/exceptions", viewerToken, nil)
	// Both were created within the clock's resolution, so their order is
	// the store's tie-break; the store conformance tests check newest first.
	if list := decode[apiv1.SecurityExceptionList](t, b).Exceptions; code != 200 || len(list) != 2 || (list[0].Id != created.Id && list[1].Id != created.Id) {
		t.Fatalf("list: %d %s", code, b)
	}

	// Revoke.
	if code, _ := h.do("DELETE", "/v1/security/exceptions/sx_missing", opToken, nil); code != 404 {
		t.Fatalf("unknown id: %d", code)
	}
	code, b = h.do("DELETE", "/v1/security/exceptions/"+created.Id, opToken, nil)
	if r := decode[apiv1.SecurityException](t, b); code != 200 || r.RevokedAt == nil || r.RevokedBy == nil || *r.RevokedBy != "op" {
		t.Fatalf("revoke: %d %s", code, b)
	}
	if code, _ := h.do("DELETE", "/v1/security/exceptions/"+created.Id, opToken, nil); code != 409 {
		t.Fatalf("revoke twice: %d", code)
	}
	_, b = h.do("GET", "/v1/security/vulnerabilities", viewerToken, nil)
	if v := decode[apiv1.SecurityVulnerabilityReport](t, b).Vulnerabilities[0]; v.Exception != nil {
		t.Fatalf("revoked exception still shown: %s", b)
	}

	// Both changes are audited with what changed.
	audit, _ := h.store.ListAudit(t.Context(), 0, 0)
	var sawCreate, sawRevoke bool
	for _, a := range audit {
		d := string(a.Detail)
		switch {
		case a.Action == "POST /v1/security/exceptions" && a.Status == 201 && strings.Contains(d, `"exception":`) && strings.Contains(d, vulnID) && strings.Contains(d, `"expiresAt"`):
			sawCreate = true
		case a.Action == "DELETE /v1/security/exceptions/{id}" && a.Status == 200 && strings.Contains(d, created.Id) && strings.Contains(d, vulnID):
			sawRevoke = true
		}
	}
	if !sawCreate || !sawRevoke {
		t.Fatalf("audit (create %v, revoke %v): %+v", sawCreate, sawRevoke, audit)
	}
}

// Only the read is tried without a store: the audit log of a change needs
// one, as on every other mutating route.
func TestSecurityExceptionsNeedAStore(t *testing.T) {
	h := newHarness(t, func(_ *harness, _ *runs.Config, d *Deps) { d.Store = nil })
	if code, _ := h.do("GET", "/v1/security/exceptions", viewerToken, nil); code != 503 {
		t.Fatalf("GET: %d", code)
	}
}

// exceptionsDown is a store whose exceptions cannot be read.
type exceptionsDown struct{ store.Store }

func (exceptionsDown) ListSecurityExceptions(context.Context) ([]store.SecurityException, error) {
	return nil, errors.New("database is down")
}

func TestSecurityReportsWithoutReadableExceptions(t *testing.T) {
	h := newHarness(t, withSecurity(), func(_ *harness, _ *runs.Config, d *Deps) { d.Store = exceptionsDown{d.Store} })
	for _, path := range []string{"/v1/security", "/v1/security/ocsf"} {
		if code, b := h.do("GET", path, viewerToken, nil); code != 200 || !strings.Contains(string(b), "k8s.privileged") {
			t.Fatalf("%s: %d %s", path, code, b)
		}
	}
}
