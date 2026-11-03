package residency

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/preflight"
	"github.com/open-policy-agent/opa/v1/rego"
)

//go:embed policy.rego
var policySource string

var (
	ErrNotReady     = errors.New("residency evaluator is not initialized")
	ErrInvalidInput = errors.New("invalid residency input")
	ErrEvaluation   = errors.New("residency evaluation failed")
)

// Evaluator owns a prepared query and an immutable policy snapshot. It is safe
// for concurrent Evaluate calls. Use New; a nil or zero evaluator denies.
type Evaluator struct {
	query  *rego.PreparedEvalQuery
	digest string
}

func New(ctx context.Context, cfg Config) (*Evaluator, error) {
	return prepare(ctx, cfg, policySource)
}

func prepare(ctx context.Context, cfg Config, source string) (*Evaluator, error) {
	cfg, err := cfg.normalized()
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		return nil, ErrInvalidConfig
	}
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, ErrInvalidConfig
	}
	h := sha256.New()
	h.Write([]byte("hopcharlie-residency-v1\x00"))
	h.Write([]byte(source))
	h.Write([]byte{0})
	h.Write(raw)
	q, err := rego.New(
		rego.Query("data.hopcharlie.residency.decision"),
		rego.Module("residency.rego", source),
		rego.Data(map[string]any{"policy": data}),
		rego.StrictBuiltinErrors(true),
		rego.UnsafeBuiltins(map[string]struct{}{"http.send": {}, "net.lookup_ip_addr": {}, "opa.runtime": {}}),
	).PrepareForEval(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, ErrEvaluation // SDK errors may contain input or policy data.
	}
	return &Evaluator{query: &q, digest: hex.EncodeToString(h.Sum(nil))}, nil
}

func (e *Evaluator) Evaluate(ctx context.Context, request preflight.Request) (preflight.ResidencyVerdict, error) {
	v := preflight.ResidencyVerdict{Reasons: []preflight.Reason{}}
	deny := func(code string, err error) (preflight.ResidencyVerdict, error) {
		v.Allowed = false
		v.Reasons = []preflight.Reason{{Code: code}}
		return v, err
	}
	if e == nil || e.query == nil || e.digest == "" {
		return deny("not_ready", ErrNotReady)
	}
	v.PolicyDigest = e.digest
	if err := ctx.Err(); err != nil {
		return deny("evaluation_cancelled", err)
	}
	if !validRequest(request) {
		return deny("invalid_input", ErrInvalidInput)
	}
	results, err := e.query.Eval(ctx, rego.EvalInput(request))
	if ctx.Err() != nil {
		return deny("evaluation_cancelled", ctx.Err())
	}
	if err != nil {
		return deny("evaluation_failed", ErrEvaluation)
	}
	allowed, reasons, err := decodeDecision(results)
	if err != nil {
		return deny("evaluation_failed", ErrEvaluation)
	}
	v.Allowed, v.Reasons = allowed, reasons
	return v, nil
}

func validRequest(r preflight.Request) bool {
	if len(r.Movements) == 0 {
		return false
	}
	ids := map[string]bool{}
	for _, m := range r.Movements {
		if !resourceID.MatchString(m.ID) || ids[m.ID] || m.Kind != preflight.PhysicalReplication ||
			!validScope(m.Source) || !resourceID.MatchString(m.Destination) || m.Source.Environment == m.Destination ||
			!m.Inventory.Complete || m.Inventory.Scope != m.Source || len(m.Inventory.Datasets) == 0 {
			return false
		}
		ids[m.ID] = true
		datasets := map[preflight.Dataset]bool{}
		for _, d := range m.Inventory.Datasets {
			if !validDataset(d) || datasets[d] {
				return false
			}
			datasets[d] = true
		}
	}
	return true
}

// An undefined query, missing boolean, null reasons or inconsistent verdict is
// an evaluation failure, never an implicit allow. Only fixed policy codes pass.
func decodeDecision(rs rego.ResultSet) (bool, []preflight.Reason, error) {
	if len(rs) != 1 || len(rs[0].Expressions) != 1 {
		return false, nil, ErrEvaluation
	}
	raw, err := json.Marshal(rs[0].Expressions[0].Value)
	if err != nil {
		return false, nil, ErrEvaluation
	}
	var d struct {
		Allowed *bool              `json:"allowed"`
		Reasons []preflight.Reason `json:"reasons"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&d); err != nil || d.Allowed == nil || d.Reasons == nil || *d.Allowed != (len(d.Reasons) == 0) {
		return false, nil, ErrEvaluation
	}
	for _, r := range d.Reasons {
		if !resourceID.MatchString(r.MovementID) {
			return false, nil, ErrEvaluation
		}
		switch r.Code {
		case "unknown_source", "unknown_destination", "unclassified_cluster":
			if r.Dataset != nil {
				return false, nil, ErrEvaluation
			}
		case "unclassified_dataset", "inventory_catalog_mismatch", "country_not_allowed":
			if r.Dataset == nil || !validDataset(*r.Dataset) {
				return false, nil, ErrEvaluation
			}
		default:
			return false, nil, ErrEvaluation
		}
	}
	slices.SortFunc(d.Reasons, func(a, b preflight.Reason) int {
		var ad, bd preflight.Dataset
		if a.Dataset != nil {
			ad = *a.Dataset
		}
		if b.Dataset != nil {
			bd = *b.Dataset
		}
		return cmp.Or(cmp.Compare(a.MovementID, b.MovementID), cmp.Compare(a.Code, b.Code), compareDataset(ad, bd))
	})
	return *d.Allowed, d.Reasons, nil
}
