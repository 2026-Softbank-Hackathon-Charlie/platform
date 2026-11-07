package onprem

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/secrets"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store/storetest"
)

type registrationNetwork struct {
	keys  int
	found bool
}

func (n *registrationNetwork) AuthKey(context.Context, string) (string, error) {
	n.keys++
	return "tskey-auth-" + strings.Repeat("a", 40), nil
}
func (n *registrationNetwork) Device(_ context.Context, host string) (string, bool, error) {
	return host + ".test.ts.net", n.found, nil
}

type registrationRuntime struct {
	checks             []store.OnpremEnrollmentCheck
	published, removed int
	afterPublish       func()
}

func (r *registrationRuntime) Configured(context.Context) error { return nil }
func (r *registrationRuntime) Route(context.Context, store.OnpremEnrollment, string) (string, error) {
	return "https://route.internal:6443", nil
}
func (r *registrationRuntime) Inspect(context.Context, RegistrationState) ([]store.OnpremEnrollmentCheck, error) {
	return r.checks, nil
}
func (r *registrationRuntime) Publish(context.Context, RegistrationState) error {
	r.published++
	if r.afterPublish != nil {
		r.afterPublish()
	}
	return nil
}
func (r *registrationRuntime) Remove(context.Context, string, string) error { r.removed++; return nil }

func registrationFixture(t *testing.T) (*Connector, *Service, Claimed, *registrationRuntime, *registrationNetwork) {
	t.Helper()
	st := storetest.NewMemory()
	dir := t.TempDir()
	for _, arch := range []string{"amd64", "arm64"} {
		if err := os.WriteFile(filepath.Join(dir, arch), []byte("fixture binary"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	cipher, err := secrets.NewCipher(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	runtime := &registrationRuntime{}
	network := &registrationNetwork{found: true}
	c := &Connector{Store: st, Cipher: cipher, Runtime: runtime, Tailnet: network, Origin: "https://api.example.test", AgentDirectory: dir, AgentImage: "docker.io/alpine/git@sha256:" + strings.Repeat("a", 64), TailscaleImage: "tailscale/tailscale@sha256:" + strings.Repeat("b", 64)}
	s := &Service{Store: st, Origin: c.Origin, Registrar: c}
	created, _, err := s.Create(t.Context(), "operator", "registration-test", CreateRequest{Environment: "edge-01"})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := s.Claim(t.Context(), created.Enrollment.ID, created.Code, ClaimRequest{Environment: "edge-01"})
	if err != nil {
		t.Fatal(err)
	}
	return c, s, claimed, runtime, network
}
func requestFixture() ConnectionRequest {
	return ConnectionRequest{ClusterUID: "12345678-1234-1234-1234-123456789012", CAData: base64.StdEncoding.EncodeToString([]byte("fixture CA")), PlatformToken: strings.Repeat("platform-token-", 4), ArgoToken: strings.Repeat("argo-token-", 4), TokenExpiresAt: time.Now().Add(time.Hour)}
}
func successfulChecks() []store.OnpremEnrollmentCheck {
	now := time.Now().UTC()
	var out []store.OnpremEnrollmentCheck
	for _, id := range []string{"k3s", "network", "permissions", "image_pull", "hello_world"} {
		out = append(out, store.OnpremEnrollmentCheck{ID: id, Status: "passed", CheckedAt: &now})
	}
	return out
}

func TestConnectionCannotPublishFromPartialOrDuplicatedChecks(t *testing.T) {
	for _, partial := range [][]store.OnpremEnrollmentCheck{successfulChecks()[:4], append(successfulChecks()[:4], successfulChecks()[0])} {
		c, s, claimed, r, _ := registrationFixture(t)
		r.checks = partial
		e, err := s.AuthenticateConnection(t.Context(), claimed.ID, claimed.ConnectionToken)
		if err != nil {
			t.Fatal(err)
		}
		result, err := c.Connect(t.Context(), e, requestFixture())
		if (err != nil && !errors.Is(err, store.ErrConflict)) || result.Status == "ready" || r.published != 0 {
			t.Fatalf("unverified registration published: status=%s error=%v", result.Status, err)
		}
	}
}

func TestRegistrationPersistsEncryptedConfigurationAndRevocationWins(t *testing.T) {
	c, s, claimed, r, _ := registrationFixture(t)
	r.checks = successfulChecks()
	e, err := s.AuthenticateConnection(t.Context(), claimed.ID, claimed.ConnectionToken)
	if err != nil {
		t.Fatal(err)
	}
	result, err := c.Connect(t.Context(), e, requestFixture())
	if err != nil || result.Status != "ready" {
		t.Fatalf("register: %s %v", result.Status, err)
	}
	serialized, _ := json.Marshal(result)
	for _, secret := range []string{claimed.ConnectionToken, requestFixture().PlatformToken, requestFixture().ArgoToken} {
		if strings.Contains(string(serialized), secret) {
			t.Fatal("plaintext registration credential persisted")
		}
	}
	if err := c.Restore(t.Context()); err != nil || r.published != 2 {
		t.Fatalf("restore: %v publishes=%d", err, r.published)
	}
	e, err = s.AuthenticateConnection(t.Context(), claimed.ID, claimed.ConnectionToken)
	if err != nil {
		t.Fatal(err)
	}
	r.afterPublish = func() {
		if _, err := s.Revoke(t.Context(), claimed.ID); err != nil {
			t.Error(err)
		}
	}
	_, err = c.Connect(t.Context(), e, requestFixture())
	if !errors.Is(err, store.ErrConflict) || r.removed != 1 {
		t.Fatalf("revoked publication not cleaned: %v remove=%d", err, r.removed)
	}
	if _, err := s.AuthenticateConnection(t.Context(), claimed.ID, claimed.ConnectionToken); err == nil {
		t.Fatal("revoked session authenticated")
	}
}

func TestBootstrapIsOneUseAndDoesNotMintBeforeSessionReservation(t *testing.T) {
	c, s, claimed, _, network := registrationFixture(t)
	e, err := s.AuthenticateConnection(t.Context(), claimed.ID, claimed.ConnectionToken)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.Bootstrap(t.Context(), e, claimed.ConnectionToken, "amd64"); err != nil {
		t.Fatal(err)
	}
	if _, err = c.Bootstrap(t.Context(), e, claimed.ConnectionToken, "amd64"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("bootstrap repeated: %v", err)
	}
	if network.keys != 1 {
		t.Fatalf("minted %d keys", network.keys)
	}
}

type revokeAfterRegistrationSnapshot struct {
	store.EnrollmentStore
	after func()
}

func (s revokeAfterRegistrationSnapshot) ListRegisteredOnpremEnvironments(ctx context.Context, now time.Time) ([]store.OnpremEnrollment, error) {
	rows, err := s.EnrollmentStore.ListRegisteredOnpremEnvironments(ctx, now)
	if err == nil {
		s.after()
	}
	return rows, err
}

func TestRestoreCannotResurrectAConcurrentRevocation(t *testing.T) {
	c, s, claimed, r, _ := registrationFixture(t)
	r.checks = successfulChecks()
	e, err := s.AuthenticateConnection(t.Context(), claimed.ID, claimed.ConnectionToken)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.Connect(t.Context(), e, requestFixture()); err != nil {
		t.Fatal(err)
	}
	c.Store = revokeAfterRegistrationSnapshot{EnrollmentStore: c.Store, after: func() {
		if _, err := s.Revoke(t.Context(), claimed.ID); err != nil {
			t.Fatal(err)
		}
		_ = r.Remove(t.Context(), claimed.ID, e.Environment)
	}}
	if err := c.Restore(t.Context()); err == nil {
		t.Fatal("stale restore did not observe revocation")
	}
	if r.published != 1 {
		t.Fatalf("revoked registration was republished %d times", r.published)
	}
}
