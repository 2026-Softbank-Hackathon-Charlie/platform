package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/auth"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/appspec"
)

// Limits of app secrets (ADR 0019).
const (
	maxSecretName      = 64
	maxSecretValue     = 4096 // bytes; a value is at least 1 byte
	maxSecretsPerScope = 50   // app-wide, and per instance
)

// revealSecretRoute returns a secret's value. Its handler writes the audit
// entry itself, before the value; the route only records denials.
const revealSecretRoute = "GET /v1/apps/{name}/secrets/{secret}"

var (
	secretNamePattern = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)
	// appNamePattern is appspec's app name rule, for audit targets.
	appNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,30}[a-z0-9]$`)
)

// checkSecretName returns why name cannot be a secret's name, or "".
func checkSecretName(name string) string {
	switch {
	case len(name) > maxSecretName:
		return fmt.Sprintf("secret name %.20q... is longer than %d characters", name, maxSecretName)
	case !secretNamePattern.MatchString(name):
		return fmt.Sprintf("secret name %q must match ^[A-Z_][A-Z0-9_]*$", name)
	case appspec.Reserved(name):
		return fmt.Sprintf("secret name %s is set by the platform (PORT, DATABASE_URL and S3_* are reserved)", name)
	}
	return ""
}

// auditTarget is the request path the audit log records. On the secrets
// routes an app or secret name that breaks the naming rules is recorded as
// "{invalid}": an operator may have pasted a value where a name goes.
func auditTarget(r *http.Request, pattern string) string {
	if !strings.HasPrefix(pattern, "GET /v1/apps/{name}/secrets") && !strings.HasPrefix(pattern, "PUT /v1/apps/{name}/secrets") {
		return r.URL.Path
	}
	clean := func(v string, ok bool) string {
		if ok {
			return v
		}
		return "{invalid}"
	}
	app := r.PathValue("name")
	target := "/v1/apps/" + clean(app, appNamePattern.MatchString(app)) + "/secrets"
	if pattern == revealSecretRoute {
		secret := r.PathValue("secret")
		target += "/" + clean(secret, checkSecretName(secret) == "")
	}
	return target
}

// secretsReady answers 503 secrets_not_configured when the server has no
// secrets key, and reports whether it has one.
func (s *Server) secretsReady(w http.ResponseWriter, r *http.Request) bool {
	if s.d.Secrets != nil {
		return true
	}
	s.fail(w, r, http.StatusServiceUnavailable, "secrets_not_configured", "this server has no secrets key (PLATFORM_SECRETS_KEY), so it cannot store or read app secrets")
	return false
}

// isInstance reports whether instance is one of app's instances: the
// default one, named after the app, or an added branch's.
func (s *Server) isInstance(ctx context.Context, app store.App, instance string) (bool, error) {
	if instance == app.Name {
		return true, nil
	}
	branches, err := s.d.Store.ListAppBranches(ctx, app.Name)
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(branches, func(b store.AppBranch) bool { return b.Instance == instance }), nil
}

// GET /v1/apps/{name}/secrets: the names, never the values.
func (s *Server) listAppSecrets(w http.ResponseWriter, r *http.Request) {
	app, ok := s.registeredApp(w, r, r.PathValue("name"))
	if !ok || !s.secretsReady(w, r) {
		return
	}
	rows, err := s.d.Store.ListAppSecrets(r.Context(), app.Name)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, appSecretsJSON(rows))
}

// appSecretsJSON is the API form of an app's secrets as ListAppSecrets
// returns them: by instance ("" first), then by name.
func appSecretsJSON(rows []store.AppSecret) apiv1.AppSecrets {
	out := apiv1.AppSecrets{App: []apiv1.AppSecretInfo{}, Instances: []apiv1.InstanceSecrets{}}
	for _, r := range rows {
		info := apiv1.AppSecretInfo{Name: r.Name, UpdatedAt: r.UpdatedAt, UpdatedBy: r.UpdatedBy}
		if r.Instance == "" {
			out.App = append(out.App, info)
			continue
		}
		if n := len(out.Instances); n == 0 || out.Instances[n-1].Instance != r.Instance {
			out.Instances = append(out.Instances, apiv1.InstanceSecrets{Instance: r.Instance, Secrets: []apiv1.AppSecretInfo{}})
		}
		last := &out.Instances[len(out.Instances)-1]
		last.Secrets = append(last.Secrets, info)
	}
	return out
}

// PUT /v1/apps/{name}/secrets: set and delete the secrets of one scope.
// The audit entry has the names that passed validation, never the values;
// a refused request records only how many names it had, since an operator
// may have pasted a value into a name.
func (s *Server) updateAppSecrets(w http.ResponseWriter, r *http.Request) {
	var req apiv1.AppSecretsUpdate
	if !s.decode(w, r, &req, true) {
		return
	}
	instance := ""
	if req.Instance != nil {
		instance = *req.Instance
	}
	var set map[string]string
	if req.Set != nil {
		set = *req.Set
	}
	var del []string
	if req.Delete != nil {
		del = *req.Delete
	}
	app, ok := s.registeredApp(w, r, r.PathValue("name"))
	if !ok {
		return
	}
	noteAudit(r, "app", app.Name)
	if !s.secretsReady(w, r) {
		return
	}

	names := make([]string, 0, len(set))
	for n := range set {
		names = append(names, n)
	}
	slices.Sort(names)
	bad := func(msg string) {
		noteAudit(r, "setCount", len(names))
		noteAudit(r, "deleteCount", len(del))
		s.fail(w, r, http.StatusBadRequest, "invalid_request", msg)
	}
	if len(names) == 0 && len(del) == 0 {
		bad("nothing to change: set or delete at least one secret")
		return
	}
	// A scope never holds more, so longer lists are mistakes.
	if len(names) > maxSecretsPerScope || len(del) > maxSecretsPerScope {
		bad(fmt.Sprintf("too many secrets: at most %d app-wide and %d per instance, so at most %d names in set and in delete", maxSecretsPerScope, maxSecretsPerScope, maxSecretsPerScope))
		return
	}
	for _, n := range names {
		if msg := checkSecretName(n); msg != "" {
			bad(msg)
			return
		}
		switch v := set[n]; {
		case v == "":
			bad(fmt.Sprintf("the value of %s is empty; delete the secret instead", n))
			return
		case len(v) > maxSecretValue:
			bad(fmt.Sprintf("the value of %s is longer than %d bytes", n, maxSecretValue))
			return
		}
	}
	for i, n := range del {
		if msg := checkSecretName(n); msg != "" {
			bad(msg)
			return
		}
		if slices.Contains(del[:i], n) {
			bad(fmt.Sprintf("delete lists %s twice", n))
			return
		}
		if _, ok := set[n]; ok {
			bad(fmt.Sprintf("%s is both set and deleted", n))
			return
		}
	}
	if len(names) > 0 {
		noteAudit(r, "set", names)
	}
	if len(del) > 0 {
		noteAudit(r, "deleted", del)
	}
	ctx := r.Context()
	if instance != "" {
		ok, err := s.isInstance(ctx, app, instance)
		if err != nil {
			s.internal(w, r, err)
			return
		}
		if !ok {
			s.fail(w, r, http.StatusNotFound, "instance_not_found", fmt.Sprintf("app %s has no instance %q", app.Name, instance))
			return
		}
		noteAudit(r, "instance", instance)
	}

	now := storeTime()
	rows := make([]store.AppSecret, 0, len(names))
	for _, n := range names {
		row, err := s.d.Secrets.Row(app.Name, instance, n, set[n], actor(r))
		if err != nil {
			s.internal(w, r, err)
			return
		}
		row.UpdatedAt = now
		rows = append(rows, row)
	}
	// One write: finish it even if the caller goes away.
	ctx = context.WithoutCancel(ctx)
	err := s.d.Store.ChangeAppSecrets(ctx, app.Name, instance, rows, del, maxSecretsPerScope)
	switch {
	case err == nil:
	case errors.Is(err, store.ErrSecretLimit):
		s.fail(w, r, http.StatusBadRequest, "invalid_request", fmt.Sprintf("too many secrets: at most %d app-wide and %d per instance; delete some first", maxSecretsPerScope, maxSecretsPerScope))
		return
	case errors.Is(err, store.ErrInstanceNotFound): // removed meanwhile
		s.fail(w, r, http.StatusNotFound, "instance_not_found", fmt.Sprintf("app %s has no instance %s", app.Name, instance))
		return
	case errors.Is(err, store.ErrNotFound): // deleted meanwhile
		s.fail(w, r, http.StatusNotFound, "app_not_found", "app not found")
		return
	default:
		s.internal(w, r, err)
		return
	}
	all, err := s.d.Store.ListAppSecrets(ctx, app.Name)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, appSecretsJSON(all))
}

// GET /v1/apps/{name}/secrets/{secret}: one value, for the console's show
// button. The answer must not be cached, and every answer is audited with
// the names that passed validation, never the value. The audit entry of a
// value is written before the value: when it cannot be written, the caller
// gets 503 audit_unavailable instead.
func (s *Server) getAppSecret(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	p, _ := auth.FromContext(r.Context())
	note, _ := r.Context().Value(noteKey{}).(map[string]any)
	out, status := s.lookupSecret(w, r)
	if status != http.StatusOK {
		// The error is already answered; record it like any other route.
		_ = s.audit(r, p, revealSecretRoute, status, note)
		return
	}
	if err := s.audit(r, p, revealSecretRoute, http.StatusOK, note); err != nil {
		s.fail(w, r, http.StatusServiceUnavailable, "audit_unavailable", "the audit log could not be written, so the value is not returned; try again")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// lookupSecret finds and decrypts the secret getAppSecret asks for. On a
// failure it answers the error itself and returns its status.
func (s *Server) lookupSecret(w http.ResponseWriter, r *http.Request) (apiv1.AppSecretValue, int) {
	failed := func(status int, code, msg string) (apiv1.AppSecretValue, int) {
		s.fail(w, r, status, code, msg)
		return apiv1.AppSecretValue{}, status
	}
	internal := func(err error) (apiv1.AppSecretValue, int) {
		s.internal(w, r, err)
		return apiv1.AppSecretValue{}, http.StatusInternalServerError
	}
	secret, instance := r.PathValue("secret"), r.URL.Query().Get("instance")
	app, ok := s.registeredApp(w, r, r.PathValue("name"))
	if !ok {
		return apiv1.AppSecretValue{}, statusOf(w)
	}
	noteAudit(r, "app", app.Name)
	if !s.secretsReady(w, r) {
		return apiv1.AppSecretValue{}, http.StatusServiceUnavailable
	}
	// No stored secret has a name that breaks the rules.
	if checkSecretName(secret) != "" {
		return failed(http.StatusNotFound, "secret_not_found", fmt.Sprintf("app %s has no such secret: names match ^[A-Z_][A-Z0-9_]*$", app.Name))
	}
	noteAudit(r, "secret", secret)
	ctx := r.Context()
	if instance != "" {
		ok, err := s.isInstance(ctx, app, instance)
		if err != nil {
			return internal(err)
		}
		if !ok {
			return failed(http.StatusNotFound, "instance_not_found", fmt.Sprintf("app %s has no instance %q", app.Name, instance))
		}
		noteAudit(r, "instance", instance)
	}
	row, err := s.d.Store.GetAppSecret(ctx, app.Name, instance, secret)
	if errors.Is(err, store.ErrNotFound) {
		where := "app-wide"
		if instance != "" {
			where = "in instance " + instance
		}
		return failed(http.StatusNotFound, "secret_not_found", fmt.Sprintf("app %s has no secret %s %s", app.Name, secret, where))
	}
	if err != nil {
		return internal(err)
	}
	value, err := s.d.Secrets.OpenRow(row)
	if err != nil {
		// The error names the secret and, for another key, both key
		// checks; never the value or the key.
		return internal(err)
	}
	out := apiv1.AppSecretValue{Name: row.Name, Value: value, UpdatedAt: row.UpdatedAt, UpdatedBy: row.UpdatedBy}
	if instance != "" {
		out.Instance = &instance
	}
	return out, http.StatusOK
}

// statusOf is the status already written to w, a *recorder of route.
func statusOf(w http.ResponseWriter) int {
	if rec, ok := w.(*recorder); ok {
		return rec.status
	}
	return http.StatusInternalServerError
}
