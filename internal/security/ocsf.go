package security

import (
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

// OCSFVersion is the Open Cybersecurity Schema Framework version the export
// follows. testdata/ocsf-<version> holds the official class schemas (with the
// cloud profile) that the tests validate every event against.
const OCSFVersion = "1.9.0"

// OCSF class and enum values, from the official schema (schema.ocsf.io).
const (
	ocsfCategoryFindings = 2 // category_uid: Findings
	ocsfCompliance       = 2003
	ocsfDetection        = 2004
	ocsfActivityCreate   = 1 // activity_id: Create
	ocsfStatusNew        = 1 // status_id: New
	ocsfRule             = 1 // analytic.type_id: Rule
	ocsfComplianceWarn   = 2 // compliance.status_id: Warning
	ocsfComplianceFail   = 3 // compliance.status_id: Fail
)

// OCSFFinding is one finding as an OCSF event: a Detection Finding (2004) for
// threat detections, a Compliance Finding (2003) for configuration checks.
// Only fields the official schema defines are written; it rejects any other.
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

type OCSFCloud struct {
	Provider string `json:"provider"`
}

type OCSFFindingInfo struct {
	UID         string       `json:"uid"`
	Title       string       `json:"title,omitempty"`
	Desc        string       `json:"desc,omitempty"`
	Types       []string     `json:"types,omitempty"`
	DataSources []string     `json:"data_sources,omitempty"`
	Analytic    OCSFAnalytic `json:"analytic"`
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

// OCSFExport is the security report as OCSF events.
type OCSFExport struct {
	OCSFVersion string        `json:"ocsfVersion"`
	GeneratedAt time.Time     `json:"generatedAt"`
	Findings    []OCSFFinding `json:"findings"`
}

// ToOCSF converts a (filtered) report. It reads nothing new: every value comes
// from the report's own findings.
func ToOCSF(rep apiv1.SecurityReport) OCSFExport {
	out := OCSFExport{OCSFVersion: OCSFVersion, GeneratedAt: rep.GeneratedAt, Findings: make([]OCSFFinding, 0, len(rep.Findings))}
	for _, f := range rep.Findings {
		out.Findings = append(out.Findings, OCSF(f))
	}
	return out
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
			Analytic:    OCSFAnalytic{TypeID: ocsfRule, Type: "Rule", Name: f.Check},
		},
		Metadata: OCSFMetadata{
			Version:  OCSFVersion,
			Product:  OCSFProduct{Name: "HopCharlie", VendorName: "team-charlie"},
			Profiles: []string{"cloud"},
		},
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
