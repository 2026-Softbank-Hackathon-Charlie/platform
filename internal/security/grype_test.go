package security

import (
	"strings"
	"testing"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

// grypeReport is shaped like `grype -o json` of v0.121 (schema v6 database):
// one advisory found by two matchers, one without a severity, one GHSA.
const grypeReport = `{
 "matches": [
  {"vulnerability": {"id": "CVE-2024-6387", "dataSource": "https://security-tracker.debian.org/tracker/CVE-2024-6387", "namespace": "debian:distro:debian:12",
     "severity": "High", "urls": ["https://security-tracker.debian.org/tracker/CVE-2024-6387"], "cvss": [],
     "fix": {"versions": ["1:9.2p1-2+deb12u3"], "state": "fixed"}, "advisories": [], "risk": 1.2},
   "relatedVulnerabilities": [{"id": "CVE-2024-6387", "dataSource": "https://nvd.nist.gov/vuln/detail/CVE-2024-6387", "namespace": "nvd:cpe", "severity": "High",
     "urls": ["https://nvd.nist.gov/vuln/detail/CVE-2024-6387"],
     "cvss": [{"source": "nvd@nist.gov", "type": "Primary", "version": "3.1", "vector": "CVSS:3.1/AV:N/AC:H/PR:N/UI:N/S:U/C:H/I:H/A:H", "metrics": {"baseScore": 8.1}}]}],
   "matchDetails": [], "artifact": {"id": "a1", "name": "openssh-client", "version": "1:9.2p1-2+deb12u2", "type": "deb", "purl": "pkg:deb/debian/openssh-client@1:9.2p1-2+deb12u2?distro=debian-12"}},
  {"vulnerability": {"id": "CVE-2024-6387", "dataSource": "https://security-tracker.debian.org/tracker/CVE-2024-6387", "namespace": "debian:distro:debian:12",
     "severity": "High", "urls": ["https://example.org/extra"], "cvss": [], "fix": {"versions": ["1:9.2p1-2+deb12u3"], "state": "fixed"}},
   "relatedVulnerabilities": [], "matchDetails": [], "artifact": {"name": "openssh-client", "version": "1:9.2p1-2+deb12u2", "type": "deb", "purl": "pkg:deb/debian/openssh-client@1:9.2p1-2+deb12u2?distro=debian-12"}},
  {"vulnerability": {"id": "CVE-2023-0001", "dataSource": "", "namespace": "debian:distro:debian:12", "severity": "", "urls": [], "cvss": [],
     "fix": {"versions": [], "state": "not-fixed"}},
   "relatedVulnerabilities": [], "matchDetails": [], "artifact": {"name": "libc6", "version": "2.36-9", "type": "deb", "purl": ""}},
  {"vulnerability": {"id": "GHSA-xxxx-yyyy-zzzz", "dataSource": "https://github.com/advisories/GHSA-xxxx-yyyy-zzzz", "namespace": "github:language:go",
     "severity": "Medium", "urls": [], "cvss": [{"version": "3.1", "vector": "CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:N/I:N/A:L", "metrics": {"baseScore": 5.3}}],
     "fix": {"versions": ["0.23.0"], "state": "fixed"}},
   "relatedVulnerabilities": [{"id": "CVE-2025-1111", "namespace": "nvd:cpe", "urls": []}],
   "matchDetails": [], "artifact": {"name": "golang.org/x/net", "version": "v0.20.0", "type": "go-module", "purl": "pkg:golang/golang.org/x/net@v0.20.0"}}
 ],
 "source": {"type": "image", "target": {"userInput": "r/app@sha256:aaa", "manifestDigest": "sha256:bbb", "architecture": "amd64", "os": "linux"}},
 "distro": {"name": "debian", "version": "12", "idLike": []},
 "descriptor": {"name": "grype", "version": "0.121.0", "db": {"status": {"schemaVersion": "v6.1.2", "from": "https://grype.anchore.io/databases/v6/x.tar.zst", "built": "2026-11-05T04:12:30Z", "valid": true}}}
}`

func TestParseGrype(t *testing.T) {
	scan := apiv1.SecurityImageScan{Id: "scan_1", Digest: "sha256:aaa", Status: apiv1.SecurityImageScanStatusSucceeded}
	got, vulns, err := ParseGrype([]byte(grypeReport), scan)
	if err != nil {
		t.Fatal(err)
	}
	if got.Scanner.Name != "grype" || got.Scanner.Version != "0.121.0" || got.ResultSha256 == nil || len(*got.ResultSha256) != 64 {
		t.Fatalf("scanner: %+v", got)
	}
	if got.Database == nil || *got.Database.SchemaVersion != "v6.1.2" || !got.Database.BuiltAt.Equal(time.Date(2026, 11, 5, 4, 12, 30, 0, time.UTC)) {
		t.Fatalf("database: %+v", got.Database)
	}
	if *got.Platform != "linux/amd64" || !strings.Contains(*got.Message, "sha256:bbb") {
		t.Fatalf("platform %v, message %v", got.Platform, got.Message)
	}
	if len(vulns) != 3 {
		t.Fatalf("got %d vulnerabilities, want the two openssh matches merged: %+v", len(vulns), vulns)
	}
	ssh := vulns[0]
	if ssh.Advisory != "CVE-2024-6387" || ssh.Severity != apiv1.SecurityVulnerabilitySeverityHigh || ssh.FixState != apiv1.SecurityVulnerabilityFixStateFixed ||
		ssh.ScanId != "scan_1" || ssh.Digest != "sha256:aaa" || *ssh.Package.Distro != "debian-12" || len(ssh.Aliases) != 0 {
		t.Fatalf("openssh: %+v", ssh)
	}
	if ssh.SeverityBasis.Type != apiv1.SecuritySeverityBasisTypeVendor || ssh.SeverityBasis.Issuer != "debian:distro:debian:12" || *ssh.SeverityBasis.SourceSeverity != "High" || ssh.SeverityBasis.Policy != PolicyVersion {
		t.Fatalf("basis: %+v", ssh.SeverityBasis)
	}
	if len(ssh.Cvss) != 1 || ssh.Cvss[0].Source != "nvd@nist.gov" || ssh.Cvss[0].BaseScore != 8.1 || len(ssh.Urls) != 3 {
		t.Fatalf("cvss %+v urls %v", ssh.Cvss, ssh.Urls)
	}
	// No severity from the source: unknown, never low, even with no CVSS.
	libc := vulns[1]
	if libc.Severity != apiv1.SecurityVulnerabilitySeverityUnknown || libc.SeverityBasis.SourceSeverity != nil || libc.FixState != apiv1.SecurityVulnerabilityFixStateNotFixed ||
		libc.FixedVersions == nil || libc.Cvss == nil || libc.Urls == nil || libc.Package.Purl != nil {
		t.Fatalf("libc: %+v", libc)
	}
	ghsa := vulns[2]
	if ghsa.Advisory != "GHSA-xxxx-yyyy-zzzz" || len(ghsa.Aliases) != 1 || ghsa.Aliases[0] != "CVE-2025-1111" || ghsa.Cvss[0].Source != "github:language:go" {
		t.Fatalf("ghsa: %+v", ghsa)
	}
	// Stable ids: the same report gives the same ids.
	_, again, _ := ParseGrype([]byte(grypeReport), scan)
	if again[0].Id != ssh.Id || ssh.Id == libc.Id {
		t.Fatal("vulnerability ids are not stable and distinct")
	}
}

func TestParseGrypeRefusesOtherReports(t *testing.T) {
	scan := apiv1.SecurityImageScan{Id: "scan_1", Digest: "sha256:aaa"}
	for _, report := range []string{`not json`, `{"descriptor": {"name": "trivy", "version": "1"}}`, `{"descriptor": {"name": "grype"}}`} {
		if _, _, err := ParseGrype([]byte(report), scan); err == nil {
			t.Errorf("accepted %s", report)
		}
	}
	// An image with nothing matched is an empty list, not nil.
	_, vulns, err := ParseGrype([]byte(`{"matches": [], "descriptor": {"name": "grype", "version": "0.121.0"}}`), scan)
	if err != nil || vulns == nil || len(vulns) != 0 {
		t.Fatalf("%v %v", vulns, err)
	}
}
