package onprem

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/secrets"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
)

const OnpremTailnetTag = "tag:hop-onprem-k3s"

type ConnectionRequest struct {
	ClusterUID     string    `json:"clusterUID"`
	CAData         string    `json:"caData"`
	PlatformToken  string    `json:"platformToken"`
	ArgoToken      string    `json:"argoToken"`
	TokenExpiresAt time.Time `json:"tokenExpiresAt"`
}

// RegistrationState is encrypted before persistence. It is never a browser response.
type RegistrationState struct {
	EnrollmentID string            `json:"enrollmentID"`
	Environment  string            `json:"environment"`
	Endpoint     string            `json:"endpoint"`
	Connection   ConnectionRequest `json:"connection"`
}

type RegistrationTailnet interface {
	AuthKey(context.Context, string) (string, error)
	Device(context.Context, string) (fqdn string, found bool, err error)
}

// RegistrationRuntime owns only resources marked with this enrollment ID.
// Publish and Remove must use resource-version/ownership checks, never replace an existing environment.
type RegistrationRuntime interface {
	Configured(context.Context) error
	Route(context.Context, store.OnpremEnrollment, string) (string, error)
	Inspect(context.Context, RegistrationState) ([]store.OnpremEnrollmentCheck, error)
	Publish(context.Context, RegistrationState) error
	Remove(context.Context, string, string) error
}

type Connector struct {
	Store                      store.EnrollmentStore
	Cipher                     *secrets.Cipher
	Tailnet                    RegistrationTailnet
	Runtime                    RegistrationRuntime
	Origin, AgentDirectory     string
	AgentImage, TailscaleImage string
	Now                        func() time.Time
}

func (c *Connector) now() time.Time {
	if c.Now != nil {
		return c.Now().UTC()
	}
	return time.Now().UTC()
}
func RegistrationHostname(id string) string { return "hop-k3s-" + strings.TrimPrefix(id, "enr_") }

func (c *Connector) CheckConfiguration(ctx context.Context, environment string) error {
	if c == nil || c.Store == nil || c.Cipher == nil || c.Tailnet == nil || c.Runtime == nil {
		return ErrUnavailable
	}
	if _, err := origin(c.Origin); err != nil {
		return ErrUnavailable
	}
	if !namePattern.MatchString(environment) {
		return ErrInvalid
	}
	if !strings.Contains(c.AgentImage, "@sha256:") || !strings.Contains(c.TailscaleImage, "@sha256:") {
		return ErrUnavailable
	}
	for _, arch := range []string{"amd64", "arm64"} {
		if _, _, err := AgentBinary(c.AgentDirectory, arch); err != nil {
			return ErrUnavailable
		}
	}
	if err := c.Runtime.Configured(ctx); err != nil {
		return ErrUnavailable
	}
	return nil
}

func (c *Connector) Bootstrap(ctx context.Context, e store.OnpremEnrollment, token, arch string) ([]byte, error) {
	if err := c.CheckConfiguration(ctx, e.Environment); err != nil {
		return nil, err
	}
	if e.Status != "claimed" || e.ClusterUID != "" {
		return nil, store.ErrConflict
	}
	_, sum, err := AgentBinary(c.AgentDirectory, arch)
	if err != nil {
		return nil, ErrInvalid
	}
	key, err := c.Tailnet.AuthKey(ctx, RegistrationHostname(e.ID))
	if err != nil {
		return nil, ErrUnavailable
	}
	// The key is one-use and expires after one hour. It is neither logged nor persisted.
	return BootstrapManifest(BootstrapConfig{EnrollmentID: e.ID, Origin: c.Origin, ConnectionToken: token, Hostname: RegistrationHostname(e.ID), TailnetAuthKey: key, AgentImage: c.AgentImage, TailscaleImage: c.TailscaleImage, AgentArch: arch, AgentURL: strings.TrimRight(c.Origin, "/") + "/v1/onprem/agent.bin?arch=" + arch, AgentSHA256: sum})
}

func (c *Connector) Connect(ctx context.Context, e store.OnpremEnrollment, req ConnectionRequest) (store.OnpremEnrollment, error) {
	now := c.now()
	if len(req.ClusterUID) < 8 || len(req.ClusterUID) > 128 || strings.ContainsAny(req.ClusterUID, "/\\ \r\n") || len(req.PlatformToken) < 20 || len(req.ArgoToken) < 20 || len(req.PlatformToken) > 16384 || len(req.ArgoToken) > 16384 || len(req.CAData) > 32768 || req.TokenExpiresAt.Before(now.Add(10*time.Minute)) || req.TokenExpiresAt.After(now.Add(2*time.Hour)) {
		return e, ErrInvalid
	}
	if _, err := base64.StdEncoding.DecodeString(req.CAData); err != nil {
		return e, ErrInvalid
	}
	if err := c.CheckConfiguration(ctx, e.Environment); err != nil {
		return e, err
	}
	if e.Status == "ready" && e.ClusterUID != req.ClusterUID {
		return e, store.ErrConflict
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return e, err
	}
	lease := hex.EncodeToString(b)
	row, err := c.Store.BeginOnpremRegistration(ctx, e.ID, e.SessionHash, req.ClusterUID, lease, now, now.Add(3*time.Minute))
	if err != nil {
		return e, err
	}
	checks := []store.OnpremEnrollmentCheck{}
	state := RegistrationState{EnrollmentID: e.ID, Environment: e.Environment, Connection: req}
	save := func(status, stage string, encrypted []byte) (store.OnpremEnrollment, error) {
		expires := c.now().Add(30 * 24 * time.Hour)
		return c.Store.SaveOnpremRegistration(context.WithoutCancel(ctx), e.ID, lease, row.Version, store.OnpremRegistrationUpdate{Status: status, Stage: stage, Checks: checks, RegistrationCiphertext: encrypted, ReleaseLease: true, SessionExpiresAt: &expires}, c.now())
	}
	_, found, err := c.Tailnet.Device(ctx, RegistrationHostname(e.ID))
	if err != nil || !found {
		checks = append(checks, store.OnpremEnrollmentCheck{ID: "network", Status: "pending", Message: "장비의 전용 Tailscale 연결을 기다립니다."})
		return save("verifying", "waiting_for_device", nil)
	}
	// Runtime constructs an internal endpoint from a server-generated hostname only.
	state.Endpoint, err = c.Runtime.Route(ctx, row, RegistrationHostname(e.ID))
	if err != nil {
		checks = append(checks, store.OnpremEnrollmentCheck{ID: "network", Status: "failed", Message: "관리 평면의 장비 전용 API 경로를 준비하지 못했습니다."})
		return save("failed", "api_route_failed", nil)
	}
	u, err := url.Parse(state.Endpoint)
	if err != nil || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return save("failed", "invalid_internal_route", nil)
	}
	checks, err = c.Runtime.Inspect(ctx, state)
	if err != nil || !allChecksPassed(checks) {
		return save("verifying", "cluster_checks", nil)
	}
	encoded, err := c.seal(state)
	if err != nil {
		return e, err
	}
	// Check the fenced state immediately before publishing external credentials.
	current, err := c.Store.GetOnpremEnrollment(ctx, e.ID, c.now())
	if err != nil || current.LeaseID != lease || current.Version != row.Version || current.Status == "revoked" {
		return e, store.ErrConflict
	}
	if err = c.Runtime.Publish(ctx, state); err != nil {
		return save("failed", "environment_registration_failed", encoded)
	}
	result, err := save("ready", "ready", encoded)
	if err != nil {
		// A revocation that won the final CAS must also revoke these external credentials.
		current, readErr := c.Store.GetOnpremEnrollment(context.WithoutCancel(ctx), e.ID, c.now())
		if readErr == nil && current.Status == "revoked" {
			_ = c.Runtime.Remove(context.WithoutCancel(ctx), e.ID, e.Environment)
		}
	}
	return result, err
}

func allChecksPassed(checks []store.OnpremEnrollmentCheck) bool {
	seen := map[string]bool{}
	for _, c := range checks {
		if c.Status != "passed" || c.CheckedAt == nil || c.CheckedAt.IsZero() || seen[c.ID] {
			return false
		}
		seen[c.ID] = true
	}
	for _, id := range []string{"k3s", "network", "permissions", "image_pull", "hello_world"} {
		if !seen[id] {
			return false
		}
	}
	return len(seen) == 5
}

type sealedRegistration struct {
	Version           int `json:"version"`
	Nonce, Ciphertext []byte
}

func (c *Connector) seal(state RegistrationState) ([]byte, error) {
	b, err := json.Marshal(state)
	if err != nil {
		return nil, err
	}
	nonce, ct, version, err := c.Cipher.Seal("onprem", state.EnrollmentID, "registration", string(b))
	if err != nil {
		return nil, err
	}
	return json.Marshal(sealedRegistration{Version: version, Nonce: nonce, Ciphertext: ct})
}

func (c *Connector) Restore(ctx context.Context) error {
	rows, err := c.Store.ListRegisteredOnpremEnvironments(ctx, c.now())
	if err != nil {
		return err
	}
	for _, e := range rows {
		var envelope sealedRegistration
		if json.Unmarshal(e.RegistrationCiphertext, &envelope) != nil {
			return errors.New("invalid encrypted onprem registration")
		}
		plain, err := c.Cipher.Open("onprem", e.ID, "registration", envelope.Version, envelope.Nonce, envelope.Ciphertext)
		if err != nil {
			return errors.New("onprem registration key does not match")
		}
		var state RegistrationState
		if json.Unmarshal([]byte(plain), &state) != nil || state.EnrollmentID != e.ID || state.Environment != e.Environment || state.Connection.ClusterUID != e.ClusterUID {
			return errors.New("onprem registration identity mismatch")
		}
		if state.Connection.TokenExpiresAt.Before(c.now()) {
			continue
		}
		if err := c.Runtime.Publish(ctx, state); err != nil {
			return fmt.Errorf("restore onprem environment %s failed", e.Environment)
		}
	}
	return nil
}

func ConnectionTokenHash(token string) []byte { v := sha256.Sum256([]byte(token)); return v[:] }
