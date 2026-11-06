package security

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

// Every check a collector reports or evaluates is in the catalog, so no
// finding is left without a control and a severity basis.
func TestEveryCheckHasAControl(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`(?:Check:\s*|\.eval\(|\.unknown\(|unread\([^,]+,[^,]+,\s*)"((?:aws|k8s|platform|storage|image)\.[a-z0-9.-]+)"`)
	seen := map[string]bool{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range re.FindAllStringSubmatch(string(b), -1) {
			seen[m[1]] = true
			if _, ok := ControlFor(m[1]); !ok {
				t.Errorf("%s: check %q has no control", f, m[1])
			}
		}
	}
	if len(seen) < 20 {
		t.Fatalf("found only %d checks in the collectors; the pattern no longer matches them", len(seen))
	}
}

// The catalog keeps criteria and severity apart: each reference names its
// framework version, URL and relation; an exactly mapped vendor control gives
// the severity; everything else is a versioned HopCharlie rule.
func TestCatalogIsConsistent(t *testing.T) {
	ids, checks := map[string]bool{}, map[string]bool{}
	for _, c := range Catalog() {
		if ids[c.Id] || checks[c.Check] {
			t.Errorf("%s %s: duplicate id or check", c.Id, c.Check)
		}
		ids[c.Id], checks[c.Check] = true, true
		if c.SeverityBasis.Policy != PolicyVersion {
			t.Errorf("%s: basis policy %q", c.Id, c.SeverityBasis.Policy)
		}
		exact := false
		for _, r := range c.References {
			if !strings.HasPrefix(r.Url, "https://") || r.Version == "" || !r.Relation.Valid() {
				t.Errorf("%s: reference %s needs an https URL, a version and a relation: %+v", c.Id, r.Id, r)
			}
			if r.Relation == apiv1.SecurityControlRelationExact {
				exact = true
			}
		}
		switch {
		case len(c.References) == 0 && c.Mapping != apiv1.SecurityControlMappingUnmapped,
			exact && c.Mapping != apiv1.SecurityControlMappingExact,
			!exact && len(c.References) > 0 && c.Mapping != apiv1.SecurityControlMappingPartial:
			t.Errorf("%s: mapping %s does not follow its references", c.Id, c.Mapping)
		}
		switch c.SeverityBasis.Type {
		case apiv1.SecuritySeverityBasisTypeVendor:
			if c.SeverityBasis.SourceSeverity == nil || !strings.EqualFold(*c.SeverityBasis.SourceSeverity, c.FailSeverity) {
				t.Errorf("%s: a vendor basis must carry the vendor severity %s", c.Id, c.FailSeverity)
			}
			if !exact {
				t.Errorf("%s: a vendor severity needs an exact reference", c.Id)
			}
		case apiv1.SecuritySeverityBasisTypePlatformPolicy:
			if c.SeverityBasis.Issuer != IssuerHopCharlie {
				t.Errorf("%s: a platform rule is issued by HopCharlie, not %s", c.Id, c.SeverityBasis.Issuer)
			}
		case apiv1.SecuritySeverityBasisTypeCvss:
			t.Errorf("%s: CVSS scores vulnerabilities, not controls", c.Id)
		}
	}
	// The severities read from the Security Hub CSPM reference on 2026-11-06.
	for check, want := range map[string]string{"aws.user-mfa": "medium", "aws.root-access-key": "critical", "aws.ebs-default-encryption": "medium", "aws.s3-public-access-block": "medium"} {
		if c, ok := ControlFor(check); !ok || c.FailSeverity != want {
			t.Errorf("%s: fail severity %q, want %q", check, c.FailSeverity, want)
		}
	}
}

func awsIAMSource() Source {
	return Source{Name: "aws-iam", Provider: apiv1.SecurityProviderAws, Environment: EnvAWSAccount}
}

func coverageOf(t *testing.T, r apiv1.SecurityControlReport, env string, d apiv1.SecurityControlDomain) apiv1.SecurityControlCoverage {
	t.Helper()
	for _, c := range r.Coverage {
		if c.Environment == env && c.Domain == d {
			return c
		}
	}
	t.Fatalf("no coverage for %s %s in %+v", env, d, r.Coverage)
	return apiv1.SecurityControlCoverage{}
}

// pass, fail, unknown and not applicable stand side by side; not applicable
// is outside the denominator and unknown is never counted as evaluated.
func TestCoverageCountsEachResult(t *testing.T) {
	col := assemble(t0, []Report{{Source: awsIAMSource(), Status: apiv1.SecuritySourceStatusOk, CheckedAt: t0,
		Findings: []apiv1.SecurityFinding{{Check: "aws.user-mfa", Category: apiv1.SecurityCategoryIdentity, Severity: apiv1.SecuritySeverityMedium,
			Title: "t", Resource: "user/bob", Detail: "d", Recommendation: "r"}},
		Evaluations: []Evaluation{
			{Check: "aws.user-mfa", Asset: "user/alice", Result: apiv1.SecurityControlResultPass, Reason: "has MFA"},
			{Check: "aws.user-mfa", Asset: "user/bob", Result: apiv1.SecurityControlResultFail, Reason: "no MFA"},
			{Check: "aws.user-mfa", Asset: "user/carol", Result: apiv1.SecurityControlResultNotApplicable, Reason: "no console password"},
			{Check: "aws.access-key-age", Asset: AllAssets, Result: apiv1.SecurityControlResultUnknown, Reason: "AccessDenied"},
		}}})
	r := ControlReport(col, "", nil)
	if r.PolicyVersion != PolicyVersion || len(r.Controls) != len(catalog) {
		t.Fatalf("policy %q, %d controls", r.PolicyVersion, len(r.Controls))
	}
	cov := coverageOf(t, r, EnvAWSAccount, apiv1.SecurityControlDomainIdentity)
	want := apiv1.SecurityControlCounts{Applicable: 3, Evaluated: 2, Pass: 1, Fail: 1, Unknown: 1, NotApplicable: 1}
	if cov.State != apiv1.SecurityControlCoverageStateEvaluated || cov.Counts != want {
		t.Fatalf("identity: %+v", cov)
	}
	for _, e := range r.Evaluations {
		if e.Asset == "user/bob" && (len(e.FindingIds) != 1 || e.FindingIds[0] != col.Report.Findings[0].Id) {
			t.Fatalf("bob's failure links %v, want the finding %s", e.FindingIds, col.Report.Findings[0].Id)
		}
	}
	// No data source is connected for the account: not evaluated, not passed.
	if d := coverageOf(t, r, EnvAWSAccount, apiv1.SecurityControlDomainData); d.State != apiv1.SecurityControlCoverageStateUnsupported || d.Counts.Pass != 0 || d.Reason == nil {
		t.Fatalf("data: %+v", d)
	}
}

// A source whose read failed reports every control of its own as unknown,
// never as 0 findings that look like a pass.
func TestUnreadableSourceIsUnknown(t *testing.T) {
	col := assemble(t0, []Report{{Source: awsIAMSource(), Status: apiv1.SecuritySourceStatusError, Message: "AccessDenied: iam:GetAccountSummary", CheckedAt: t0}})
	r := ControlReport(col, EnvAWSAccount, nil)
	n := 0
	for _, e := range r.Evaluations {
		if e.Source != "aws-iam" {
			continue
		}
		n++
		if e.Result != apiv1.SecurityControlResultUnknown || e.Asset != AllAssets || !strings.Contains(e.Reason, "AccessDenied") {
			t.Errorf("%s: %+v", e.ControlId, e)
		}
	}
	if n != 4 {
		t.Fatalf("%d unknown evaluations, want one per aws-iam control (4): %+v", n, r.Evaluations)
	}
	if cov := coverageOf(t, r, EnvAWSAccount, apiv1.SecurityControlDomainIdentity); cov.State != apiv1.SecurityControlCoverageStateNotConnected || cov.Counts.Evaluated != 0 {
		t.Fatalf("identity: %+v", cov)
	}
	if len(col.Report.Findings) != 0 {
		t.Fatal("an unread source reported findings")
	}
}

// Findings keep the source's severity and score next to the normalized one.
func TestFindingsCarryTheirBasis(t *testing.T) {
	col := assemble(t0, []Report{{Source: awsIAMSource(), Status: apiv1.SecuritySourceStatusOk, CheckedAt: t0,
		Findings: []apiv1.SecurityFinding{
			{Check: "aws.user-mfa", Category: apiv1.SecurityCategoryIdentity, Severity: apiv1.SecuritySeverityMedium, Title: "t", Resource: "user/bob", Detail: "d", Recommendation: "r"},
			{Check: "aws.root-mfa", Category: apiv1.SecurityCategoryIdentity, Severity: apiv1.SecuritySeverityCritical, Title: "t", Resource: "root", Detail: "d", Recommendation: "r"},
		}}})
	for _, f := range col.Report.Findings {
		if f.Kind == nil || *f.Kind != apiv1.SecurityFindingKindConfiguration || f.ControlId == nil || f.SeverityBasis == nil {
			t.Fatalf("%s: %+v", f.Check, f)
		}
		b := f.SeverityBasis
		switch f.Check {
		case "aws.user-mfa":
			if b.Type != apiv1.SecuritySeverityBasisTypeVendor || b.SourceSeverity == nil || *b.SourceSeverity != "Medium" || b.SourceUrl == nil {
				t.Errorf("IAM.5 basis: %+v", b)
			}
		case "aws.root-mfa":
			if b.Type != apiv1.SecuritySeverityBasisTypePlatformPolicy || b.Policy != PolicyVersion {
				t.Errorf("root MFA basis: %+v", b)
			}
		}
		if b.SourceScore != nil && b.Type != apiv1.SecuritySeverityBasisTypeVendor {
			t.Errorf("%s: a score on a non-vendor basis: %+v", f.Check, b)
		}
	}
}

func TestImageDigest(t *testing.T) {
	d := "sha256:" + strings.Repeat("ab", 32)
	for _, c := range []struct{ status, spec, want string }{
		{"ghcr.io/x/app@" + d, "ghcr.io/x/app:v1", d},
		{"docker-pullable://ghcr.io/x/app@" + d, "ghcr.io/x/app:v1", d},
		{d, "ghcr.io/x/app:v1", ""}, // a local image id is not a registry digest
		{"", "ghcr.io/x/app@" + d, d},
		{"", "ghcr.io/x/app:v1", ""},
		{"ghcr.io/x/app@sha256:short", "", ""},
	} {
		if got := ImageDigest(c.status, c.spec); got != c.want {
			t.Errorf("ImageDigest(%q, %q) = %q, want %q", c.status, c.spec, got, c.want)
		}
	}
}

type fakeScans struct {
	scans []apiv1.SecurityImageScan
	vulns []apiv1.SecurityVulnerability
	err   error
}

func (f fakeScans) Latest() ([]apiv1.SecurityImageScan, []apiv1.SecurityVulnerability, error) {
	return f.scans, f.vulns, f.err
}

func kubeReport(images ...apiv1.SecurityRunningImage) Report {
	return Report{Source: Source{Name: "kubernetes", Provider: apiv1.SecurityProviderGcp, Environment: "gke"}, Status: apiv1.SecuritySourceStatusOk, CheckedAt: t0, Images: images}
}

func running(workload, digest string) apiv1.SecurityRunningImage {
	img := apiv1.SecurityRunningImage{Environment: "gke", Namespace: "shop", Workload: workload, Container: "app", Image: "ghcr.io/x/app:v1"}
	if digest != "" {
		img.Digest = &digest
	}
	return img
}

func vulnResults(t *testing.T, r apiv1.SecurityControlReport) map[string]apiv1.SecurityControlResult {
	t.Helper()
	c, _ := ControlFor(CheckImageScanned)
	out := map[string]apiv1.SecurityControlResult{}
	for _, e := range r.Evaluations {
		if e.ControlId == c.Id {
			out[e.Asset] = e.Result
		}
	}
	return out
}

// Without a scanner, running images are not scanned: HC-VUL-01 fails and the
// vulnerability report says so instead of reporting zero vulnerabilities.
func TestNoScannerIsNotZeroVulnerabilities(t *testing.T) {
	d1 := "sha256:" + strings.Repeat("1", 64)
	col := assemble(t0, []Report{kubeReport(running("Deployment/web", d1), running("Deployment/old", ""))})
	got := vulnResults(t, ControlReport(col, "", nil))
	if got["shop/Deployment/web/app@"+d1] != apiv1.SecurityControlResultFail || got["shop/Deployment/old/app"] != apiv1.SecurityControlResultUnknown {
		t.Fatalf("HC-VUL-01: %v", got)
	}
	v := VulnerabilityReport(col, "", nil)
	if v.Scanner.Status != apiv1.SecurityScannerStateStatusNotConfigured || v.Scanner.Message == nil {
		t.Fatalf("scanner: %+v", v.Scanner)
	}
	if v.Summary.NotScannedContainers != 2 || v.Summary.Advisories != 0 || len(v.RunningImages) != 2 {
		t.Fatalf("summary: %+v", v.Summary)
	}
}

// Advisories, images, affected containers and occurrences are separate
// counts; a failed scan fails the control and a scan store read error makes
// it unknown.
func TestScannedImagesLinkToWorkloads(t *testing.T) {
	d1, d2, d3 := "sha256:"+strings.Repeat("1", 64), "sha256:"+strings.Repeat("2", 64), "sha256:"+strings.Repeat("3", 64)
	col := assemble(t0, []Report{kubeReport(running("Deployment/web", d1), running("Deployment/api", d1), running("Deployment/job", d2), running("Deployment/new", d3))})
	pkg := apiv1.SecurityPackage{Name: "openssl", Version: "3.0.13", Type: "deb"}
	scans := fakeScans{
		scans: []apiv1.SecurityImageScan{{Id: "scan1", Digest: d1, Status: apiv1.SecurityImageScanStatusSucceeded}, {Id: "scan2", Digest: d2, Status: apiv1.SecurityImageScanStatusFailed}},
		vulns: []apiv1.SecurityVulnerability{
			{Id: "v1", ScanId: "scan1", Digest: d1, Advisory: "CVE-2026-0001", Package: pkg, Severity: apiv1.SecurityVulnerabilitySeverityHigh, FixState: apiv1.SecurityVulnerabilityFixStateFixed, FixedVersions: []string{"3.0.14"}},
			{Id: "v2", ScanId: "scan1", Digest: d1, Advisory: "CVE-2026-0002", Package: pkg, Severity: apiv1.SecurityVulnerabilitySeverityUnknown, FixState: apiv1.SecurityVulnerabilityFixStateUnknown},
		},
	}
	got := vulnResults(t, ControlReport(col, "", scans))
	want := map[string]apiv1.SecurityControlResult{
		"shop/Deployment/web/app@" + d1: apiv1.SecurityControlResultPass,
		"shop/Deployment/api/app@" + d1: apiv1.SecurityControlResultPass,
		"shop/Deployment/job/app@" + d2: apiv1.SecurityControlResultFail, // the scan failed
		"shop/Deployment/new/app@" + d3: apiv1.SecurityControlResultFail, // never scanned
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s: %s, want %s", k, got[k], w)
		}
	}
	v := VulnerabilityReport(col, "", scans)
	if s := v.Summary; s.Advisories != 2 || s.Images != 1 || s.AffectedContainers != 2 || s.Occurrences != 2 || s.NotScannedContainers != 2 {
		t.Fatalf("summary: %+v", s)
	}
	broken := fakeScans{err: errors.New("db down")}
	for k, r := range vulnResults(t, ControlReport(col, "", broken)) {
		if r != apiv1.SecurityControlResultUnknown {
			t.Errorf("%s with unreadable scans: %s", k, r)
		}
	}
	if v := VulnerabilityReport(col, "", broken); v.Scanner.Status != apiv1.SecurityScannerStateStatusError || len(v.Vulnerabilities) != 0 {
		t.Fatalf("scanner error: %+v", v.Scanner)
	}
}

// A cluster that could not be read has an unknown HC-VUL-01, not no images.
func TestUnreadableClusterImagesAreUnknown(t *testing.T) {
	rep := kubeReport()
	rep.Status, rep.Message = apiv1.SecuritySourceStatusError, "forbidden"
	got := vulnResults(t, ControlReport(assemble(t0, []Report{rep}), "", nil))
	if len(got) != 1 || got[AllAssets] != apiv1.SecurityControlResultUnknown {
		t.Fatalf("HC-VUL-01: %v", got)
	}
}
