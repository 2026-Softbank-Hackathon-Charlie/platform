package security

import (
	"cmp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

// OCSFVersion is the Open Cybersecurity Schema Framework version the export
// follows. testdata/ocsf-<version> holds the official class schemas (with the
// cloud profile) that the tests validate every event against.
const OCSFVersion = "1.9.0"

// OCSF class and enum values, from the official schema (schema.ocsf.io).
const (
	ocsfCategoryFindings  = 2 // category_uid: Findings
	ocsfVulnerability     = 2002
	ocsfCompliance        = 2003
	ocsfDetection         = 2004
	ocsfActivityCreate    = 1 // activity_id: Create
	ocsfStatusNew         = 1 // status_id: New
	ocsfStatusSuppressed  = 3 // status_id: Suppressed (an active exception, SEC-08)
	ocsfRule              = 1 // analytic.type_id: Rule
	ocsfComplianceUnknown = 0 // compliance.status_id: Unknown
	ocsfCompliancePass    = 1 // compliance.status_id: Pass
	ocsfComplianceWarn    = 2 // compliance.status_id: Warning
	ocsfComplianceFail    = 3 // compliance.status_id: Fail
)

// kevCatalogURL is the exploit reference of a KEV-listed CVE. OCSF 1.9.0 has
// no KEV field.
const kevCatalogURL = "https://www.cisa.gov/known-exploited-vulnerabilities-catalog"

// OCSFFinding is one finding as an OCSF event: a Detection Finding (2004) for
// threat detections, a Compliance Finding (2003) for configuration checks and
// control evaluations. Only fields the official schema defines are written;
// it rejects any other.
type OCSFFinding struct {
	ActivityID   int    `json:"activity_id"`
	ActivityName string `json:"activity_name"`
	CategoryUID  int    `json:"category_uid"`
	CategoryName string `json:"category_name"`
	ClassUID     int    `json:"class_uid"`
	ClassName    string `json:"class_name"`
	TypeUID      int    `json:"type_uid"`
	TypeName     string `json:"type_name"`
	SeverityID   int    `json:"severity_id"`
	Severity     string `json:"severity"`
	StatusID     int    `json:"status_id"`
	Status       string `json:"status"`
	// StatusDetail is the reason of the finding's active exception.
	StatusDetail string `json:"status_detail,omitempty"`
	// Time is when the source reported the finding, in epoch milliseconds.
	Time        int64              `json:"time"`
	Message     string             `json:"message,omitempty"`
	Cloud       OCSFCloud          `json:"cloud"`
	FindingInfo OCSFFindingInfo    `json:"finding_info"`
	Metadata    OCSFMetadata       `json:"metadata"`
	Resources   []OCSFResource     `json:"resources,omitempty"`
	Remediation *OCSFRemediation   `json:"remediation,omitempty"`
	Compliance  *OCSFComplianceObj `json:"compliance,omitempty"`
}

// OCSFVulnerabilityFinding is one image vulnerability as an OCSF
// Vulnerability Finding (2002): the common finding fields and exactly one
// vulnerability.
type OCSFVulnerabilityFinding struct {
	ActivityID   int    `json:"activity_id"`
	ActivityName string `json:"activity_name"`
	CategoryUID  int    `json:"category_uid"`
	CategoryName string `json:"category_name"`
	ClassUID     int    `json:"class_uid"`
	ClassName    string `json:"class_name"`
	TypeUID      int    `json:"type_uid"`
	TypeName     string `json:"type_name"`
	SeverityID   int    `json:"severity_id"`
	Severity     string `json:"severity"`
	StatusID     int    `json:"status_id"`
	Status       string `json:"status"`
	// StatusDetail is the reason of the vulnerability's active exception.
	StatusDetail string `json:"status_detail,omitempty"`
	// Time is when the image was scanned (or the report built), in epoch milliseconds.
	Time            int64               `json:"time"`
	Message         string              `json:"message,omitempty"`
	Cloud           OCSFCloud           `json:"cloud"`
	FindingInfo     OCSFFindingInfo     `json:"finding_info"`
	Metadata        OCSFMetadata        `json:"metadata"`
	Resources       []OCSFResource      `json:"resources,omitempty"`
	Vulnerabilities []OCSFVulnerability `json:"vulnerabilities"`
}

// OCSFVulnerability is the OCSF vulnerability object. A KEV-listed CVE sets
// IsExploitAvailable and ExploitRefURL; otherwise both are left out.
type OCSFVulnerability struct {
	CVE                OCSFCVE               `json:"cve"`
	Title              string                `json:"title,omitempty"`
	Severity           string                `json:"severity,omitempty"`
	AffectedPackages   []OCSFAffectedPackage `json:"affected_packages"`
	IsFixAvailable     *bool                 `json:"is_fix_available,omitempty"`
	IsExploitAvailable *bool                 `json:"is_exploit_available,omitempty"`
	ExploitRefURL      string                `json:"exploit_ref_url,omitempty"`
	References         []string              `json:"references,omitempty"`
}

// OCSFCVE is the OCSF cve object; UID is the advisory id when there is no CVE.
type OCSFCVE struct {
	UID  string     `json:"uid"`
	CVSS []OCSFCVSS `json:"cvss,omitempty"`
	EPSS *OCSFEPSS  `json:"epss,omitempty"`
}

type OCSFCVSS struct {
	BaseScore    float64 `json:"base_score"`
	Version      string  `json:"version"`
	VectorString string  `json:"vector_string,omitempty"`
}

// OCSFEPSS is the OCSF epss object; the schema types score as a string.
type OCSFEPSS struct {
	Score      string   `json:"score"`
	Percentile *float64 `json:"percentile,omitempty"`
}

type OCSFAffectedPackage struct {
	Name           string `json:"name"`
	Version        string `json:"version"`
	Purl           string `json:"purl,omitempty"`
	Type           string `json:"type,omitempty"`
	FixedInVersion string `json:"fixed_in_version,omitempty"`
}

type OCSFCloud struct {
	Provider string `json:"provider"`
}

type OCSFFindingInfo struct {
	UID         string        `json:"uid"`
	Title       string        `json:"title,omitempty"`
	Desc        string        `json:"desc,omitempty"`
	Types       []string      `json:"types,omitempty"`
	DataSources []string      `json:"data_sources,omitempty"`
	Analytic    *OCSFAnalytic `json:"analytic,omitempty"`
}

type OCSFAnalytic struct {
	TypeID int    `json:"type_id"`
	Type   string `json:"type"`
	Name   string `json:"name,omitempty"`
}

type OCSFMetadata struct {
	Version  string      `json:"version"`
	Product  OCSFProduct `json:"product"`
	Profiles []string    `json:"profiles"`
}

type OCSFProduct struct {
	Name       string `json:"name"`
	VendorName string `json:"vendor_name"`
}

type OCSFResource struct {
	UID    string   `json:"uid"`
	Labels []string `json:"labels,omitempty"`
}

type OCSFRemediation struct {
	Desc string `json:"desc"`
}

type OCSFComplianceObj struct {
	Control  string `json:"control,omitempty"`
	StatusID int    `json:"status_id"`
	Status   string `json:"status"`
}

// OCSFExport is the security report as OCSF events. Every array is present,
// possibly empty.
type OCSFExport struct {
	OCSFVersion     string                     `json:"ocsfVersion"`
	GeneratedAt     time.Time                  `json:"generatedAt"`
	Findings        []OCSFFinding              `json:"findings"`
	Vulnerabilities []OCSFVulnerabilityFinding `json:"vulnerabilities"`
	Controls        []OCSFFinding              `json:"controls"`
}

// ToOCSF converts a (filtered) report. It reads nothing new: every value comes
// from the report's own findings. Vulnerabilities and controls are empty.
func ToOCSF(rep apiv1.SecurityReport) OCSFExport {
	return ToOCSFAll(rep, nil, nil)
}

// ToOCSFAll converts a (filtered) report, a vulnerability report and a control
// report; a nil report gives an empty array. not_applicable control
// evaluations are left out. Cloud providers come from the report's sources.
func ToOCSFAll(rep apiv1.SecurityReport, vulns *apiv1.SecurityVulnerabilityReport, controls *apiv1.SecurityControlReport) OCSFExport {
	out := OCSFExport{OCSFVersion: OCSFVersion, GeneratedAt: rep.GeneratedAt, Findings: make([]OCSFFinding, 0, len(rep.Findings)),
		Vulnerabilities: []OCSFVulnerabilityFinding{}, Controls: []OCSFFinding{}}
	for _, f := range rep.Findings {
		out.Findings = append(out.Findings, OCSF(f))
	}
	provider := map[string]string{} // environment -> OCSF cloud provider
	for _, s := range rep.Sources {
		provider[s.Environment] = ocsfProvider(s.Provider)
	}
	if vulns != nil {
		out.Vulnerabilities = ocsfVulnerabilities(*vulns, provider)
	}
	if controls != nil {
		byID := map[string]apiv1.SecurityControl{}
		for _, c := range controls.Controls {
			byID[c.Id] = c
		}
		for _, ev := range controls.Evaluations {
			if ev.Result == apiv1.SecurityControlResultNotApplicable {
				continue
			}
			c, ok := byID[ev.ControlId]
			if !ok {
				c = apiv1.SecurityControl{Id: ev.ControlId}
			}
			e := OCSFControlEvent(ev, c)
			if p, ok := provider[ev.Environment]; ok {
				e.Cloud.Provider = p
			}
			out.Controls = append(out.Controls, e)
		}
	}
	return out
}

// ocsfVulnerabilities converts every vulnerability of r. Its time is the
// finish of its scan in r, else r's generatedAt; its cloud is the one every
// environment running the image is on, else the platform.
func ocsfVulnerabilities(r apiv1.SecurityVulnerabilityReport, provider map[string]string) []OCSFVulnerabilityFinding {
	scanTime := map[string]time.Time{}
	for _, s := range r.Scans {
		if s.FinishedAt != nil {
			scanTime[s.Id] = *s.FinishedAt
		}
	}
	envs := map[string][]string{} // digest -> environments running it
	for _, img := range r.RunningImages {
		if img.Digest != nil && !slices.Contains(envs[*img.Digest], img.Environment) {
			envs[*img.Digest] = append(envs[*img.Digest], img.Environment)
		}
	}
	out := make([]OCSFVulnerabilityFinding, 0, len(r.Vulnerabilities))
	for _, v := range r.Vulnerabilities {
		at, ok := scanTime[v.ScanId]
		if !ok {
			at = r.GeneratedAt
		}
		e := OCSFVulnerabilityEvent(v, at)
		clouds := map[string]bool{}
		for _, env := range envs[v.Digest] {
			clouds[cmp.Or(provider[env], string(apiv1.SecurityProviderPlatform))] = true
			e.Resources[0].Labels = append(e.Resources[0].Labels, "environment:"+env)
		}
		if len(clouds) == 1 {
			for c := range clouds {
				e.Cloud.Provider = c
			}
		}
		out = append(out, e)
	}
	return out
}

func ocsfMetadata() OCSFMetadata {
	return OCSFMetadata{
		Version:  OCSFVersion,
		Product:  OCSFProduct{Name: "HopCharlie", VendorName: "team-charlie"},
		Profiles: []string{"cloud"},
	}
}

// OCSF converts one finding.
func OCSF(f apiv1.SecurityFinding) OCSFFinding {
	sevID, sev := ocsfSeverity(f.Severity)
	e := OCSFFinding{
		ActivityID:   ocsfActivityCreate,
		ActivityName: "Create",
		CategoryUID:  ocsfCategoryFindings,
		CategoryName: "Findings",
		SeverityID:   sevID,
		Severity:     sev,
		StatusID:     ocsfStatusNew,
		Status:       "New",
		Time:         f.ObservedAt.UnixMilli(),
		Message:      f.Title,
		Cloud:        OCSFCloud{Provider: ocsfProvider(f.Provider)},
		FindingInfo: OCSFFindingInfo{
			UID:         f.Id,
			Title:       f.Title,
			Desc:        f.Detail,
			Types:       []string{string(f.Category)},
			DataSources: []string{f.Source},
			Analytic:    &OCSFAnalytic{TypeID: ocsfRule, Type: "Rule", Name: f.Check},
		},
		Metadata: ocsfMetadata(),
	}
	if f.Exception != nil {
		// An operator accepted the risk until the exception ends; severity stays.
		e.StatusID, e.Status, e.StatusDetail = ocsfStatusSuppressed, "Suppressed", f.Exception.Reason
	}
	if f.Resource != "" {
		e.Resources = []OCSFResource{{UID: f.Resource, Labels: []string{"environment:" + f.Environment}}}
	}
	if f.Recommendation != "" {
		e.Remediation = &OCSFRemediation{Desc: f.Recommendation}
	}
	if f.Category == apiv1.SecurityCategoryThreat {
		e.ClassUID, e.ClassName = ocsfDetection, "Detection Finding"
	} else {
		// A configuration check that found something. Informational findings
		// are reported as a warning, everything else as a failed control.
		e.ClassUID, e.ClassName = ocsfCompliance, "Compliance Finding"
		c := &OCSFComplianceObj{Control: f.Check, StatusID: ocsfComplianceFail, Status: "Fail"}
		if f.Severity == apiv1.SecuritySeverityInfo {
			c.StatusID, c.Status = ocsfComplianceWarn, "Warning"
		}
		e.Compliance = c
	}
	e.TypeUID = e.ClassUID*100 + e.ActivityID
	e.TypeName = e.ClassName + ": Create"
	return e
}

// OCSFVulnerabilityEvent converts one vulnerability of an image scanned at
// at. Its cloud is the platform (whose scanner found it) until ToOCSFAll
// knows where the image runs.
func OCSFVulnerabilityEvent(v apiv1.SecurityVulnerability, at time.Time) OCSFVulnerabilityFinding {
	sevID, sev := ocsfSeverity(vulnerabilitySeverity(v.Severity))
	title := v.Advisory + " in " + v.Package.Name
	e := OCSFVulnerabilityFinding{
		ActivityID:   ocsfActivityCreate,
		ActivityName: "Create",
		CategoryUID:  ocsfCategoryFindings,
		CategoryName: "Findings",
		ClassUID:     ocsfVulnerability,
		ClassName:    "Vulnerability Finding",
		TypeUID:      ocsfVulnerability*100 + ocsfActivityCreate,
		TypeName:     "Vulnerability Finding: Create",
		SeverityID:   sevID,
		Severity:     sev,
		StatusID:     ocsfStatusNew,
		Status:       "New",
		Time:         at.UnixMilli(),
		Message:      title,
		Cloud:        OCSFCloud{Provider: string(apiv1.SecurityProviderPlatform)},
		FindingInfo:  OCSFFindingInfo{UID: v.Id, Title: title},
		Metadata:     ocsfMetadata(),
		Resources:    []OCSFResource{{UID: v.Digest}},
	}
	if v.Exception != nil {
		// An operator accepted the risk until the exception ends; severity stays.
		e.StatusID, e.Status, e.StatusDetail = ocsfStatusSuppressed, "Suppressed", v.Exception.Reason
	}
	x := OCSFVulnerability{
		CVE:        OCSFCVE{UID: cmp.Or(VulnerabilityCVE(v), v.Advisory)},
		Title:      v.Advisory,
		Severity:   sev,
		References: v.Urls,
	}
	for _, c := range v.Cvss {
		x.CVE.CVSS = append(x.CVE.CVSS, OCSFCVSS{BaseScore: c.BaseScore, Version: c.Version, VectorString: c.Vector})
	}
	p := OCSFAffectedPackage{Name: v.Package.Name, Version: v.Package.Version, Type: v.Package.Type}
	if v.Package.Purl != nil {
		p.Purl = *v.Package.Purl
	}
	if len(v.FixedVersions) > 0 {
		p.FixedInVersion = v.FixedVersions[0]
	}
	x.AffectedPackages = []OCSFAffectedPackage{p}
	switch v.FixState {
	case apiv1.SecurityVulnerabilityFixStateFixed:
		x.IsFixAvailable = ptrTo(true)
	case apiv1.SecurityVulnerabilityFixStateNotFixed, apiv1.SecurityVulnerabilityFixStateWontFix:
		x.IsFixAvailable = ptrTo(false)
	} // unknown: left out, not false
	if ex := v.Exploitation; ex != nil {
		if ex.Kev.State == apiv1.SecurityKEVStateListed {
			x.IsExploitAvailable, x.ExploitRefURL = ptrTo(true), kevCatalogURL
		}
		if ex.Epss.State == apiv1.SecurityEPSSStateScored && ex.Epss.Probability != nil {
			x.CVE.EPSS = &OCSFEPSS{Score: strconv.FormatFloat(*ex.Epss.Probability, 'f', -1, 64), Percentile: ex.Epss.Percentile}
		}
	}
	e.Vulnerabilities = []OCSFVulnerability{x}
	return e
}

// vulnerabilitySeverity maps a package vulnerability's severity to the
// finding scale: negligible is Informational, unknown stays Unknown.
func vulnerabilitySeverity(s apiv1.SecurityVulnerabilitySeverity) apiv1.SecuritySeverity {
	switch s {
	case apiv1.SecurityVulnerabilitySeverityNegligible:
		return apiv1.SecuritySeverityInfo
	case apiv1.SecurityVulnerabilitySeverityUnknown:
		return ""
	}
	return apiv1.SecuritySeverity(s)
}

// OCSFControlEvent converts one evaluation of control c to a Compliance
// Finding (2003): pass is Pass, fail is Fail, anything else Unknown. A failing
// evaluation has the control's fail severity (Unknown when it varies); a pass
// or unknown one is Informational. Its cloud is the platform until ToOCSFAll
// knows the environment's.
func OCSFControlEvent(ev apiv1.SecurityControlEvaluation, c apiv1.SecurityControl) OCSFFinding {
	sevID, sev := ocsfSeverity(apiv1.SecuritySeverityInfo)
	comp := &OCSFComplianceObj{Control: c.Id, StatusID: ocsfComplianceUnknown, Status: "Unknown"}
	switch ev.Result {
	case apiv1.SecurityControlResultPass:
		comp.StatusID, comp.Status = ocsfCompliancePass, "Pass"
	case apiv1.SecurityControlResultFail:
		comp.StatusID, comp.Status = ocsfComplianceFail, "Fail"
		sevID, sev = ocsfSeverity(apiv1.SecuritySeverity(c.FailSeverity))
	}
	title := strings.TrimSpace(c.Id + " " + c.Title)
	e := OCSFFinding{
		ActivityID:   ocsfActivityCreate,
		ActivityName: "Create",
		CategoryUID:  ocsfCategoryFindings,
		CategoryName: "Findings",
		ClassUID:     ocsfCompliance,
		ClassName:    "Compliance Finding",
		TypeUID:      ocsfCompliance*100 + ocsfActivityCreate,
		TypeName:     "Compliance Finding: Create",
		SeverityID:   sevID,
		Severity:     sev,
		StatusID:     ocsfStatusNew,
		Status:       "New",
		Time:         ev.EvaluatedAt.UnixMilli(),
		Message:      title,
		Cloud:        OCSFCloud{Provider: string(apiv1.SecurityProviderPlatform)},
		FindingInfo: OCSFFindingInfo{
			UID:      ev.Id,
			Title:    title,
			Desc:     ev.Reason,
			Analytic: &OCSFAnalytic{TypeID: ocsfRule, Type: "Rule", Name: cmp.Or(c.Check, c.Id)},
		},
		Metadata:   ocsfMetadata(),
		Compliance: comp,
	}
	if c.Domain != "" {
		e.FindingInfo.Types = []string{string(c.Domain)}
	}
	if ev.Source != "" {
		e.FindingInfo.DataSources = []string{ev.Source}
	}
	if ev.Asset != "" {
		e.Resources = []OCSFResource{{UID: ev.Asset, Labels: []string{"environment:" + ev.Environment}}}
	}
	return e
}

func ptrTo[T any](v T) *T { return &v }

func ocsfSeverity(s apiv1.SecuritySeverity) (int, string) {
	switch s {
	case apiv1.SecuritySeverityInfo:
		return 1, "Informational"
	case apiv1.SecuritySeverityLow:
		return 2, "Low"
	case apiv1.SecuritySeverityMedium:
		return 3, "Medium"
	case apiv1.SecuritySeverityHigh:
		return 4, "High"
	case apiv1.SecuritySeverityCritical:
		return 5, "Critical"
	}
	return 0, "Unknown"
}

// ocsfProvider names the cloud as OCSF examples do (AWS, GCP); environments
// outside a public cloud keep the platform's own name.
func ocsfProvider(p apiv1.SecurityProvider) string {
	switch p {
	case apiv1.SecurityProviderAws:
		return "AWS"
	case apiv1.SecurityProviderGcp:
		return "GCP"
	case apiv1.SecurityProviderCloudflare:
		return "Cloudflare"
	}
	return string(p)
}
