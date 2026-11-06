package security

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

var scanned = time.Date(2026, 11, 2, 9, 30, 0, 0, time.UTC)

func ptr[T any](v T) *T { return &v }

const (
	digestA = "sha256:aaaa"
	digestB = "sha256:bbbb"
)

// sampleVulns covers CVE/no CVE, KEV listed/not, EPSS scored/not, an
// exception, and fixed/not fixed/won't fix/unknown, over every severity.
func sampleVulns() *apiv1.SecurityVulnerabilityReport {
	ex := exception("sx_v1", apiv1.SecurityExceptionTargetVulnerability, "v1", time.Hour)
	pkg := func(name string) apiv1.SecurityPackage {
		return apiv1.SecurityPackage{Name: name, Version: "1.0.0", Type: "deb", Purl: ptr("pkg:deb/debian/" + name + "@1.0.0")}
	}
	return &apiv1.SecurityVulnerabilityReport{
		GeneratedAt: observed,
		RunningImages: []apiv1.SecurityRunningImage{
			{Environment: "eks", Digest: ptr(digestA)},
			{Environment: "gke", Digest: ptr(digestB)},
			{Environment: "eks", Digest: ptr(digestB)},
		},
		Scans: []apiv1.SecurityImageScan{
			{Id: "s1", Digest: digestA, FinishedAt: &scanned, Status: apiv1.SecurityImageScanStatusSucceeded},
			{Id: "s2", Digest: digestB, Status: apiv1.SecurityImageScanStatusSucceeded}, // no finish time
		},
		Vulnerabilities: []apiv1.SecurityVulnerability{
			{Id: "v1", Advisory: "CVE-2024-6387", Digest: digestA, ScanId: "s1", Package: pkg("openssh-server"), Severity: apiv1.SecurityVulnerabilitySeverityHigh,
				FixState: apiv1.SecurityVulnerabilityFixStateFixed, FixedVersions: []string{"1:9.2p1-2+deb12u3"}, Urls: []string{"https://nvd.nist.gov/vuln/detail/CVE-2024-6387"},
				Cvss: []apiv1.SecurityCVSS{{BaseScore: 8.1, Version: "3.1", Vector: "CVSS:3.1/AV:N/AC:H/PR:N/UI:N/S:U/C:H/I:H/A:H", Source: "nvd@nist.gov"}},
				Exploitation: &apiv1.SecurityExploitation{Cve: ptr("CVE-2024-6387"),
					Kev:  apiv1.SecurityKEV{State: apiv1.SecurityKEVStateListed, DateAdded: ptr("2026-11-01")},
					Epss: apiv1.SecurityEPSS{State: apiv1.SecurityEPSSStateScored, Probability: ptr(0.042), Percentile: ptr(0.92)}},
				Exception: &ex},
			{Id: "v2", Advisory: "GHSA-xxxx-yyyy-zzzz", Digest: digestA, ScanId: "s1", Package: pkg("lodash"), Severity: apiv1.SecurityVulnerabilitySeverityUnknown,
				FixState: apiv1.SecurityVulnerabilityFixStateNotFixed, FixedVersions: []string{}, Urls: []string{}, Cvss: []apiv1.SecurityCVSS{},
				Exploitation: &apiv1.SecurityExploitation{Kev: apiv1.SecurityKEV{State: apiv1.SecurityKEVStateNoCve}, Epss: apiv1.SecurityEPSS{State: apiv1.SecurityEPSSStateNoCve}}},
			{Id: "v3", Advisory: "DLA-1234-1", Aliases: []string{"cve-2023-1111"}, Digest: digestB, ScanId: "s2", Package: apiv1.SecurityPackage{Name: "zlib", Version: "1.2", Type: "deb"},
				Severity: apiv1.SecurityVulnerabilitySeverityNegligible, FixState: apiv1.SecurityVulnerabilityFixStateWontFix,
				Exploitation: &apiv1.SecurityExploitation{Cve: ptr("CVE-2023-1111"),
					Kev:  apiv1.SecurityKEV{State: apiv1.SecurityKEVStateNotListed},
					Epss: apiv1.SecurityEPSS{State: apiv1.SecurityEPSSStateUnknown}}},
			{Id: "v4", Advisory: "CVE-2025-0001", Digest: digestB, ScanId: "s-gone", Package: pkg("libc6"), Severity: apiv1.SecurityVulnerabilitySeverityCritical,
				FixState: apiv1.SecurityVulnerabilityFixStateUnknown,
				Exploitation: &apiv1.SecurityExploitation{Cve: ptr("CVE-2025-0001"),
					Kev:  apiv1.SecurityKEV{State: apiv1.SecurityKEVStateUnknown},
					Epss: apiv1.SecurityEPSS{State: apiv1.SecurityEPSSStateScored, Probability: ptr(0.5)}}}, // no percentile
			{Id: "v5", Advisory: "CVE-2025-0002", Digest: digestA, ScanId: "s1", Package: pkg("curl"), Severity: apiv1.SecurityVulnerabilitySeverityMedium,
				FixState: apiv1.SecurityVulnerabilityFixStateFixed}, // no exploitation looked up
			{Id: "v6", Advisory: "CVE-2025-0003", Digest: digestA, ScanId: "s1", Package: pkg("bash"), Severity: apiv1.SecurityVulnerabilitySeverityLow,
				FixState: apiv1.SecurityVulnerabilityFixStateNotFixed},
		},
	}
}

func sampleControls() *apiv1.SecurityControlReport {
	ctl := func(id, check, sev string) apiv1.SecurityControl {
		return apiv1.SecurityControl{Id: id, Check: check, Title: "Title of " + id, FailSeverity: sev, Domain: apiv1.SecurityControlDomain("identity"), Source: "aws-iam"}
	}
	eval := func(id, control, env, asset string, r apiv1.SecurityControlResult) apiv1.SecurityControlEvaluation {
		return apiv1.SecurityControlEvaluation{Id: id, ControlId: control, Environment: env, Asset: asset, Result: r, Reason: "evidence " + id,
			Source: "aws-iam", EvaluatedAt: scanned, FindingIds: []string{}}
	}
	return &apiv1.SecurityControlReport{
		GeneratedAt: observed,
		Controls:    []apiv1.SecurityControl{ctl("HC-IDN-03", "aws.user-mfa", "medium"), ctl("HC-POL-01", "k8s.privileged", "varies")},
		Evaluations: []apiv1.SecurityControlEvaluation{
			eval("e1", "HC-IDN-03", "aws-account", "arn:aws:iam::1:user/alice", apiv1.SecurityControlResultPass),
			eval("e2", "HC-IDN-03", "aws-account", "arn:aws:iam::1:user/bob", apiv1.SecurityControlResultFail),
			eval("e3", "HC-POL-01", "gke", "*", apiv1.SecurityControlResultUnknown),
			eval("e4", "HC-IDN-03", "aws-account", "arn:aws:iam::1:user/svc", apiv1.SecurityControlResultNotApplicable),
			eval("e5", "HC-POL-01", "gke", "shop/Deployment/web", apiv1.SecurityControlResultFail),
		},
	}
}

func sampleExport() OCSFExport {
	return ToOCSFAll(apiv1.SecurityReport{GeneratedAt: observed, Findings: sample, Sources: []apiv1.SecuritySource{
		{Environment: "eks", Provider: apiv1.SecurityProviderAws}, {Environment: "aws-account", Provider: apiv1.SecurityProviderAws},
		{Environment: "gke", Provider: apiv1.SecurityProviderGcp},
	}}, sampleVulns(), sampleControls())
}

func TestOCSFAllEventsMatchTheOfficialSchema(t *testing.T) {
	schemas := ocsfSchemas(t)
	exp := sampleExport()
	if len(exp.Findings) != len(sample) || len(exp.Vulnerabilities) != 6 || len(exp.Controls) != 4 {
		t.Fatalf("findings %d vulnerabilities %d controls %d", len(exp.Findings), len(exp.Vulnerabilities), len(exp.Controls))
	}
	for _, e := range exp.Findings {
		if err := validate(t, schemas, e); err != nil {
			t.Errorf("finding %s: %v", e.FindingInfo.UID, err)
		}
	}
	for _, e := range exp.Vulnerabilities {
		if err := validateClass(t, schemas, ocsfVulnerability, e); err != nil {
			t.Errorf("vulnerability %s: %v", e.FindingInfo.UID, err)
		}
	}
	for _, e := range exp.Controls {
		if e.ClassUID != ocsfCompliance {
			t.Errorf("control %s: class %d", e.FindingInfo.UID, e.ClassUID)
		}
		if err := validate(t, schemas, e); err != nil {
			t.Errorf("control %s: %v", e.FindingInfo.UID, err)
		}
	}
}

func TestOCSFVulnerabilityMapping(t *testing.T) {
	byID := map[string]OCSFVulnerabilityFinding{}
	for _, e := range sampleExport().Vulnerabilities {
		byID[e.FindingInfo.UID] = e
	}
	for id, want := range map[string]struct {
		severity int
		sevName  string
		status   int
		cve      string
		time     time.Time
		provider string
	}{
		"v1": {4, "High", 3, "CVE-2024-6387", scanned, "AWS"},
		"v2": {0, "Unknown", 1, "GHSA-xxxx-yyyy-zzzz", scanned, "AWS"},
		"v3": {1, "Informational", 1, "CVE-2023-1111", observed, "platform"}, // no finish time; digestB runs on AWS and GCP
		"v4": {5, "Critical", 1, "CVE-2025-0001", observed, "platform"},      // its scan is not in the report
		"v5": {3, "Medium", 1, "CVE-2025-0002", scanned, "AWS"},
		"v6": {2, "Low", 1, "CVE-2025-0003", scanned, "AWS"},
	} {
		e, ok := byID[id]
		if !ok {
			t.Fatalf("%s missing", id)
		}
		if e.ClassUID != 2002 || e.TypeUID != 200201 || e.CategoryUID != 2 || e.ActivityID != 1 || e.ClassName != "Vulnerability Finding" {
			t.Errorf("%s class: %+v", id, e)
		}
		if e.SeverityID != want.severity || e.Severity != want.sevName || e.StatusID != want.status || e.Time != want.time.UnixMilli() || e.Cloud.Provider != want.provider {
			t.Errorf("%s: severity %d %q status %d time %d provider %q", id, e.SeverityID, e.Severity, e.StatusID, e.Time, e.Cloud.Provider)
		}
		if len(e.Vulnerabilities) != 1 || e.Vulnerabilities[0].CVE.UID != want.cve {
			t.Errorf("%s vulnerabilities: %+v", id, e.Vulnerabilities)
		}
		if e.Metadata.Version != "1.9.0" || e.Metadata.Product.Name != "HopCharlie" || len(e.Metadata.Profiles) != 1 || e.Metadata.Profiles[0] != "cloud" {
			t.Errorf("%s metadata: %+v", id, e.Metadata)
		}
		if len(e.Resources) != 1 || e.Resources[0].UID == "" {
			t.Errorf("%s resources: %+v", id, e.Resources)
		}
	}

	// Exception: suppressed with the reason; severity stays.
	if v1 := byID["v1"]; v1.Status != "Suppressed" || v1.StatusDetail != "accepted sx_v1" {
		t.Errorf("v1 status: %q %q", v1.Status, v1.StatusDetail)
	}
	if v5 := byID["v5"]; v5.Status != "New" || v5.StatusDetail != "" {
		t.Errorf("v5 status: %q %q", v5.Status, v5.StatusDetail)
	}

	// KEV: only listed writes is_exploit_available and the catalog URL.
	for id, e := range byID {
		x := e.Vulnerabilities[0]
		if id == "v1" {
			if x.IsExploitAvailable == nil || !*x.IsExploitAvailable || x.ExploitRefURL != "https://www.cisa.gov/known-exploited-vulnerabilities-catalog" {
				t.Errorf("v1 KEV: %+v", x)
			}
		} else if x.IsExploitAvailable != nil || x.ExploitRefURL != "" {
			t.Errorf("%s: KEV fields without a listing: %+v", id, x)
		}
	}

	// EPSS: only when scored, score as a string.
	if ep := byID["v1"].Vulnerabilities[0].CVE.EPSS; ep == nil || ep.Score != "0.042" || ep.Percentile == nil || *ep.Percentile != 0.92 {
		t.Errorf("v1 EPSS: %+v", ep)
	}
	if ep := byID["v4"].Vulnerabilities[0].CVE.EPSS; ep == nil || ep.Score != "0.5" || ep.Percentile != nil {
		t.Errorf("v4 EPSS: %+v", ep)
	}
	for _, id := range []string{"v2", "v3", "v5", "v6"} {
		if ep := byID[id].Vulnerabilities[0].CVE.EPSS; ep != nil {
			t.Errorf("%s: EPSS without a score: %+v", id, ep)
		}
	}

	// CVSS, package, fix.
	x1 := byID["v1"].Vulnerabilities[0]
	if len(x1.CVE.CVSS) != 1 || x1.CVE.CVSS[0].BaseScore != 8.1 || x1.CVE.CVSS[0].Version != "3.1" || !strings.HasPrefix(x1.CVE.CVSS[0].VectorString, "CVSS:3.1/") {
		t.Errorf("v1 cvss: %+v", x1.CVE.CVSS)
	}
	if len(x1.AffectedPackages) != 1 || x1.AffectedPackages[0].Name != "openssh-server" || x1.AffectedPackages[0].FixedInVersion != "1:9.2p1-2+deb12u3" ||
		x1.AffectedPackages[0].Purl == "" || len(x1.References) != 1 || x1.Title != "CVE-2024-6387" {
		t.Errorf("v1 vulnerability: %+v", x1)
	}
	for id, want := range map[string]*bool{"v1": ptr(true), "v2": ptr(false), "v3": ptr(false), "v4": nil, "v5": ptr(true)} {
		got := byID[id].Vulnerabilities[0].IsFixAvailable
		if (got == nil) != (want == nil) || (got != nil && *got != *want) {
			t.Errorf("%s is_fix_available: %v, want %v", id, got, want)
		}
	}
	if e := byID["v1"]; !strings.Contains(e.FindingInfo.Title, "CVE-2024-6387") || !strings.Contains(e.FindingInfo.Title, "openssh-server") {
		t.Errorf("v1 title: %q", e.FindingInfo.Title)
	}
}

func TestOCSFControlMapping(t *testing.T) {
	byID := map[string]OCSFFinding{}
	for _, e := range sampleExport().Controls {
		byID[e.FindingInfo.UID] = e
	}
	if _, ok := byID["e4"]; ok {
		t.Fatal("not_applicable evaluation exported")
	}
	for id, want := range map[string]struct {
		status   int
		name     string
		severity int
		provider string
		control  string
	}{
		"e1": {1, "Pass", 1, "AWS", "HC-IDN-03"},
		"e2": {3, "Fail", 3, "AWS", "HC-IDN-03"},
		"e3": {0, "Unknown", 1, "GCP", "HC-POL-01"},
		"e5": {3, "Fail", 0, "GCP", "HC-POL-01"}, // failSeverity varies
	} {
		e := byID[id]
		if e.Compliance == nil || e.Compliance.StatusID != want.status || e.Compliance.Status != want.name || e.Compliance.Control != want.control {
			t.Errorf("%s compliance: %+v", id, e.Compliance)
		}
		if e.ClassUID != 2003 || e.TypeUID != 200301 || e.SeverityID != want.severity || e.Cloud.Provider != want.provider || e.Time != scanned.UnixMilli() {
			t.Errorf("%s: %+v", id, e)
		}
		if e.StatusID != 1 || len(e.Resources) != 1 || e.FindingInfo.Analytic == nil || e.FindingInfo.Analytic.Name == "" ||
			!strings.HasPrefix(e.FindingInfo.Title, want.control+" ") {
			t.Errorf("%s: %+v", id, e)
		}
	}
	if e := byID["e3"]; e.Resources[0].UID != "*" {
		t.Errorf("e3 asset: %+v", e.Resources)
	}
}

func TestOCSFExportArraysAlwaysPresent(t *testing.T) {
	for name, exp := range map[string]OCSFExport{
		"ToOCSF":         ToOCSF(apiv1.SecurityReport{}),
		"ToOCSFAll nils": ToOCSFAll(apiv1.SecurityReport{}, nil, nil),
	} {
		raw, _ := json.Marshal(exp)
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		for _, k := range []string{"findings", "vulnerabilities", "controls"} {
			if a, ok := m[k].([]any); !ok || len(a) != 0 {
				t.Errorf("%s: %s = %v", name, k, m[k])
			}
		}
	}
}

// The 2002 validator must reject what its schema forbids.
func TestOCSFVulnerabilityValidatorRejectsInvalidEvents(t *testing.T) {
	schemas := ocsfSchemas(t)
	good := sampleExport().Vulnerabilities[0]
	for name, change := range map[string]func(m map[string]any){
		"no vulnerabilities":   func(m map[string]any) { delete(m, "vulnerabilities") },
		"wrong class":          func(m map[string]any) { m["class_uid"] = 2003 },
		"epss score as number": func(m map[string]any) { vuln(m)["cve"].(map[string]any)["epss"].(map[string]any)["score"] = 0.042 },
		"package without version": func(m map[string]any) {
			delete(vuln(m)["affected_packages"].([]any)[0].(map[string]any), "version")
		},
		"kev field": func(m map[string]any) { vuln(m)["kev"] = true },
	} {
		t.Run(name, func(t *testing.T) {
			raw, _ := json.Marshal(good)
			var m map[string]any
			_ = json.Unmarshal(raw, &m)
			change(m)
			b, _ := json.Marshal(m)
			inst, _ := jsonschema.UnmarshalJSON(strings.NewReader(string(b)))
			if err := schemas[ocsfVulnerability].Validate(inst); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

func vuln(m map[string]any) map[string]any {
	return m["vulnerabilities"].([]any)[0].(map[string]any)
}
