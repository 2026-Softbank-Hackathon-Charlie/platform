package probes

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/preflight/parity"
)

// SecretUsage is operator-owned configuration. Operation bindings describe the
// reviewed app adapter, not proof that every key or permission was exercised.
type SecretUsage struct {
	Service string      `json:"service"`
	Port    int         `json:"port"`
	Checks  []SecretUse `json:"checks"`
}
type SecretUse struct {
	ID        string `json:"id"`
	SecretID  string `json:"secretId"`
	Operation string `json:"operation"`
}

// UsageFetch must use the registered target's Kubernetes service proxy. It
// must not accept arbitrary URLs or return unbounded response bodies.
type UsageFetch func(context.Context, parity.Target, SecretUsage, string) ([]byte, error)

func (c SecretUsage) Validate(required []SecretRequirement) error {
	if !simpleID.MatchString(c.Service) || c.Port < 1 || c.Port > 65535 || len(c.Checks) == 0 || len(c.Checks) > 16 {
		return ErrInvalidConfig
	}
	ids := map[string]bool{}
	known := map[string]bool{}
	for _, s := range required {
		known[s.ID] = true
	}
	for _, q := range c.Checks {
		if !simpleID.MatchString(q.ID) || ids[q.ID] || !known[q.SecretID] || (q.Operation != "database-query" && q.Operation != "bucket-access") {
			return ErrInvalidConfig
		}
		ids[q.ID] = true
	}
	return nil
}

// checkSecretUsage only accepts a fresh challenge-bound structured report.
// An HTTP 200, ready pod, injected env var or missing check is never enough.
func checkSecretUsage(ctx context.Context, target parity.Target, cfg SecretUsage, fetch UsageFetch, required []SecretRequirement) []parity.Check {
	summary := parity.Check{ID: "secret-usability", Status: parity.Unknown, Code: "secret_usage_unavailable"}
	if fetch == nil {
		return []parity.Check{summary}
	}
	var nonce [32]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return []parity.Check{summary}
	}
	challenge := hex.EncodeToString(nonce[:])
	raw, err := fetch(ctx, target, cfg, challenge)
	if err != nil || ctx.Err() != nil || len(raw) > 8192 {
		return []parity.Check{summary}
	}
	var body struct {
		Version   int               `json:"version"`
		Challenge string            `json:"challenge"`
		Checks    map[string]string `json:"checks"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err = dec.Decode(&body); err != nil || body.Version != 1 || body.Challenge != challenge {
		summary.Code = "secret_usage_invalid_report"
		return []parity.Check{summary}
	}
	if dec.Decode(&struct{}{}) != io.EOF {
		summary.Code = "secret_usage_invalid_report"
		return []parity.Check{summary}
	}
	checks := []parity.Check{}
	covered := map[string]bool{}
	status := parity.Pass
	for _, q := range cfg.Checks {
		c := parity.Check{ID: "usage-" + q.ID, Status: parity.Unknown, Code: "secret_usage_not_verified"}
		switch body.Checks[q.Operation] {
		case "pass":
			c.Status, c.Code = parity.Pass, "runtime_"+q.Operation+"_verified"
			covered[q.SecretID] = true
		case "fail":
			c.Status, c.Code = parity.Fail, "runtime_"+q.Operation+"_failed"
		}
		checks = append(checks, c)
		if c.Status == parity.Fail {
			status = parity.Fail
		} else if c.Status != parity.Pass && status != parity.Fail {
			status = parity.Unknown
		}
	}
	for _, s := range required {
		if !covered[s.ID] && status != parity.Fail {
			status = parity.Unknown
		}
	}
	summary.Status = status
	switch status {
	case parity.Pass:
		summary.Code = "configured_runtime_operations_verified"
	case parity.Fail:
		summary.Code = "secret_runtime_operation_failed"
	default:
		summary.Code = "secret_usage_incomplete"
	}
	return append(checks, summary)
}
