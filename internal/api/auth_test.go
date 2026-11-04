package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/auth"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/runs"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store/storetest"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

const adminPassword = "correct horse battery"

// fakeClock is the server's clock in these tests.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *fakeClock { return &fakeClock{t: time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func testAdmins(t *testing.T) *auth.Admins {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(adminPassword), bcrypt.MinCost) // fast in tests
	if err != nil {
		t.Fatal(err)
	}
	a, err := auth.ParseAdmins("seohyun:" + string(hash) + ",minji:" + string(hash))
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// withAdmins configures the administrators seohyun and minji, both with
// adminPassword, and runs the server on clock.
func withAdmins(clock *fakeClock) option {
	return func(h *harness, _ *runs.Config, _ *Deps) {
		h.admins, h.now = testAdmins(h.t), clock.Now
	}
}

func (h *harness) login(username, password string) (int, []byte) {
	h.t.Helper()
	return h.do("POST", "/v1/auth/login", "", apiv1.LoginRequest{Username: username, Password: password})
}

// session logs username in and returns the console session.
func (h *harness) session(username string) apiv1.Session {
	h.t.Helper()
	code, b := h.login(username, adminPassword)
	if code != 200 {
		h.t.Fatalf("login %s: %d %s", username, code, b)
	}
	return decode[apiv1.Session](h.t, b)
}

// public sends a request without a token, like do, and also returns the
// response headers.
func (h *harness) public(method, path string, body any, hdr ...string) (int, http.Header, []byte) {
	h.t.Helper()
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequest(method, h.srv.URL+path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	h.validate(req, resp, out)
	return resp.StatusCode, resp.Header, out
}

func (h *harness) me(token string) (int, apiv1.Principal) {
	h.t.Helper()
	code, b := h.do("GET", "/v1/me", token, nil)
	if code != 200 {
		return code, apiv1.Principal{}
	}
	return code, decode[apiv1.Principal](h.t, b)
}

// auditOf returns the audit entries of action, newest first.
func (h *harness) auditOf(action string) []store.Audit {
	h.t.Helper()
	all, err := h.store.ListAudit(h.t.Context(), 0, 0)
	if err != nil {
		h.t.Fatal(err)
	}
	var out []store.Audit
	for _, a := range all {
		if a.Action == action {
			out = append(out, a)
		}
	}
	return out
}

func TestLogin(t *testing.T) {
	clock := newClock()
	h := newHarness(t, withAdmins(clock))

	sess := h.session("seohyun")
	if !strings.HasPrefix(sess.Token, auth.SessionPrefix) || sess.Principal != (apiv1.Principal{Name: "seohyun", Role: apiv1.RoleOperator}) ||
		!sess.ExpiresAt.Equal(clock.Now().Add(12*time.Hour)) {
		t.Fatalf("session %+v", sess)
	}
	// The session works on a viewer route and on an operator route.
	if code, me := h.me(sess.Token); code != 200 || me.Name != "seohyun" || me.Role != apiv1.RoleOperator {
		t.Fatalf("me: %d %+v", code, me)
	}
	rec := h.submit(planJSON("p1", "a", "b", false))
	code, b := h.do("POST", "/v1/plans/"+rec.Ref+"/approve", sess.Token, map[string]string{"comment": "looks good"})
	if code != 200 || decode[apiv1.PlanRecord](t, b).ReviewedBy == nil || *decode[apiv1.PlanRecord](t, b).ReviewedBy != "seohyun" {
		t.Fatalf("approve with a session: %d %s", code, b)
	}

	// Names match in any case; the session has the configured name.
	if s := h.session("SeoHyun"); s.Principal.Name != "seohyun" {
		t.Fatalf("login as SeoHyun: %+v", s.Principal)
	}

	// A wrong password and an unknown username get the same answer.
	c1, b1 := h.login("seohyun", "wrong")
	c2, b2 := h.login("nobody", adminPassword)
	if c1 != 401 || c2 != 401 || errCode(t, b1) != "invalid_credentials" || !bytes.Equal(b1, b2) {
		t.Fatalf("wrong password: %d %s; unknown user: %d %s", c1, b1, c2, b2)
	}

	// Static tokens keep working next to administrators.
	if code, me := h.me(opToken); code != 200 || me.Name != "op" {
		t.Fatalf("static token: %d %+v", code, me)
	}

	for _, body := range []string{
		`{"username":"","password":"x"}`,
		`{"username":"seohyun","password":1}`,
		`{"username":"seohyun","password":"x","remember":true}`,
		`{"username":"` + strings.Repeat("a", 65) + `","password":"x"}`,
		`{"username":"seohyun","password":"` + strings.Repeat("x", 257) + `"}`,
	} {
		if code, b := h.do("POST", "/v1/auth/login", "", body); code != 400 || errCode(t, b) != "invalid_request" {
			t.Errorf("%.40s: %d %s", body, code, b)
		}
	}
}

func TestLoginNotConfigured(t *testing.T) {
	h := newHarness(t)
	if code, b := h.login("seohyun", adminPassword); code != 503 || errCode(t, b) != "login_not_configured" {
		t.Fatalf("login without administrators: %d %s", code, b)
	}
	if n := len(h.auditOf("POST /v1/auth/login")); n != 0 {
		t.Fatalf("%d audit entries for a server without administrators", n)
	}
}

func TestLoginAudit(t *testing.T) {
	h := newHarness(t, withAdmins(newClock()))
	h.session("seohyun")
	h.login("seohyun", "hunter2-wrong")
	h.login("bad name!", "hunter3-wrong")
	h.login(strings.Repeat("a", 64), "hunter4-wrong")
	h.do("POST", "/v1/auth/login", "", `{"username":"seohyun","password":"hunter5","x":1}`) // malformed: not audited

	entries := h.auditOf("POST /v1/auth/login")
	want := []struct {
		actor, role string
		status      int
	}{
		{strings.Repeat("a", 64), "", 401},
		{"anonymous", "", 401},
		{"seohyun", "", 401},
		{"seohyun", "operator", 200},
	}
	if len(entries) != len(want) {
		t.Fatalf("%d login audit entries, want %d: %+v", len(entries), len(want), entries)
	}
	for i, w := range want {
		e := entries[i]
		if e.Actor != w.actor || e.Role != w.role || e.Status != w.status || e.Target != "/v1/auth/login" {
			t.Errorf("entry %d = %s/%s %d %s, want %s/%s %d", i, e.Actor, e.Role, e.Status, e.Target, w.actor, w.role, w.status)
		}
	}
	all, _ := h.store.ListAudit(t.Context(), 0, 0)
	for _, e := range all {
		line, _ := json.Marshal(e)
		if strings.Contains(string(line), "hunter") || strings.Contains(string(line), adminPassword) {
			t.Errorf("audit entry has a password: %s", line)
		}
	}
}

func TestLoginLimiter(t *testing.T) {
	clock := newClock()
	h := newHarness(t, withAdmins(clock))
	for i := range 10 {
		if code, b := h.login("seohyun", "wrong"); code != 401 {
			t.Fatalf("failure %d: %d %s", i+1, code, b)
		}
	}
	clock.Add(4 * time.Minute)
	// Limited: even the right password is not checked. The oldest failure
	// leaves the window in 6 minutes. Any case is the same username.
	for _, name := range []string{"seohyun", "SEOHYUN"} {
		code, hdr, b := h.public("POST", "/v1/auth/login", apiv1.LoginRequest{Username: name, Password: adminPassword}, "Origin", "http://console.test")
		if code != 429 || errCode(t, b) != "too_many_attempts" || hdr.Get("Retry-After") != "360" {
			t.Fatalf("%s while limited: %d Retry-After %q %s", name, code, hdr.Get("Retry-After"), b)
		}
		// The console can read Retry-After.
		if !strings.Contains(hdr.Get("Access-Control-Expose-Headers"), "Retry-After") {
			t.Fatalf("Retry-After not exposed to the browser: %q", hdr.Get("Access-Control-Expose-Headers"))
		}
	}
	if a := h.auditOf("POST /v1/auth/login"); a[0].Status != 429 || a[0].Actor != "SEOHYUN" {
		t.Fatalf("limited attempt audited as %+v", a[0])
	}
	// Another administrator is not limited.
	h.session("minji")

	clock.Add(6 * time.Minute)
	h.session("seohyun")

	// A success clears the failures before it.
	for range 9 {
		h.login("seohyun", "wrong")
	}
	h.session("seohyun")
	for range 9 {
		h.login("seohyun", "wrong")
	}
	h.session("seohyun")
}

func TestSessionExpiryAndLogout(t *testing.T) {
	clock := newClock()
	h := newHarness(t, withAdmins(clock))

	s1 := h.session("seohyun")
	clock.Add(12*time.Hour - time.Second)
	if code, _ := h.me(s1.Token); code != 200 {
		t.Fatalf("session before expiry: %d", code)
	}
	clock.Add(time.Second)
	if code, _ := h.me(s1.Token); code != 401 {
		t.Fatalf("expired session: %d", code)
	}

	s2 := h.session("seohyun")
	if code, b := h.do("POST", "/v1/auth/logout", s2.Token, nil); code != 204 {
		t.Fatalf("logout: %d %s", code, b)
	}
	if code, _ := h.me(s2.Token); code != 401 {
		t.Fatalf("session after logout: %d", code)
	}
	if code, _ := h.do("POST", "/v1/auth/logout", s2.Token, nil); code != 401 {
		t.Fatalf("second logout: %d", code)
	}
	if a := h.auditOf("POST /v1/auth/logout"); len(a) != 2 || a[1].Actor != "seohyun" || a[1].Status != 204 || a[0].Status != 401 {
		t.Fatalf("logout audit %+v", a)
	}

	// A static token cannot be ended; logout is a no-op for it.
	if code, _ := h.do("POST", "/v1/auth/logout", opToken, nil); code != 204 {
		t.Fatalf("logout with a static token: %d", code)
	}
	if code, _ := h.me(opToken); code != 200 {
		t.Fatalf("static token after logout: %d", code)
	}
	if code, _ := h.me(auth.SessionPrefix + "unknown"); code != 401 {
		t.Fatalf("unknown session token: %d", code)
	}
}

var userCodeShape = regexp.MustCompile(`^[BCDFGHJKLMNPQRSTVWXZ]{4}-[BCDFGHJKLMNPQRSTVWXZ]{4}$`)

// startDevice starts a device login for client.
func (h *harness) startDevice(client string) apiv1.DeviceLoginStart {
	h.t.Helper()
	code, b := h.do("POST", "/v1/auth/device", "", apiv1.DeviceLoginStartRequest{Client: client})
	if code != 200 {
		h.t.Fatalf("start device login: %d %s", code, b)
	}
	return decode[apiv1.DeviceLoginStart](h.t, b)
}

func (h *harness) poll(deviceCode string) (int, []byte) {
	h.t.Helper()
	return h.do("POST", "/v1/auth/device/token", "", apiv1.DeviceTokenRequest{DeviceCode: deviceCode})
}

func TestDeviceLogin(t *testing.T) {
	clock := newClock()
	h := newHarness(t, withAdmins(clock))

	st := h.startDevice("  charlie v0.5.0 on rin-sbx-12 ")
	if !strings.HasPrefix(st.DeviceCode, auth.DeviceCodePrefix) || !userCodeShape.MatchString(st.UserCode) ||
		st.VerificationUri != "https://console.hopcharlie.dev/#device" || st.ExpiresIn != 600 || st.Interval != 5 {
		t.Fatalf("start %+v", st)
	}
	if code, b := h.poll(st.DeviceCode); code != 400 || errCode(t, b) != "authorization_pending" {
		t.Fatalf("poll before approval: %d %s", code, b)
	}

	admin := h.session("seohyun")
	lower := strings.ToLower(st.UserCode)
	for _, path := range []string{"/v1/auth/device/" + lower, "/v1/auth/device/" + url.PathEscape(strings.ReplaceAll(lower, "-", " "))} {
		code, b := h.do("GET", path, admin.Token, nil)
		d := decode[apiv1.DeviceLogin](t, b)
		if code != 200 || d.UserCode != st.UserCode || d.Client != "charlie v0.5.0 on rin-sbx-12" || d.Status != apiv1.DeviceLoginStatusPending ||
			d.ReviewedBy != nil || !d.ExpiresAt.Equal(clock.Now().Add(10*time.Minute)) {
			t.Fatalf("GET %s: %d %s", path, code, b)
		}
		if strings.Contains(string(b), st.DeviceCode) {
			t.Fatalf("GET shows the device code: %s", b)
		}
	}

	// Only an operator reviews.
	for _, tok := range []string{agentToken, viewerToken} {
		if code, _ := h.do("POST", "/v1/auth/device/"+lower+"/approve", tok, nil); code != 403 {
			t.Fatalf("approve as a non-operator: %d", code)
		}
	}
	if code, _ := h.do("GET", "/v1/auth/device/"+lower, viewerToken, nil); code != 403 {
		t.Fatalf("GET as viewer: %d", code)
	}

	clock.Add(time.Minute)
	code, b := h.do("POST", "/v1/auth/device/"+lower+"/approve", admin.Token, nil)
	if d := decode[apiv1.DeviceLogin](t, b); code != 200 || d.Status != apiv1.DeviceLoginStatusApproved || d.ReviewedBy == nil || *d.ReviewedBy != "seohyun" {
		t.Fatalf("approve: %d %s", code, b)
	}
	for _, action := range []string{"approve", "deny"} {
		if code, b := h.do("POST", "/v1/auth/device/"+st.UserCode+"/"+action, admin.Token, nil); code != 409 || errCode(t, b) != "device_login_state_conflict" {
			t.Fatalf("%s after approval: %d %s", action, code, b)
		}
	}

	// The next poll gets the token, once.
	code, b = h.poll(st.DeviceCode)
	cli := decode[apiv1.Session](t, b)
	if code != 200 || !strings.HasPrefix(cli.Token, auth.SessionPrefix) || cli.Principal != (apiv1.Principal{Name: "seohyun", Role: apiv1.RoleOperator}) ||
		!cli.ExpiresAt.Equal(clock.Now().Add(30*24*time.Hour)) {
		t.Fatalf("poll after approval: %d %s", code, b)
	}
	if code, b := h.poll(st.DeviceCode); code != 400 || errCode(t, b) != "expired_token" {
		t.Fatalf("second poll: %d %s", code, b)
	}
	if code, me := h.me(cli.Token); code != 200 || me != (apiv1.Principal{Name: "seohyun", Role: apiv1.RoleOperator}) {
		t.Fatalf("me with the CLI token: %d %+v", code, me)
	}
	if code, _ := h.do("GET", "/v1/auth/device/"+lower, admin.Token, nil); code != 404 {
		t.Fatalf("GET after the exchange: %d", code)
	}
	// The CLI token outlives the console session.
	clock.Add(13 * time.Hour)
	if code, _ := h.me(admin.Token); code != 401 {
		t.Fatalf("console session after 13h: %d", code)
	}
	if code, _ := h.me(cli.Token); code != 200 {
		t.Fatalf("CLI token after 13h: %d", code)
	}

	// The review and the exchange are audited as the approver, without secrets.
	approve := h.auditOf("POST /v1/auth/device/{userCode}/approve")
	exchange := h.auditOf("POST /v1/auth/device/token")
	if len(exchange) != 1 || exchange[0].Actor != "seohyun" || exchange[0].Role != "operator" || exchange[0].Status != 200 ||
		!strings.Contains(string(exchange[0].Detail), `"client":"charlie v0.5.0 on rin-sbx-12"`) ||
		!strings.Contains(string(exchange[0].Detail), `"userCode":"`+st.UserCode+`"`) {
		t.Fatalf("exchange audit %+v %s", exchange, exchange[0].Detail)
	}
	var ok bool
	for _, a := range approve {
		if a.Actor == "seohyun" && a.Status == 200 && strings.Contains(string(a.Detail), st.UserCode) && strings.Contains(string(a.Detail), "rin-sbx-12") {
			ok = true
		}
	}
	if !ok {
		t.Fatalf("approve audit %+v", approve)
	}
	all, _ := h.store.ListAudit(t.Context(), 0, 0)
	for _, e := range all {
		line, _ := json.Marshal(e)
		if strings.Contains(string(line), st.DeviceCode) || strings.Contains(string(line), cli.Token) || strings.Contains(string(line), admin.Token) {
			t.Fatalf("audit entry has a secret: %s", line)
		}
	}
}

func TestDeviceLoginDeny(t *testing.T) {
	h := newHarness(t, withAdmins(newClock()))
	st := h.startDevice("charlie on laptop")
	// A static operator token reviews too.
	code, b := h.do("POST", "/v1/auth/device/"+st.UserCode+"/deny", opToken, nil)
	if d := decode[apiv1.DeviceLogin](t, b); code != 200 || d.Status != apiv1.DeviceLoginStatusDenied || *d.ReviewedBy != "op" {
		t.Fatalf("deny: %d %s", code, b)
	}
	if code, b := h.poll(st.DeviceCode); code != 400 || errCode(t, b) != "access_denied" {
		t.Fatalf("poll after deny: %d %s", code, b)
	}
	if code, b := h.poll(st.DeviceCode); code != 400 || errCode(t, b) != "expired_token" {
		t.Fatalf("poll after access_denied: %d %s", code, b)
	}
}

func TestDeviceLoginExpiry(t *testing.T) {
	clock := newClock()
	h := newHarness(t, withAdmins(clock))
	st := h.startDevice("charlie on laptop")
	clock.Add(10 * time.Minute)
	for _, req := range []struct{ method, path string }{
		{"GET", "/v1/auth/device/" + st.UserCode},
		{"POST", "/v1/auth/device/" + st.UserCode + "/approve"},
		{"GET", "/v1/auth/device/ZZZZ-ZZZZ"},
		{"GET", "/v1/auth/device/not-a-code"},
	} {
		if code, b := h.do(req.method, req.path, opToken, nil); code != 404 || errCode(t, b) != "device_login_not_found" {
			t.Errorf("%s %s: %d %s", req.method, req.path, code, b)
		}
	}
	for _, code := range []string{st.DeviceCode, "hcd_unknown"} {
		if c, b := h.poll(code); c != 400 || errCode(t, b) != "expired_token" {
			t.Errorf("poll %.8s: %d %s", code, c, b)
		}
	}
	if code, b := h.poll(""); code != 400 || errCode(t, b) != "invalid_request" {
		t.Errorf("poll without a device code: %d %s", code, b)
	}
}

func TestDeviceLoginLimits(t *testing.T) {
	clock := newClock()
	h := newHarness(t, withAdmins(clock))
	for range 50 {
		h.startDevice("charlie")
	}
	code, hdr, b := h.public("POST", "/v1/auth/device", apiv1.DeviceLoginStartRequest{Client: "charlie"})
	if code != 429 || errCode(t, b) != "too_many_attempts" || hdr.Get("Retry-After") != "60" {
		t.Fatalf("51st pending device login: %d Retry-After %q %s", code, hdr.Get("Retry-After"), b)
	}
	clock.Add(10 * time.Minute) // all 50 expire
	h.startDevice(strings.Repeat("c", 200))

	for _, client := range []string{"", "   ", strings.Repeat("c", 201), "a\x00b", "charlie\non laptop", "charlie ‮evil"} {
		if code, b := h.do("POST", "/v1/auth/device", "", apiv1.DeviceLoginStartRequest{Client: client}); code != 400 || errCode(t, b) != "invalid_request" {
			t.Errorf("client %q: %d %s", client, code, b)
		}
	}
}

// The "everyone is dev" mode is only for a server with neither tokens nor
// administrators.
func TestInsecureOnlyWithoutAdmins(t *testing.T) {
	for _, c := range []struct {
		admins *auth.Admins
		want   int
	}{{nil, 200}, {testAdmins(t), 401}} {
		srv := httptest.NewServer(New(Config{Insecure: true, Admins: c.admins}, Deps{Store: storetest.NewMemory()}))
		resp, err := http.Get(srv.URL + "/v1/me")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		srv.Close()
		if resp.StatusCode != c.want {
			t.Errorf("insecure with %d admins: GET /v1/me without a token = %d, want %d", c.admins.Len(), resp.StatusCode, c.want)
		}
	}
}
