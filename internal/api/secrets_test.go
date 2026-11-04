package api

import (
	"bytes"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/runs"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/secrets"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

func testCipher(t *testing.T) *secrets.Cipher {
	t.Helper()
	c, err := secrets.NewCipher(bytes.Repeat([]byte{3}, secrets.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func withSecrets(c *secrets.Cipher) option {
	return func(_ *harness, _ *runs.Config, d *Deps) { d.Secrets = c }
}

// secretsHarness has shop registered from GitHub with the instance
// shop--dev, moimroom only in the apps file, and a secrets key.
func secretsHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t, withApps(""), withGitHub(instanceGitHub(), nil), withSecrets(testCipher(t)))
	h.registerShop()
	h.addInstance(map[string]any{"branch": "dev"})
	return h
}

func (h *harness) putSecrets(body any) apiv1.AppSecrets {
	h.t.Helper()
	code, b := h.do("PUT", "/v1/apps/shop/secrets", opToken, body)
	if code != 200 {
		h.t.Fatalf("put %v: %d %s", body, code, b)
	}
	return decode[apiv1.AppSecrets](h.t, b)
}

func secretList(s apiv1.AppSecrets) string {
	var parts []string
	for _, a := range s.App {
		parts = append(parts, a.Name)
	}
	for _, in := range s.Instances {
		for _, a := range in.Secrets {
			parts = append(parts, in.Instance+"/"+a.Name)
		}
	}
	return strings.Join(parts, ",")
}

// lastAudit is the newest audit entry.
func (h *harness) lastAudit() store.Audit {
	h.t.Helper()
	list, err := h.store.ListAudit(h.t.Context(), 0, 1)
	if err != nil || len(list) != 1 {
		h.t.Fatalf("audit %v, %v", list, err)
	}
	return list[0]
}

func TestAppSecrets(t *testing.T) {
	h := secretsHarness(t)
	ctx := t.Context()

	code, b := h.do("GET", "/v1/apps/shop/secrets", viewerToken, nil)
	if code != 200 || string(bytes.TrimSpace(b)) != `{"app":[],"instances":[]}` {
		t.Fatalf("empty list: %d %s", code, b)
	}

	got := h.putSecrets(map[string]any{"set": map[string]string{"HOST_CODE": "4821", "ADMIN_TOKEN": "t0p-s3cret"}})
	if secretList(got) != "ADMIN_TOKEN,HOST_CODE" || got.App[0].UpdatedBy != "op" || got.App[0].UpdatedAt.IsZero() {
		t.Fatalf("after set: %s", mustJSON(t, got))
	}
	// Stored encrypted; the audit entry has the names only.
	rows, err := h.store.ListAppSecrets(ctx, "shop")
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows %v, %v", rows, err)
	}
	for _, r := range rows {
		if bytes.Contains(r.Ciphertext, []byte("4821")) || bytes.Contains(r.Ciphertext, []byte("t0p-s3cret")) || r.KeyVersion != secrets.KeyVersion {
			t.Fatalf("row %s is not encrypted", r.Name)
		}
	}
	a := h.lastAudit()
	if a.Action != "PUT /v1/apps/{name}/secrets" || a.Actor != "op" || a.Status != 200 ||
		!strings.Contains(string(a.Detail), `"set":["ADMIN_TOKEN","HOST_CODE"]`) || strings.Contains(string(a.Detail), "4821") || strings.Contains(string(a.Detail), "t0p") {
		t.Fatalf("audit %+v %s", a, a.Detail)
	}

	// Overrides of the default instance and of an added one.
	h.putSecrets(map[string]any{"instance": "shop", "set": map[string]string{"HOST_CODE": "1111"}})
	got = h.putSecrets(map[string]any{"instance": "shop--dev", "set": map[string]string{"DEV_ONLY": "dev", "HOST_CODE": "2222"}})
	if want := "ADMIN_TOKEN,HOST_CODE,shop/HOST_CODE,shop--dev/DEV_ONLY,shop--dev/HOST_CODE"; secretList(got) != want {
		t.Fatalf("after overrides: %s, want %s", secretList(got), want)
	}
	if a := h.lastAudit(); !strings.Contains(string(a.Detail), `"instance":"shop--dev"`) {
		t.Fatalf("audit %s", a.Detail)
	}
	// Viewers see the names too.
	code, b = h.do("GET", "/v1/apps/shop/secrets", viewerToken, nil)
	if code != 200 || secretList(decode[apiv1.AppSecrets](t, b)) != secretList(got) {
		t.Fatalf("viewer list: %d %s", code, b)
	}

	// Reading a value: operator only, never cached, always audited.
	before := h.lastAudit().ID
	reveal := func(token, path string) (int, []byte, http.Header) {
		t.Helper()
		req, _ := http.NewRequest("GET", h.srv.URL+path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(resp.Body)
		h.validate(req, resp, buf.Bytes())
		return resp.StatusCode, buf.Bytes(), resp.Header
	}
	code, b, hdr := reveal(opToken, "/v1/apps/shop/secrets/HOST_CODE")
	v := decode[apiv1.AppSecretValue](t, b)
	if code != 200 || v.Name != "HOST_CODE" || v.Value != "4821" || v.Instance != nil || v.UpdatedBy != "op" || hdr.Get("Cache-Control") != "no-store" {
		t.Fatalf("reveal: %d %s %v", code, b, hdr)
	}
	a = h.lastAudit()
	if a.ID == before || a.Action != revealSecretRoute || a.Target != "/v1/apps/shop/secrets/HOST_CODE" || a.Status != 200 ||
		!strings.Contains(string(a.Detail), `"secret":"HOST_CODE"`) || strings.Contains(string(a.Detail), "4821") {
		t.Fatalf("reveal audit %+v %s", a, a.Detail)
	}
	code, b, _ = reveal(opToken, "/v1/apps/shop/secrets/HOST_CODE?instance=shop--dev")
	v = decode[apiv1.AppSecretValue](t, b)
	if code != 200 || v.Value != "2222" || v.Instance == nil || *v.Instance != "shop--dev" {
		t.Fatalf("reveal override: %d %s", code, b)
	}
	if a := h.lastAudit(); !strings.Contains(string(a.Detail), `"instance":"shop--dev"`) || strings.Contains(string(a.Detail), "2222") {
		t.Fatalf("reveal override audit %s", a.Detail)
	}
	// Only the instance's own override, not the app-wide value.
	code, b, hdr = reveal(opToken, "/v1/apps/shop/secrets/ADMIN_TOKEN?instance=shop--dev")
	if code != 404 || errCode(t, b) != "secret_not_found" || hdr.Get("Cache-Control") != "no-store" {
		t.Fatalf("no override: %d %s", code, b)
	}
	if a := h.lastAudit(); a.Action != revealSecretRoute || a.Status != 404 {
		t.Fatalf("failed reveal not audited: %+v", a)
	}
	for _, tc := range []struct {
		token, path string
		status      int
		code        string
	}{
		{viewerToken, "/v1/apps/shop/secrets/HOST_CODE", 403, "forbidden"},
		{agentToken, "/v1/apps/shop/secrets/HOST_CODE", 403, "forbidden"},
		{"", "/v1/apps/shop/secrets/HOST_CODE", 401, "unauthorized"},
		{opToken, "/v1/apps/shop/secrets/NOPE", 404, "secret_not_found"},
		{opToken, "/v1/apps/shop/secrets/HOST_CODE?instance=shop--qa", 404, "instance_not_found"},
		{opToken, "/v1/apps/nope/secrets/HOST_CODE", 404, "app_not_found"},
		{opToken, "/v1/apps/moimroom/secrets/HOST_CODE", 409, "app_not_registered"},
	} {
		code, b, _ := reveal(tc.token, tc.path)
		if code != tc.status || errCode(t, b) != tc.code {
			t.Errorf("GET %s as %q: %d %s, want %d %s", tc.path, tc.token, code, b, tc.status, tc.code)
		}
		if strings.Contains(string(b), "4821") {
			t.Errorf("GET %s as %q leaked the value", tc.path, tc.token)
		}
	}

	// Replacing, and deleting an override falls back to the app-wide value.
	got = h.putSecrets(map[string]any{"set": map[string]string{"HOST_CODE": "9999"}, "delete": []string{"ADMIN_TOKEN", "NEVER_SET"}})
	if want := "HOST_CODE,shop/HOST_CODE,shop--dev/DEV_ONLY,shop--dev/HOST_CODE"; secretList(got) != want {
		t.Fatalf("after replace and delete: %s", secretList(got))
	}
	got = h.putSecrets(map[string]any{"instance": "shop", "delete": []string{"HOST_CODE"}})
	if want := "HOST_CODE,shop--dev/DEV_ONLY,shop--dev/HOST_CODE"; secretList(got) != want {
		t.Fatalf("after deleting an override: %s", secretList(got))
	}
	if a := h.lastAudit(); !strings.Contains(string(a.Detail), `"deleted":["HOST_CODE"]`) || strings.Contains(string(a.Detail), `"set"`) {
		t.Fatalf("delete audit %s", a.Detail)
	}
	code, b, _ = reveal(opToken, "/v1/apps/shop/secrets/HOST_CODE")
	if code != 200 || decode[apiv1.AppSecretValue](t, b).Value != "9999" {
		t.Fatalf("replaced value: %d %s", code, b)
	}

	// The instance's overrides go with the instance.
	if err := h.store.DeleteAppBranch(ctx, "shop", "dev"); err != nil {
		t.Fatal(err)
	}
	code, b = h.do("GET", "/v1/apps/shop/secrets", viewerToken, nil)
	if code != 200 || secretList(decode[apiv1.AppSecrets](t, b)) != "HOST_CODE" {
		t.Fatalf("after deleting the instance: %d %s", code, b)
	}
}

func TestAppSecretsUpdateErrors(t *testing.T) {
	h := secretsHarness(t)
	h.putSecrets(map[string]any{"set": map[string]string{"KEEP": "x"}})

	many := map[string]string{}
	for i := range maxSecretsPerScope + 1 {
		many[fmt.Sprintf("S%02d", i)] = "v"
	}
	cases := []struct {
		name, app, token string
		body             any
		status           int
		code             string
	}{
		{name: "no token", app: "shop", body: map[string]any{"set": map[string]string{"A": "b"}}, status: 401, code: "unauthorized"},
		{name: "viewer", app: "shop", token: viewerToken, body: map[string]any{"set": map[string]string{"A": "b"}}, status: 403, code: "forbidden"},
		{name: "agent", app: "shop", token: agentToken, body: map[string]any{"set": map[string]string{"A": "b"}}, status: 403, code: "forbidden"},
		{name: "no body", app: "shop", token: opToken, status: 400, code: "invalid_request"},
		{name: "unknown field", app: "shop", token: opToken, body: map[string]any{"set": map[string]string{"A": "b"}, "env": "eks"}, status: 400, code: "invalid_request"},
		{name: "nothing", app: "shop", token: opToken, body: map[string]any{}, status: 400, code: "invalid_request"},
		{name: "empty lists", app: "shop", token: opToken, body: map[string]any{"set": map[string]string{}, "delete": []string{}}, status: 400, code: "invalid_request"},
		{name: "lowercase", app: "shop", token: opToken, body: map[string]any{"set": map[string]string{"host_code": "b"}}, status: 400, code: "invalid_request"},
		{name: "digit first", app: "shop", token: opToken, body: map[string]any{"set": map[string]string{"1A": "b"}}, status: 400, code: "invalid_request"},
		{name: "dash", app: "shop", token: opToken, body: map[string]any{"set": map[string]string{"A-B": "b"}}, status: 400, code: "invalid_request"},
		{name: "too long", app: "shop", token: opToken, body: map[string]any{"set": map[string]string{strings.Repeat("A", 65): "b"}}, status: 400, code: "invalid_request"},
		{name: "PORT", app: "shop", token: opToken, body: map[string]any{"set": map[string]string{"PORT": "1"}}, status: 400, code: "invalid_request"},
		{name: "DATABASE_URL", app: "shop", token: opToken, body: map[string]any{"set": map[string]string{"DATABASE_URL": "x"}}, status: 400, code: "invalid_request"},
		{name: "S3_", app: "shop", token: opToken, body: map[string]any{"set": map[string]string{"S3_BUCKET": "x"}}, status: 400, code: "invalid_request"},
		{name: "reserved delete", app: "shop", token: opToken, body: map[string]any{"delete": []string{"PORT"}}, status: 400, code: "invalid_request"},
		{name: "empty value", app: "shop", token: opToken, body: map[string]any{"set": map[string]string{"A": ""}}, status: 400, code: "invalid_request"},
		{name: "empty value among others", app: "shop", token: opToken, body: map[string]any{"set": map[string]string{"A": "x", "B": ""}}, status: 400, code: "invalid_request"},
		{name: "long value", app: "shop", token: opToken, body: map[string]any{"set": map[string]string{"A": strings.Repeat("가", 1366)}}, status: 400, code: "invalid_request"},
		{name: "set and delete", app: "shop", token: opToken, body: map[string]any{"set": map[string]string{"A": "b"}, "delete": []string{"A"}}, status: 400, code: "invalid_request"},
		{name: "deleted twice", app: "shop", token: opToken, body: map[string]any{"delete": []string{"A", "A"}}, status: 400, code: "invalid_request"},
		{name: "too many in one", app: "shop", token: opToken, body: map[string]any{"set": many}, status: 400, code: "invalid_request"},
		{name: "unknown instance", app: "shop", token: opToken, body: map[string]any{"instance": "shop--qa", "set": map[string]string{"A": "b"}}, status: 404, code: "instance_not_found"},
		{name: "another app's instance", app: "shop", token: opToken, body: map[string]any{"instance": "moimroom", "set": map[string]string{"A": "b"}}, status: 404, code: "instance_not_found"},
		{name: "unknown app", app: "nope", token: opToken, body: map[string]any{"set": map[string]string{"A": "b"}}, status: 404, code: "app_not_found"},
		{name: "file app", app: "moimroom", token: opToken, body: map[string]any{"set": map[string]string{"A": "b"}}, status: 409, code: "app_not_registered"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, b := h.do("PUT", "/v1/apps/"+tc.app+"/secrets", tc.token, tc.body)
			if code != tc.status || errCode(t, b) != tc.code {
				t.Fatalf("%d %s, want %d %s", code, b, tc.status, tc.code)
			}
		})
	}
	// Exactly at the value limits is fine: 1 byte, and 4096 bytes (not characters).
	h.putSecrets(map[string]any{"set": map[string]string{"LONG": strings.Repeat("a", maxSecretValue)}})
	h.putSecrets(map[string]any{"set": map[string]string{"LONG": "a"}})
	// Nothing changed by the refused requests.
	code, b := h.do("GET", "/v1/apps/shop/secrets", viewerToken, nil)
	if code != 200 || secretList(decode[apiv1.AppSecrets](t, b)) != "KEEP,LONG" {
		t.Fatalf("after the refused changes: %d %s", code, b)
	}

	// The limit counts what the scope holds after the change.
	fill := map[string]string{}
	for i := range maxSecretsPerScope - 2 {
		fill[fmt.Sprintf("F%02d", i)] = "v"
	}
	h.putSecrets(map[string]any{"set": fill})
	code, b = h.do("PUT", "/v1/apps/shop/secrets", opToken, map[string]any{"set": map[string]string{"ONE_MORE": "v"}})
	if code != 400 || errCode(t, b) != "invalid_request" || !strings.Contains(string(b), "at most 50") {
		t.Fatalf("over the limit: %d %s", code, b)
	}
	// Replacing one and deleting one fits, and the instances have their own limit.
	h.putSecrets(map[string]any{"set": map[string]string{"ONE_MORE": "v"}, "delete": []string{"KEEP"}})
	h.putSecrets(map[string]any{"instance": "shop--dev", "set": map[string]string{"ONE_MORE": "v"}})
}

func TestAppSecretsNotConfigured(t *testing.T) {
	h := newHarness(t, withApps(""), withGitHub(instanceGitHub(), nil))
	h.registerShop()
	for _, tc := range []struct {
		method, path string
		body         any
	}{
		{"GET", "/v1/apps/shop/secrets", nil},
		{"PUT", "/v1/apps/shop/secrets", map[string]any{"set": map[string]string{"A": "b"}}},
		{"GET", "/v1/apps/shop/secrets/A", nil},
	} {
		code, b := h.do(tc.method, tc.path, opToken, tc.body)
		if code != 503 || errCode(t, b) != "secrets_not_configured" {
			t.Errorf("%s %s: %d %s", tc.method, tc.path, code, b)
		}
	}
	// An unknown app is still not found.
	if code, b := h.do("GET", "/v1/apps/nope/secrets", opToken, nil); code != 404 {
		t.Errorf("unknown app: %d %s", code, b)
	}
}

func TestAppSecretsDeletedWithApp(t *testing.T) {
	h := secretsHarness(t)
	h.putSecrets(map[string]any{"set": map[string]string{"A": "1"}})
	h.putSecrets(map[string]any{"instance": "shop--dev", "set": map[string]string{"B": "2"}})
	if err := h.store.DeleteApp(t.Context(), "shop"); err != nil {
		t.Fatal(err)
	}
	h.registerShop()
	code, b := h.do("GET", "/v1/apps/shop/secrets", viewerToken, nil)
	if code != 200 || string(bytes.TrimSpace(b)) != `{"app":[],"instances":[]}` {
		t.Fatalf("a re-registered app: %d %s", code, b)
	}
}
