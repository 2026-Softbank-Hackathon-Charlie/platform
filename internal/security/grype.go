package security

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

// ScannerName is the one image scanner the platform reads (SEC-04).
const ScannerName = "grype"

// grypeDocument is the part of Grype's JSON report (`grype -o json`,
// grype/presenter/models) the platform reads.
type grypeDocument struct {
	Matches []struct {
		Vulnerability struct {
			grypeMetadata
			Fix struct {
				Versions []string `json:"versions"`
				State    string   `json:"state"`
			} `json:"fix"`
		} `json:"vulnerability"`
		RelatedVulnerabilities []grypeMetadata `json:"relatedVulnerabilities"`
		Artifact               struct {
			Name    string `json:"name"`
			Version string `json:"version"`
			Type    string `json:"type"`
			PURL    string `json:"purl"`
		} `json:"artifact"`
	} `json:"matches"`
	Source *struct {
		Target struct {
			ManifestDigest string `json:"manifestDigest"`
			Architecture   string `json:"architecture"`
			OS             string `json:"os"`
		} `json:"target"`
	} `json:"source"`
	Distro struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"distro"`
	Descriptor struct {
		Name    string          `json:"name"`
		Version string          `json:"version"`
		DB      json.RawMessage `json:"db"`
	} `json:"descriptor"`
}

type grypeMetadata struct {
	ID         string   `json:"id"`
	DataSource string   `json:"dataSource"`
	Namespace  string   `json:"namespace"`
	Severity   string   `json:"severity"`
	URLs       []string `json:"urls"`
	CVSS       []struct {
		Source  string `json:"source"`
		Version string `json:"version"`
		Vector  string `json:"vector"`
		Metrics struct {
			BaseScore float64 `json:"baseScore"`
		} `json:"metrics"`
	} `json:"cvss"`
}

// grypeDB is descriptor.db: {"status": {...}} since schema v6, the status
// itself before.
type grypeDB struct {
	Built         string `json:"built"`
	SchemaVersion any    `json:"schemaVersion"`
	Status        *struct {
		Built         string `json:"built"`
		SchemaVersion any    `json:"schemaVersion"`
	} `json:"status"`
}

// ScanID is the id of the scan of digest that finished at finished.
func ScanID(digest string, finished time.Time) string {
	sum := sha256.Sum256([]byte(digest + "\x00" + finished.UTC().Format(time.RFC3339Nano)))
	return "scan_" + hex.EncodeToString(sum[:8])
}

// VulnerabilityID is stable across scans of the same digest: the digest, the
// advisory and the package.
func VulnerabilityID(digest, advisory string, p apiv1.SecurityPackage) string {
	key := p.Name + "\x00" + p.Version + "\x00" + p.Type
	if p.Purl != nil && *p.Purl != "" {
		key = *p.Purl
	}
	sum := sha256.Sum256([]byte(strings.Join([]string{digest, advisory, key}, "\x00")))
	return "vuln_" + hex.EncodeToString(sum[:8])
}

// ParseGrype turns Grype's JSON report of a scan of digest into the
// platform's scan and vulnerabilities. scan's id, digest, image, status and
// times are the caller's; ParseGrype fills the scanner, database, platform
// and result hash. It refuses a report that is not Grype's.
func ParseGrype(report []byte, scan apiv1.SecurityImageScan) (apiv1.SecurityImageScan, []apiv1.SecurityVulnerability, error) {
	var doc grypeDocument
	if err := json.Unmarshal(report, &doc); err != nil {
		return scan, nil, fmt.Errorf("the report is not Grype JSON: %w", err)
	}
	if doc.Descriptor.Name != ScannerName {
		return scan, nil, fmt.Errorf("the report's descriptor.name is %q, not %q", doc.Descriptor.Name, ScannerName)
	}
	if doc.Descriptor.Version == "" {
		return scan, nil, errors.New("the report has no descriptor.version")
	}
	scan.Scanner.Name, scan.Scanner.Version = ScannerName, doc.Descriptor.Version
	sum := sha256.Sum256(report)
	h := hex.EncodeToString(sum[:])
	scan.ResultSha256 = &h
	if db := parseGrypeDB(doc.Descriptor.DB); db != nil {
		scan.Database = db
	}
	if t := doc.Source; t != nil && t.Target.OS != "" && t.Target.Architecture != "" {
		p := t.Target.OS + "/" + t.Target.Architecture
		scan.Platform = &p
	}
	if t := doc.Source; t != nil && t.Target.ManifestDigest != "" && t.Target.ManifestDigest != scan.Digest {
		// A multi-arch index: Grype scanned one platform manifest of it.
		m := "scanned platform manifest " + t.Target.ManifestDigest + " of image index " + scan.Digest
		scan.Message = &m
	}
	var distro *string
	if doc.Distro.Name != "" {
		d := doc.Distro.Name
		if doc.Distro.Version != "" {
			d += "-" + doc.Distro.Version
		}
		distro = &d
	}

	byID := map[string]int{}
	var out []apiv1.SecurityVulnerability
	for _, m := range doc.Matches {
		v := m.Vulnerability
		if v.ID == "" || m.Artifact.Name == "" {
			continue
		}
		pkg := apiv1.SecurityPackage{Name: m.Artifact.Name, Version: m.Artifact.Version, Type: m.Artifact.Type, Distro: distro}
		if m.Artifact.PURL != "" {
			purl := m.Artifact.PURL
			pkg.Purl = &purl
		}
		id := VulnerabilityID(scan.Digest, v.ID, pkg)
		related := slices.Concat([]grypeMetadata{v.grypeMetadata}, m.RelatedVulnerabilities)
		if i, ok := byID[id]; ok { // several matchers found the same advisory and package
			out[i].Aliases = mergeStrings(out[i].Aliases, aliases(v.ID, related))
			out[i].Cvss = mergeCVSS(out[i].Cvss, related)
			out[i].Urls = mergeStrings(out[i].Urls, urls(related))
			continue
		}
		sev, basis := grypeSeverity(v.grypeMetadata)
		byID[id] = len(out)
		out = append(out, apiv1.SecurityVulnerability{
			Id: id, ScanId: scan.Id, Digest: scan.Digest, Advisory: v.ID,
			Aliases: aliases(v.ID, related), Package: pkg,
			FixedVersions: mergeStrings(nil, v.Fix.Versions), FixState: fixState(v.Fix.State),
			Severity: sev, SeverityBasis: basis,
			Cvss: mergeCVSS(nil, related), Urls: urls(related),
		})
	}
	if out == nil {
		out = []apiv1.SecurityVulnerability{}
	}
	return scan, out, nil
}

func parseGrypeDB(raw json.RawMessage) *struct {
	BuiltAt       *time.Time `json:"builtAt,omitempty"`
	SchemaVersion *string    `json:"schemaVersion,omitempty"`
} {
	var db grypeDB
	if len(raw) == 0 || json.Unmarshal(raw, &db) != nil {
		return nil
	}
	built, schema := db.Built, db.SchemaVersion
	if db.Status != nil {
		built, schema = db.Status.Built, db.Status.SchemaVersion
	}
	out := &struct {
		BuiltAt       *time.Time `json:"builtAt,omitempty"`
		SchemaVersion *string    `json:"schemaVersion,omitempty"`
	}{}
	if t, err := time.Parse(time.RFC3339, built); err == nil {
		out.BuiltAt = &t
	}
	if schema != nil && fmt.Sprint(schema) != "" {
		s := fmt.Sprint(schema)
		out.SchemaVersion = &s
	}
	if out.BuiltAt == nil && out.SchemaVersion == nil {
		return nil
	}
	return out
}

// grypeSeverity keeps the severity the advisory's source (the record's
// namespace: a distribution, NVD, GitHub) gave. It does not derive one from
// CVSS: an advisory without a severity is unknown, not low.
func grypeSeverity(v grypeMetadata) (apiv1.SecurityVulnerabilitySeverity, apiv1.SecuritySeverityBasis) {
	issuer := v.Namespace
	if issuer == "" {
		issuer = v.DataSource
	}
	if issuer == "" {
		issuer = "unnamed Grype data source"
	}
	basis := apiv1.SecuritySeverityBasis{Type: apiv1.SecuritySeverityBasisTypeVendor, Issuer: issuer, Policy: PolicyVersion}
	if v.Severity != "" {
		s := v.Severity
		basis.SourceSeverity = &s
	}
	if v.DataSource != "" {
		u := v.DataSource
		basis.SourceUrl = &u
	}
	var sev apiv1.SecurityVulnerabilitySeverity
	switch strings.ToLower(v.Severity) {
	case "critical":
		sev = apiv1.SecurityVulnerabilitySeverityCritical
	case "high":
		sev = apiv1.SecurityVulnerabilitySeverityHigh
	case "medium":
		sev = apiv1.SecurityVulnerabilitySeverityMedium
	case "low":
		sev = apiv1.SecurityVulnerabilitySeverityLow
	case "negligible":
		sev = apiv1.SecurityVulnerabilitySeverityNegligible
	default:
		sev = apiv1.SecurityVulnerabilitySeverityUnknown
	}
	reason := "the severity " + issuer + " gave the advisory, as Grype reported it"
	if sev == apiv1.SecurityVulnerabilitySeverityUnknown {
		reason = issuer + " gave no severity Grype could report; CVSS scores, if any, are listed but not converted"
	}
	basis.Reason = &reason
	return sev, basis
}

func fixState(s string) apiv1.SecurityVulnerabilityFixState {
	switch s {
	case "fixed":
		return apiv1.SecurityVulnerabilityFixStateFixed
	case "not-fixed":
		return apiv1.SecurityVulnerabilityFixStateNotFixed
	case "wont-fix":
		return apiv1.SecurityVulnerabilityFixStateWontFix
	}
	return apiv1.SecurityVulnerabilityFixStateUnknown
}

func aliases(id string, related []grypeMetadata) []string {
	var out []string
	for _, r := range related {
		if r.ID != "" && r.ID != id {
			out = append(out, r.ID)
		}
	}
	return mergeStrings(nil, out)
}

func urls(related []grypeMetadata) []string {
	var out []string
	for _, r := range related {
		out = append(out, r.URLs...)
	}
	return mergeStrings(nil, out)
}

func mergeCVSS(have []apiv1.SecurityCVSS, related []grypeMetadata) []apiv1.SecurityCVSS {
	out := slices.Clone(have)
	if out == nil {
		out = []apiv1.SecurityCVSS{}
	}
	for _, r := range related {
		for _, c := range r.CVSS {
			if c.Vector == "" {
				continue
			}
			src := c.Source
			if src == "" {
				src = r.Namespace
			}
			s := apiv1.SecurityCVSS{Source: src, Version: c.Version, Vector: c.Vector, BaseScore: c.Metrics.BaseScore}
			if !slices.Contains(out, s) {
				out = append(out, s)
			}
		}
	}
	return out
}

// mergeStrings appends add to have without duplicates or empty strings and
// never returns nil.
func mergeStrings(have, add []string) []string {
	out := slices.Clone(have)
	if out == nil {
		out = []string{}
	}
	for _, s := range add {
		if s != "" && !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}
