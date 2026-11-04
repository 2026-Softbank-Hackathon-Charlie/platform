package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"

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

// revealSecretRoute is audited although it is a read: it returns a value.
const revealSecretRoute = "GET /v1/apps/{name}/secrets/{secret}"

var secretNamePattern = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)

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
// The audit entry has the names, never the values.
func (s *Server) updateAppSecrets(w http.ResponseWriter, r *http.Request) {
	var req apiv1.AppSecretsUpdate
	if !s.decode(w, r, &req, true) {
		return
	}
	name := r.PathValue("name")
	noteAudit(r, "app", name)
	instance := ""
	if req.Instance != nil {
		instance = *req.Instance
		noteAudit(r, "instance", instance)
	}
	var set map[string]string
	if req.Set != nil {
		set = *req.Set
	}
	var del []string
	if req.Delete != nil {
		del = *req.Delete
	}
	names := make([]string, 0, len(set))
	for n := range set {
		names = append(names, n)
	}
	slices.Sort(names)
	if len(names) > 0 {
		noteAudit(r, "set", names)
	}
	if len(del) > 0 {
		noteAudit(r, "deleted", del)
	}
	app, ok := s.registeredApp(w, r, name)
	if !ok || !s.secretsReady(w, r) {
		return
	}

	bad := func(msg string) { s.fail(w, r, http.StatusBadRequest, "invalid_request", msg) }
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
	ctx := r.Context()
	if instance != "" {
		ok, err := s.isInstance(ctx, app, instance)
		if err != nil {
			s.internal(w, r, err)
			return
		}
		if !ok {
			s.fail(w, r, http.StatusNotFound, "instance_not_found", fmt.Sprintf("app %s has no instance %s", app.Name, instance))
			return
		}
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
		bad(fmt.Sprintf("too many secrets: at most %d app-wide and %d per instance; delete some first", maxSecretsPerScope, maxSecretsPerScope))
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
// button. Every call is audited (revealSecretRoute), with the name and the
// instance, never the value, and the answer must not be cached.
func (s *Server) getAppSecret(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	name, secret, instance := r.PathValue("name"), r.PathValue("secret"), r.URL.Query().Get("instance")
	noteAudit(r, "app", name)
	noteAudit(r, "secret", secret)
	if instance != "" {
		noteAudit(r, "instance", instance)
	}
	app, ok := s.registeredApp(w, r, name)
	if !ok || !s.secretsReady(w, r) {
		return
	}
	ctx := r.Context()
	if instance != "" {
		ok, err := s.isInstance(ctx, app, instance)
		if err != nil {
			s.internal(w, r, err)
			return
		}
		if !ok {
			s.fail(w, r, http.StatusNotFound, "instance_not_found", fmt.Sprintf("app %s has no instance %s", app.Name, instance))
			return
		}
	}
	row, err := s.d.Store.GetAppSecret(ctx, app.Name, instance, secret)
	if errors.Is(err, store.ErrNotFound) {
		where := "app-wide"
		if instance != "" {
			where = "in instance " + instance
		}
		s.fail(w, r, http.StatusNotFound, "secret_not_found", fmt.Sprintf("app %s has no secret %q %s", app.Name, secret, where))
		return
	}
	if err != nil {
		s.internal(w, r, err)
		return
	}
	value, err := s.d.Secrets.OpenRow(row)
	if err != nil {
		// The error names the secret, never its value.
		s.internal(w, r, err)
		return
	}
	out := apiv1.AppSecretValue{Name: row.Name, Value: value, UpdatedAt: row.UpdatedAt, UpdatedBy: row.UpdatedBy}
	if instance != "" {
		out.Instance = &instance
	}
	writeJSON(w, http.StatusOK, out)
}
