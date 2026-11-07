// Package onprem issues bootstrap grants. It does not register clusters or
// assert deployment readiness without a separate, independently verified adapter.
package onprem

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
)

var (
	ErrInvalid      = errors.New("invalid enrollment request")
	ErrUnauthorized = errors.New("invalid bootstrap grant")
	ErrUnavailable  = errors.New("enrollment issuer configuration unavailable")
	namePattern     = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,30}[a-z0-9])?$`)
	keyPattern      = regexp.MustCompile(`^[A-Za-z0-9._:-]{8,128}$`)
	idPattern       = regexp.MustCompile(`^enr_[a-f0-9]{24}$`)
)

type CreateRequest struct {
	Environment string `json:"environment"`
	InstallK3s  bool   `json:"installK3s"`
}
type ClaimRequest struct {
	Code        string `json:"code"`
	Environment string `json:"environment"`
	InstallK3s  bool   `json:"installK3s"`
}
type Check struct {
	ID        string     `json:"id"`
	Status    string     `json:"status"`
	Message   string     `json:"message,omitempty"`
	CheckedAt *time.Time `json:"checkedAt,omitempty"`
}
type Enrollment struct {
	ID          string    `json:"id"`
	Environment string    `json:"environment"`
	Status      string    `json:"status"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
	ExpiresAt   time.Time `json:"expiresAt"`
	Checks      []Check   `json:"checks"`
}
type Created struct {
	Enrollment     Enrollment `json:"enrollment"`
	Code           string     `json:"code,omitempty"`
	InstallCommand string     `json:"installCommand,omitempty"`
}
type Service struct {
	Store  store.EnrollmentStore
	Origin string
	Now    func() time.Time
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}
func origin(value string) (string, error) {
	u, err := url.Parse(value)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") || strings.ContainsAny(value, "'\"\\ \t\r\n") {
		return "", ErrUnavailable
	}
	return strings.TrimRight(value, "/"), nil
}
func view(e store.OnpremEnrollment) Enrollment {
	v := Enrollment{ID: e.ID, Environment: e.Environment, Status: e.Status, CreatedAt: e.CreatedAt, UpdatedAt: e.UpdatedAt, ExpiresAt: e.ExpiresAt, Checks: []Check{}}
	for _, id := range []string{"k3s", "network", "permissions", "image_pull", "hello_world"} {
		c := Check{ID: id, Status: "pending"}
		if e.Status != "pending" {
			c.Status = "unknown"
			c.Message = "등록 작업자가 구성되지 않아 서버에서 검증하지 않았습니다."
		}
		v.Checks = append(v.Checks, c)
	}
	return v
}
func (s *Service) Create(ctx context.Context, actor, key string, req CreateRequest) (Created, bool, error) {
	if !namePattern.MatchString(req.Environment) || req.Environment == "eks" || req.Environment == "gke" || !keyPattern.MatchString(key) {
		return Created{}, false, ErrInvalid
	}
	base, err := origin(s.Origin)
	if err != nil || s.Store == nil {
		return Created{}, false, ErrUnavailable
	}
	raw := make([]byte, 32)
	if _, err = rand.Read(raw); err != nil {
		return Created{}, false, err
	}
	code := "onp_" + base64.RawURLEncoding.EncodeToString(raw)
	id := make([]byte, 12)
	if _, err = rand.Read(id); err != nil {
		return Created{}, false, err
	}
	hash, requestHash := sha256.Sum256([]byte(code)), sha256.Sum256([]byte(key))
	now := s.now()
	e, created, err := s.Store.CreateOnpremEnrollment(ctx, store.OnpremEnrollment{ID: "enr_" + hex.EncodeToString(id), Actor: actor, Environment: req.Environment, InstallK3s: req.InstallK3s, CodeHash: hash[:], RequestHash: requestHash[:], Status: "pending", CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(15 * time.Minute)})
	if err != nil {
		return Created{}, false, err
	}
	if !created {
		v, err := s.Get(ctx, e.ID)
		return Created{Enrollment: v}, false, err
	}
	cmd := fmt.Sprintf("(umask 077; hopcharlie_script=$(mktemp) || exit; trap 'rm -f \"$hopcharlie_script\"' EXIT HUP INT TERM; curl --proto '=https' --tlsv1.2 --connect-timeout 10 --max-time 60 -fsS '%s/v1/onprem/installer.sh' -o \"$hopcharlie_script\" && sh \"$hopcharlie_script\" --server '%s' --enrollment '%s' --environment '%s' --code '%s'", base, base, e.ID, e.Environment, code)
	if req.InstallK3s {
		cmd += " --install-k3s"
	}
	cmd += ")"
	return Created{Enrollment: view(e), Code: code, InstallCommand: cmd}, true, nil
}
func (s *Service) Get(ctx context.Context, id string) (Enrollment, error) {
	if !idPattern.MatchString(id) {
		return Enrollment{}, store.ErrNotFound
	}
	e, err := s.Store.GetOnpremEnrollment(ctx, id, s.now())
	return view(e), err
}
func (s *Service) Claim(ctx context.Context, id, code string, req ClaimRequest) (Enrollment, error) {
	if !idPattern.MatchString(id) || len(code) != 47 || !strings.HasPrefix(code, "onp_") {
		return Enrollment{}, ErrUnauthorized
	}
	e, err := s.Store.GetOnpremEnrollment(ctx, id, s.now())
	if errors.Is(err, store.ErrNotFound) {
		return Enrollment{}, ErrUnauthorized
	}
	if err != nil {
		return Enrollment{}, err
	}
	hash := sha256.Sum256([]byte(code))
	if subtle.ConstantTimeCompare(e.CodeHash, hash[:]) != 1 {
		return Enrollment{}, ErrUnauthorized
	}
	if e.Environment != req.Environment || e.InstallK3s != req.InstallK3s {
		return Enrollment{}, ErrInvalid
	}
	e, err = s.Store.ClaimOnpremEnrollment(ctx, id, hash[:], s.now())
	return view(e), err
}
func (s *Service) Revoke(ctx context.Context, id string) (Enrollment, error) {
	if !idPattern.MatchString(id) {
		return Enrollment{}, store.ErrNotFound
	}
	e, err := s.Store.RevokeOnpremEnrollment(ctx, id, s.now())
	return view(e), err
}
