package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/runs"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/security"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

// imagesCollector is a cluster that runs fixed images (tests only).
type imagesCollector struct {
	src    security.Source
	images []apiv1.SecurityRunningImage
}

func (c imagesCollector) Sources() []security.Source { return []security.Source{c.src} }

func (c imagesCollector) Collect(context.Context) []security.Report {
	return []security.Report{{Source: c.src, Status: apiv1.SecuritySourceStatusOk, Images: c.images}}
}

const scanReport = `{"matches": [{"vulnerability": {"id": "CVE-2024-6387", "namespace": "debian:distro:debian:12", "severity": "High",
 "urls": [], "cvss": [], "fix": {"versions": ["1:9.2p1-2+deb12u3"], "state": "fixed"}}, "relatedVulnerabilities": [],
 "artifact": {"name": "openssh-client", "version": "1:9.2p1-2+deb12u2", "type": "deb"}}],
 "distro": {"name": "debian", "version": "12"}, "descriptor": {"name": "grype", "version": "0.121.0"}}`

func TestSubmitSecurityScan(t *testing.T) {
	d1, d2 := "sha256:"+strings.Repeat("1", 64), "sha256:"+strings.Repeat("2", 64)
	h := newHarness(t, func(h *harness, _ *runs.Config, d *Deps) {
		d.Security = security.New([]security.Collector{imagesCollector{
			src: security.Source{Name: "kubernetes", Provider: apiv1.SecurityProviderAws, Environment: "eks"},
			images: []apiv1.SecurityRunningImage{
				{Environment: "eks", Namespace: "shop", Workload: "Deployment/shop-web", Container: "web", Image: "r/web:v1", Digest: &d1},
				{Environment: "eks", Namespace: "shop", Workload: "Deployment/shop-api", Container: "api", Image: "r/api:v1", Digest: &d2},
			}}}, security.Options{})
		d.ImageScans = security.StoredScans{Store: h.store}
	})
	ok := map[string]any{"image": "r/web@" + d1, "digest": d1, "status": "succeeded", "report": json.RawMessage(scanReport)}
	if code, _ := h.do("POST", "/v1/security/scans", viewerToken, ok); code != 403 {
		t.Fatalf("a viewer posted a scan: %d", code)
	}
	for name, bad := range map[string]map[string]any{
		"digest":             {"image": "r/web@sha256:abc", "digest": "sha256:abc", "status": "succeeded", "report": json.RawMessage(scanReport)},
		"image":              {"image": "r/web:v1", "digest": d1, "status": "succeeded", "report": json.RawMessage(scanReport)},
		"no report":          {"image": "r/web@" + d1, "digest": d1, "status": "succeeded"},
		"failed with report": {"image": "r/web@" + d1, "digest": d1, "status": "failed", "message": "x", "report": json.RawMessage(scanReport)},
		"failed no message":  {"image": "r/web@" + d1, "digest": d1, "status": "failed"},
		"other scanner":      {"image": "r/web@" + d1, "digest": d1, "status": "succeeded", "report": json.RawMessage(`{"descriptor": {"name": "trivy", "version": "1"}}`)},
		"unknown field":      {"image": "r/web@" + d1, "digest": d1, "status": "failed", "message": "x", "severity": "low"},
	} {
		if code, b := h.do("POST", "/v1/security/scans", agentToken, bad); code != 400 {
			t.Errorf("%s: %d %s", name, code, b)
		}
	}
	code, b := h.do("POST", "/v1/security/scans", agentToken, ok)
	scan := decode[apiv1.SecurityImageScan](t, b)
	if code != 201 || scan.Status != apiv1.SecurityImageScanStatusSucceeded || scan.Scanner.Version != "0.121.0" || scan.ResultSha256 == nil {
		t.Fatalf("submit: %d %s", code, b)
	}
	older := time.Now().Add(-time.Hour)
	failed := map[string]any{"image": "r/api@" + d2, "digest": d2, "status": "failed", "message": "unauthorized: pull denied", "finishedAt": older}
	if code, b := h.do("POST", "/v1/security/scans", agentToken, failed); code != 201 {
		t.Fatalf("failed scan: %d %s", code, b)
	}
	// A retry that finished before the stored scan does not replace it.
	failed["finishedAt"] = older.Add(-time.Minute)
	if code, b := h.do("POST", "/v1/security/scans", agentToken, failed); code != 409 {
		t.Fatalf("older scan: %d %s", code, b)
	}

	code, b = h.do("GET", "/v1/security/vulnerabilities", viewerToken, nil)
	v := decode[apiv1.SecurityVulnerabilityReport](t, b)
	if code != 200 || v.Scanner.Status != apiv1.SecurityScannerStateStatusConnected || len(v.Scans) != 2 || len(v.Vulnerabilities) != 1 {
		t.Fatalf("report: %d %s", code, b)
	}
	if v.RunningImages[0].ScanState != apiv1.SecurityRunningImageScanStateScanned || v.RunningImages[1].ScanState != apiv1.SecurityRunningImageScanStateScanFailed {
		t.Fatalf("scan states: %+v", v.RunningImages)
	}
	// A failed scan is not zero vulnerabilities: its container counts as not scanned.
	if v.Summary.AffectedContainers != 1 || v.Summary.NotScannedContainers != 1 || v.Vulnerabilities[0].Severity != apiv1.SecurityVulnerabilitySeverityHigh {
		t.Fatalf("summary: %+v", v.Summary)
	}
}
