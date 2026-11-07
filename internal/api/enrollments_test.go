package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/onprem"
)

func TestEnrollmentRealIssuanceAndOneUseHTTP(t *testing.T) {
	h := newHarness(t)
	path := "/v1/onprem/enrollments"
	body := map[string]any{"environment": "onprem", "installK3s": false}
	for _, token := range []string{viewerToken, agentToken} {
		if code, _ := h.do("POST", path, token, body, "Idempotency-Key", "bootstrap-one"); code != 403 {
			t.Fatalf("role escaped create: %d", code)
		}
	}
	code, b := h.do("POST", path, opToken, body, "Idempotency-Key", "bootstrap-one", "X-Forwarded-Host", "attacker.invalid")
	if code != 201 {
		t.Fatalf("create HTTP %d", code)
	}
	a := decode[onprem.Created](t, b)
	if len(a.Code) != 47 || !strings.Contains(a.InstallCommand, "https://api.hopcharlie.dev/v1/onprem/installer.sh") || strings.Contains(a.InstallCommand, "attacker.invalid") {
		t.Fatal("not a real, trusted-origin issuance")
	}
	idpath := path + "/" + a.Enrollment.ID
	for _, methodPath := range []struct{ method, path string }{{"GET", idpath}, {"POST", idpath + "/revoke"}} {
		if status, _ := h.do(methodPath.method, methodPath.path, viewerToken, nil); status != 403 {
			t.Fatal("viewer read or revoked a bootstrap enrollment")
		}
	}
	code, b = h.do("POST", path, opToken, body, "Idempotency-Key", "bootstrap-one")
	replay := decode[onprem.Created](t, b)
	if code != 200 || replay.Enrollment.ID != a.Enrollment.ID || replay.Code != "" || replay.InstallCommand != "" {
		t.Fatal("HTTP retry exposed or recreated code")
	}
	_, b = h.do("GET", idpath, opToken, nil)
	for _, secret := range []string{a.Code, "codeHash", "installCommand"} {
		if strings.Contains(string(b), secret) {
			t.Fatal("GET exposed issuance secret")
		}
	}
	claim := map[string]any{"code": a.Code, "environment": "onprem", "installK3s": false}
	if code, b = h.do("POST", idpath+"/claim", "", claim); code != 200 {
		t.Fatalf("real claim HTTP %d", code)
	}
	var result struct {
		Enrollment onprem.Enrollment `json:"enrollment"`
	}
	if json.Unmarshal(b, &result) != nil || result.Enrollment.Status != "claimed" {
		t.Fatal("claim is not persisted")
	}
	for _, check := range result.Enrollment.Checks {
		if check.Status != "unknown" {
			t.Fatal("unconfigured worker invented ready checks")
		}
	}
	if code, _ = h.do("POST", idpath+"/claim", "", claim); code != 409 {
		t.Fatal("bootstrap code was used twice")
	}
	if code, _ = h.do("POST", idpath+"/revoke", opToken, nil); code != 200 {
		t.Fatal("revoke failed")
	}
	if code, _ = h.do("POST", idpath+"/claim", "", claim); code != 409 {
		t.Fatal("revoked grant reused")
	}
	entries, err := h.store.ListAudit(t.Context(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(string(e.Detail), a.Code) {
			t.Fatal("audit contains bootstrap code")
		}
	}
}

func TestEnrollmentInputBoundsExpiryAndInstaller(t *testing.T) {
	h := newHarness(t)
	now := time.Now().UTC()
	h.api.now = func() time.Time { return now }
	p := "/v1/onprem/enrollments"
	body := `{"environment":"onprem","installK3s":false}`
	for _, bad := range []string{body + `{}`, `{"environment":"onprem"}`, `{"environment":"onprem;bad","installK3s":false}`, `{"environment":"onprem","installK3s":false,"unexpected":"no"}`} {
		if status, _ := h.do("POST", p, opToken, bad, "Idempotency-Key", "bounds-one"); status != 400 {
			t.Fatalf("invalid input HTTP %d", status)
		}
	}
	if status, _ := h.do("POST", p, opToken, body); status != 400 {
		t.Fatal("idempotency key is required")
	}
	status, b := h.do("POST", p, opToken, body, "Idempotency-Key", "expiry-one")
	if status != 201 {
		t.Fatal("expiry issuance failed")
	}
	a := decode[onprem.Created](t, b)
	now = now.Add(15 * time.Minute)
	status, _ = h.do("POST", p+"/"+a.Enrollment.ID+"/claim", "", map[string]any{"code": a.Code, "environment": "onprem", "installK3s": false})
	if status != 409 {
		t.Fatal("expired code accepted")
	}
	status, b = h.do("GET", p+"/"+a.Enrollment.ID, opToken, nil)
	if status != 200 || !strings.Contains(string(b), `"status":"expired"`) {
		t.Fatal("expiry not persisted")
	}
	if status, _ = h.do("POST", p+"/"+a.Enrollment.ID+"/claim", "", strings.Repeat("x", 4097)); status != 413 {
		t.Fatalf("body bound HTTP %d", status)
	}
	status, b = h.do("GET", "/v1/onprem/installer.sh", "", nil)
	if status != 200 || string(b) != onprem.Installer || !strings.Contains(string(b), "sha256sum -c") || strings.Contains(string(b), "curl -k") {
		t.Fatal("installer not actually served with TLS and hash checks")
	}
	req, _ := http.NewRequest("POST", h.srv.URL+p, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+opToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "origin-one")
	req.Host = "attacker.invalid"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out onprem.Created
	if json.NewDecoder(resp.Body).Decode(&out) != nil || strings.Contains(out.InstallCommand, "attacker.invalid") {
		t.Fatal("request Host affected installer command")
	}
}

func TestEnrollmentClaimRateBound(t *testing.T) {
	h := newHarness(t)
	for i := 0; i < 21; i++ {
		status, _ := h.do("POST", "/v1/onprem/enrollments/enr_000000000000000000000000/claim", "", map[string]any{"code": "onp_" + strings.Repeat("x", 43), "environment": "onprem", "installK3s": false})
		if i < 20 && status != 401 {
			t.Fatalf("unexpected claim status %d", status)
		}
		if i == 20 && status != 429 {
			t.Fatalf("rate limit not enforced: %d", status)
		}
	}
}
