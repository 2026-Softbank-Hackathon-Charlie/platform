package api

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/auth"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

// Lifetimes and limits of administrator login (ADR 0018).
const (
	consoleSessionTTL = 12 * time.Hour
	cliTokenTTL       = 30 * 24 * time.Hour
	deviceLoginTTL    = 10 * time.Minute
	// After an approval the CLI has at least this long to exchange it.
	approvalExchangeWindow = 10 * time.Minute
	devicePollInterval     = 5 // seconds between polls of POST /v1/auth/device/token
	maxPendingDeviceLogins = 50
	deviceLoginRetryAfter  = 60 // seconds, once maxPendingDeviceLogins are pending

	loginMaxFailures  = 10
	loginWindow       = 10 * time.Minute
	loginMaxUsernames = 10000 // usernames the login limiter remembers

	maxUsernameChars = 64  // LoginRequest.username maxLength
	maxPasswordChars = 256 // LoginRequest.password maxLength
	maxClientChars   = 200 // DeviceLoginStartRequest.client maxLength
)

// --- console login ---

// POST /v1/auth/login. The route takes no token, so it is audited here:
// every attempt that reaches the limiter, as the administrator when the
// password was right and otherwise as the username tried (anonymous when it
// cannot be a name). The password is never logged or recorded.
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	p, status := s.handleLogin(w, r)
	if status == 0 {
		return
	}
	note, _ := r.Context().Value(noteKey{}).(map[string]any)
	s.audit(r, p, "POST /v1/auth/login", status, note)
}

// handleLogin answers a login and returns whom to audit it as and the status
// it sent; status 0 means the request was no attempt (no administrators, a
// malformed body).
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) (auth.Principal, int) {
	if s.cfg.Admins.Len() == 0 {
		s.fail(w, r, http.StatusServiceUnavailable, "login_not_configured", "this server has no administrators (PLATFORM_ADMINS)")
		return auth.Principal{}, 0
	}
	var req apiv1.LoginRequest
	if !s.decode(w, r, &req, true) {
		return auth.Principal{}, 0
	}
	if req.Username == "" || utf8.RuneCountInString(req.Username) > maxUsernameChars || utf8.RuneCountInString(req.Password) > maxPasswordChars {
		s.fail(w, r, http.StatusBadRequest, "invalid_request",
			fmt.Sprintf("username is required and at most %d characters; password is at most %d", maxUsernameChars, maxPasswordChars))
		return auth.Principal{}, 0
	}
	var tried auth.Principal // anonymous unless the username can be a name
	if auth.ValidName(req.Username) {
		tried.Name = req.Username
	}
	// The attempt counts before the password is checked, so parallel
	// guesses cannot pass the limit; a limited username gets no check.
	if wait, ok := s.logins.Attempt(req.Username, s.now()); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds(wait)))
		s.fail(w, r, http.StatusTooManyRequests, "too_many_attempts", "too many failed logins for this username; try again later")
		return tried, http.StatusTooManyRequests
	}
	p, ok := s.cfg.Admins.Check(req.Username, req.Password)
	if !ok {
		s.fail(w, r, http.StatusUnauthorized, "invalid_credentials", "unknown username or wrong password")
		return tried, http.StatusUnauthorized
	}
	s.logins.Reset(req.Username)

	token, sum := auth.NewSessionToken()
	now := s.now()
	sess := store.Session{TokenHash: sum, Name: p.Name, Role: string(p.Role), Kind: store.SessionConsole,
		CreatedAt: now, ExpiresAt: now.Add(consoleSessionTTL)}
	if err := s.d.Store.CreateSession(r.Context(), sess); err != nil {
		s.internal(w, r, err)
		return p, http.StatusInternalServerError
	}
	writeJSON(w, http.StatusOK, sessionJSON(token, sess.ExpiresAt, p))
	return p, http.StatusOK
}

// POST /v1/auth/logout. A static token is no session: there is nothing to
// end, and it keeps working.
func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if raw, ok := auth.BearerToken(r); ok && strings.HasPrefix(raw, auth.SessionPrefix) {
		// Not found: it expired or another logout ended it meanwhile.
		if err := s.d.Store.DeleteSession(r.Context(), auth.HashSecret(raw)); err != nil && !errors.Is(err, store.ErrNotFound) {
			s.internal(w, r, err)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- CLI device login ---

// POST /v1/auth/device. Not audited: nothing is decided until an
// administrator reviews it, and the review is audited.
func (s *Server) startDeviceLogin(w http.ResponseWriter, r *http.Request) {
	var req apiv1.DeviceLoginStartRequest
	if !s.decode(w, r, &req, true) {
		return
	}
	// The administrator reads client before approving: no control or
	// formatting characters (such as bidi overrides) that could change how
	// it looks.
	client := strings.TrimSpace(req.Client)
	if n := utf8.RuneCountInString(client); n == 0 || n > maxClientChars ||
		strings.ContainsFunc(client, func(c rune) bool { return unicode.IsControl(c) || unicode.Is(unicode.Cf, c) }) {
		s.fail(w, r, http.StatusBadRequest, "invalid_request",
			fmt.Sprintf("client is required: 1 to %d characters without control characters", maxClientChars))
		return
	}
	code, sum := auth.NewDeviceCode()
	now := s.now()
	d := store.DeviceLogin{DeviceCodeHash: sum, Client: client, Status: store.DeviceLoginPending,
		CreatedAt: now, ExpiresAt: now.Add(deviceLoginTTL)}
	for attempt := 1; ; attempt++ {
		d.UserCode = auth.NewUserCode()
		err := s.d.Store.CreateDeviceLogin(r.Context(), d, maxPendingDeviceLogins)
		switch {
		case err == nil:
			writeJSON(w, http.StatusOK, apiv1.DeviceLoginStart{
				DeviceCode:      code,
				UserCode:        auth.FormatUserCode(d.UserCode),
				VerificationUri: s.cfg.ConsoleURL + "/#device",
				ExpiresIn:       int(deviceLoginTTL / time.Second),
				Interval:        devicePollInterval,
			})
			return
		case errors.Is(err, store.ErrDeviceLoginLimit):
			w.Header().Set("Retry-After", strconv.Itoa(deviceLoginRetryAfter))
			s.fail(w, r, http.StatusTooManyRequests, "too_many_attempts",
				fmt.Sprintf("%d device logins are already pending; try again later", maxPendingDeviceLogins))
			return
		case errors.Is(err, store.ErrConflict) && attempt < 5:
			// The user code is taken by a live device login (20^8 codes,
			// at most a few dozen live): draw another.
		default:
			s.internal(w, r, err)
			return
		}
	}
}

// POST /v1/auth/device/token. The route takes no token, so the exchange
// that hands out a CLI token is audited here, as the administrator who
// approved it with the role they picked. Polls that get no token are not
// audited.
func (s *Server) pollDeviceLogin(w http.ResponseWriter, r *http.Request) {
	var req apiv1.DeviceTokenRequest
	if !s.decode(w, r, &req, true) {
		return
	}
	if req.DeviceCode == "" {
		s.fail(w, r, http.StatusBadRequest, "invalid_request", "deviceCode is required")
		return
	}
	token, sum := auth.NewSessionToken()
	now := s.now()
	d, sess, err := s.d.Store.ExchangeDeviceLogin(r.Context(), auth.HashSecret(req.DeviceCode),
		store.Session{TokenHash: sum, Kind: store.SessionCLI, CreatedAt: now, ExpiresAt: now.Add(cliTokenTTL)}, now)
	switch {
	case errors.Is(err, store.ErrNotFound):
		s.fail(w, r, http.StatusBadRequest, "expired_token", "the device login is unknown, expired or already used; start a new one")
		return
	case err != nil:
		s.internal(w, r, err)
		return
	}
	switch d.Status {
	case store.DeviceLoginPending:
		s.fail(w, r, http.StatusBadRequest, "authorization_pending", fmt.Sprintf("not approved yet; ask again in %d seconds", devicePollInterval))
	case store.DeviceLoginDenied:
		s.fail(w, r, http.StatusBadRequest, "access_denied", "an administrator denied this device login")
	default:
		p := auth.Principal{Name: sess.Name, Role: apiv1.Role(sess.Role)}
		writeJSON(w, http.StatusOK, sessionJSON(token, sess.ExpiresAt, p))
		noteAudit(r, "client", d.Client)
		noteAudit(r, "userCode", auth.FormatUserCode(d.UserCode))
		note, _ := r.Context().Value(noteKey{}).(map[string]any)
		s.audit(r, p, "POST /v1/auth/device/token", http.StatusOK, note)
	}
}

// GET /v1/auth/device/{userCode}
func (s *Server) getDeviceLogin(w http.ResponseWriter, r *http.Request) {
	if !s.consoleSession(w, r) {
		return
	}
	code, ok := s.userCode(w, r)
	if !ok {
		return
	}
	d, err := s.d.Store.GetDeviceLogin(r.Context(), code, s.now())
	switch {
	case errors.Is(err, store.ErrNotFound):
		s.deviceLoginNotFound(w, r)
	case err != nil:
		s.internal(w, r, err)
	default:
		writeJSON(w, http.StatusOK, deviceLoginJSON(d))
	}
}

// POST /v1/auth/device/{userCode}/approve. The CLI token gets the caller's
// name and the role in the body, which the approver picks for what the
// client needs. Approving gives the CLI at least approvalExchangeWindow from
// now to exchange it, so one that polls late (a bot re-running its login in
// a new run) still gets the token.
func (s *Server) approveDeviceLogin(w http.ResponseWriter, r *http.Request) {
	if !s.consoleSession(w, r) {
		return
	}
	var req apiv1.DeviceLoginApproveRequest
	if !s.decode(w, r, &req, true) {
		return
	}
	if !req.Role.Valid() {
		s.fail(w, r, http.StatusBadRequest, "invalid_request", "role must be viewer, agent or operator")
		return
	}
	noteAudit(r, "tokenRole", req.Role)
	now := s.now()
	s.reviewDeviceLogin(w, r, store.DeviceLoginReview{Status: store.DeviceLoginApproved, TokenRole: string(req.Role),
		At: now, KeepUntil: now.Add(approvalExchangeWindow)})
}

// POST /v1/auth/device/{userCode}/deny
func (s *Server) denyDeviceLogin(w http.ResponseWriter, r *http.Request) {
	if !s.consoleSession(w, r) {
		return
	}
	s.reviewDeviceLogin(w, r, store.DeviceLoginReview{Status: store.DeviceLoginDenied, At: s.now()})
}

// consoleSession answers 403 console_session_required unless the caller
// logged in to the console with a password. Static tokens and CLI tokens
// may not review device logins, whatever their role: a bot holding an
// operator CLI token could otherwise approve its own next login and keep
// minting fresh tokens, and approval is meant for a person.
func (s *Server) consoleSession(w http.ResponseWriter, r *http.Request) bool {
	if p, _ := auth.FromContext(r.Context()); p.Kind != auth.KindConsole {
		s.fail(w, r, http.StatusForbidden, "console_session_required",
			"only a console session (an administrator's password login) may review device logins")
		return false
	}
	return true
}

// reviewDeviceLogin applies rev to a pending device login as the caller.
func (s *Server) reviewDeviceLogin(w http.ResponseWriter, r *http.Request, rev store.DeviceLoginReview) {
	code, ok := s.userCode(w, r)
	if !ok {
		return
	}
	noteAudit(r, "userCode", auth.FormatUserCode(code))
	p, _ := auth.FromContext(r.Context())
	rev.By = p.Name
	d, err := s.d.Store.ReviewDeviceLogin(r.Context(), code, rev)
	switch {
	case errors.Is(err, store.ErrNotFound):
		s.deviceLoginNotFound(w, r)
	case errors.Is(err, store.ErrConflict):
		s.fail(w, r, http.StatusConflict, "device_login_state_conflict", err.Error())
	case err != nil:
		s.internal(w, r, err)
	default:
		noteAudit(r, "client", d.Client)
		writeJSON(w, http.StatusOK, deviceLoginJSON(d))
	}
}

// userCode reads the {userCode} path parameter in its stored form. One that
// cannot be a user code is answered like an unknown one.
func (s *Server) userCode(w http.ResponseWriter, r *http.Request) (string, bool) {
	code, ok := auth.NormalizeUserCode(r.PathValue("userCode"))
	if !ok {
		s.deviceLoginNotFound(w, r)
	}
	return code, ok
}

func (s *Server) deviceLoginNotFound(w http.ResponseWriter, r *http.Request) {
	s.fail(w, r, http.StatusNotFound, "device_login_not_found", "no device login has this code, or it expired")
}

// deviceLoginJSON never carries the device code: the store does not have it.
func deviceLoginJSON(d store.DeviceLogin) apiv1.DeviceLogin {
	out := apiv1.DeviceLogin{
		UserCode:  auth.FormatUserCode(d.UserCode),
		Client:    d.Client,
		Status:    apiv1.DeviceLoginStatus(d.Status),
		CreatedAt: d.CreatedAt,
		ExpiresAt: d.ExpiresAt,
	}
	if d.ReviewedBy != "" {
		out.ReviewedBy = &d.ReviewedBy
	}
	if d.TokenRole != "" {
		role := apiv1.Role(d.TokenRole)
		out.Role = &role
	}
	return out
}

func sessionJSON(token string, expiresAt time.Time, p auth.Principal) apiv1.Session {
	return apiv1.Session{Token: token, ExpiresAt: expiresAt, Principal: apiv1.Principal{Name: p.Name, Role: p.Role}}
}

// retryAfterSeconds rounds d up to whole seconds, at least one, for a
// Retry-After header.
func retryAfterSeconds(d time.Duration) int {
	return max(1, int((d+time.Second-1)/time.Second))
}
