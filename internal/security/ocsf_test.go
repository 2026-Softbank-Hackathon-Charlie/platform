package security

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

// ocsfSchemas compiles the official OCSF class schemas in testdata (fetched
// from schema.ocsf.io with the cloud profile; see testdata/ocsf-1.9.0/README.md).
func ocsfSchemas(t *testing.T) map[int]*jsonschema.Schema {
	t.Helper()
	dir := filepath.Join("testdata", "ocsf-"+OCSFVersion)
	out := map[int]*jsonschema.Schema{}
	for class, file := range map[int]string{ocsfCompliance: "compliance_finding.json", ocsfDetection: "detection_finding.json"} {
		f, err := os.Open(filepath.Join(dir, file))
		if err != nil {
			t.Fatal(err)
		}
		doc, err := jsonschema.UnmarshalJSON(f)
		f.Close()
		if err != nil {
			t.Fatal(err)
		}
		c := jsonschema.NewCompiler()
		if err := c.AddResource(file, doc); err != nil {
			t.Fatal(err)
		}
		s, err := c.Compile(file)
		if err != nil {
			t.Fatalf("compile %s: %v", file, err)
		}
		out[class] = s
	}
	return out
}

func validate(t *testing.T, schemas map[int]*jsonschema.Schema, e OCSFFinding) error {
	t.Helper()
	raw, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	inst, err := jsonschema.UnmarshalJSON(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	return schemas[e.ClassUID].Validate(inst)
}

var observed = time.Date(2026, 11, 3, 7, 0, 0, 0, time.UTC)

// One finding of each kind the collectors produce.
var sample = []apiv1.SecurityFinding{
	{Id: "f1", Category: apiv1.SecurityCategoryWorkload, Check: "k8s.privileged", Title: "Privileged container", Detail: "web runs privileged", Environment: "gke",
		ObservedAt: observed, Provider: apiv1.SecurityProviderGcp, Recommendation: "Drop privileged.", Resource: "shop/Deployment/web", Severity: apiv1.SecuritySeverityHigh, Source: "kubernetes"},
	{Id: "f2", Category: apiv1.SecurityCategoryThreat, Check: "guardduty.Recon:EC2/PortProbeUnprotectedPort", Title: "Unprotected port is being probed", Detail: "probed",
		Environment: "aws-account", ObservedAt: observed, Provider: apiv1.SecurityProviderAws, Resource: "instance/i-1", Severity: apiv1.SecuritySeverityMedium, Source: "aws-guardduty"},
	{Id: "f3", Category: apiv1.SecurityCategoryData, Check: "s3.public-access-block", Title: "S3 buckets should block public access", Environment: "aws-account",
		ObservedAt: observed, Provider: apiv1.SecurityProviderAws, Recommendation: "Block public access.", Resource: "arn:aws:s3:::shop-media", Severity: apiv1.SecuritySeverityCritical, Source: "aws-securityhub"},
	{Id: "f4", Category: apiv1.SecurityCategoryLogging, Check: "platform.audit-denied", Title: "Denied requests", Environment: "platform",
		ObservedAt: observed, Provider: apiv1.SecurityProviderPlatform, Severity: apiv1.SecuritySeverityInfo, Source: "platform-audit"},
	{Id: "f5", Category: apiv1.SecurityCategoryNetwork, Check: "k8s.no-networkpolicy", Title: "No NetworkPolicy", Environment: "onprem",
		ObservedAt: observed, Provider: apiv1.SecurityProviderOnprem, Severity: apiv1.SecuritySeverityLow, Source: "kubernetes", Resource: "shop"},
}

func TestOCSFEventsMatchTheOfficialSchema(t *testing.T) {
	schemas := ocsfSchemas(t)
	exp := ToOCSF(apiv1.SecurityReport{GeneratedAt: observed, Findings: sample})
	if exp.OCSFVersion != "1.9.0" || len(exp.Findings) != len(sample) {
		t.Fatalf("%+v", exp)
	}
	for _, e := range exp.Findings {
		if err := validate(t, schemas, e); err != nil {
			t.Errorf("%s (class %d) is not valid OCSF %s: %v", e.FindingInfo.UID, e.ClassUID, OCSFVersion, err)
		}
	}
}

func TestOCSFMapping(t *testing.T) {
	byID := map[string]OCSFFinding{}
	for _, e := range ToOCSF(apiv1.SecurityReport{Findings: sample}).Findings {
		byID[e.FindingInfo.UID] = e
	}
	for id, want := range map[string]struct {
		class, typeUID, severity, compliance int
		provider                             string
	}{
		"f1": {2003, 200301, 4, 3, "GCP"},
		"f2": {2004, 200401, 3, 0, "AWS"},
		"f3": {2003, 200301, 5, 3, "AWS"},
		"f4": {2003, 200301, 1, 2, "platform"},
		"f5": {2003, 200301, 2, 3, "onprem"},
	} {
		e := byID[id]
		got := 0
		if e.Compliance != nil {
			got = e.Compliance.StatusID
		}
		if e.ClassUID != want.class || e.TypeUID != want.typeUID || e.SeverityID != want.severity || got != want.compliance || e.Cloud.Provider != want.provider {
			t.Errorf("%s: %+v", id, e)
		}
		if e.Time != observed.UnixMilli() || e.FindingInfo.DataSources[0] == "" || e.FindingInfo.Analytic.Name == "" {
			t.Errorf("%s: %+v", id, e)
		}
	}
	if e := byID["f4"]; e.Resources != nil || e.Remediation != nil {
		t.Errorf("empty resource or recommendation must be left out: %+v", e)
	}
}

// The validator itself must reject what the schema forbids, or the test above proves nothing.
func TestOCSFValidatorRejectsInvalidEvents(t *testing.T) {
	schemas := ocsfSchemas(t)
	good := OCSF(sample[0])
	for name, change := range map[string]func(*OCSFFinding){
		"missing finding uid":    func(e *OCSFFinding) { e.FindingInfo.UID = "" },
		"wrong class for schema": func(e *OCSFFinding) { e.ClassUID = 2004 },
		"severity out of range":  func(e *OCSFFinding) { e.SeverityID = 42 },
	} {
		t.Run(name, func(t *testing.T) {
			e := good
			change(&e)
			raw, _ := json.Marshal(e)
			// "" uid still marshals; an empty required string is allowed by OCSF, so drop the key instead.
			var m map[string]any
			_ = json.Unmarshal(raw, &m)
			if name == "missing finding uid" {
				delete(m["finding_info"].(map[string]any), "uid")
			}
			b, _ := json.Marshal(m)
			inst, _ := jsonschema.UnmarshalJSON(strings.NewReader(string(b)))
			if err := schemas[ocsfCompliance].Validate(inst); err == nil {
				t.Fatal("accepted")
			}
		})
	}
	// An unknown field is rejected too (additionalProperties: false).
	raw, _ := json.Marshal(good)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	m["hopcharlie_extra"] = true
	b, _ := json.Marshal(m)
	inst, _ := jsonschema.UnmarshalJSON(strings.NewReader(string(b)))
	if err := schemas[ocsfCompliance].Validate(inst); err == nil {
		t.Fatal("unknown field accepted")
	}
}
