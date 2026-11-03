package api

import (
	"encoding/json"
	"testing"
)

func TestSecurityOCSFExport(t *testing.T) {
	h := newHarness(t, withSecurity())
	if code, _ := h.do("GET", "/v1/security/ocsf", "", nil); code != 401 {
		t.Fatalf("without a token: %d", code)
	}
	code, b := h.do("GET", "/v1/security/ocsf", viewerToken, nil)
	if code != 200 {
		t.Fatalf("%d %s", code, b)
	}
	var exp struct {
		OCSFVersion string           `json:"ocsfVersion"`
		Findings    []map[string]any `json:"findings"`
	}
	if err := json.Unmarshal(b, &exp); err != nil {
		t.Fatal(err)
	}
	if exp.OCSFVersion != "1.9.0" || len(exp.Findings) == 0 {
		t.Fatalf("%s", b)
	}
	var privileged map[string]any
	for _, e := range exp.Findings {
		if e["finding_info"].(map[string]any)["analytic"].(map[string]any)["name"] == "k8s.privileged" {
			privileged = e
		}
	}
	if privileged == nil || privileged["class_uid"] != float64(2003) || privileged["severity_id"] != float64(4) ||
		privileged["cloud"].(map[string]any)["provider"] != "GCP" {
		t.Fatalf("privileged container event: %v", privileged)
	}

	// The same filters as /v1/security.
	_, all := h.do("GET", "/v1/security", viewerToken, nil)
	var rep struct{ Findings []any }
	_ = json.Unmarshal(all, &rep)
	if len(rep.Findings) != len(exp.Findings) {
		t.Fatalf("ocsf has %d events, report %d findings", len(exp.Findings), len(rep.Findings))
	}
	if code, b := h.do("GET", "/v1/security/ocsf?severity=critical", viewerToken, nil); code != 200 || json.Unmarshal(b, &exp) != nil || len(exp.Findings) != 0 {
		t.Fatalf("critical only: %d %s", code, b)
	}
	if code, _ := h.do("GET", "/v1/security/ocsf?environment=mars", viewerToken, nil); code != 400 {
		t.Fatalf("unknown environment: %d", code)
	}
}
