// Package plans holds submitted migration plans and their human approval.
//
// Lifecycle: pending_approval → approved → started. A plan starts at most one run.
package plans

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"
)

// Status of a submitted plan.
type Status string

const (
	StatusPending  Status = "pending_approval"
	StatusApproved Status = "approved"
	StatusStarting Status = "starting" // reserved while a run is being created
	StatusStarted  Status = "started"
)

var (
	ErrNotFound    = errors.New("plan not found")
	ErrInvalid     = errors.New("invalid plan")
	ErrWrongStatus = errors.New("plan is not in the required status")
)

// Endpoint is the non-secret view of a plan endpoint. DSNs never leave the server.
type Endpoint struct {
	Env       string `json:"env"`
	Context   string `json:"context,omitempty"`
	Namespace string `json:"namespace"`
	Cluster   string `json:"cluster"`
}

// Summary is what clients see of a plan.
type Summary struct {
	PlanID      string   `json:"planId"`
	Source      Endpoint `json:"source"`
	Target      Endpoint `json:"target"`
	Files       bool     `json:"files"`
	GatewayMode string   `json:"gatewayMode"`
	FenceGate   bool     `json:"fenceGate"`
	MaxDowntime string   `json:"maxDowntime"`
}

// Record is a submitted plan.
type Record struct {
	Ref         string     `json:"ref"` // server-assigned, e.g. "pln_3f9c1a0b2d4e"
	Status      Status     `json:"status"`
	Summary     Summary    `json:"summary"`
	SubmittedBy string     `json:"submittedBy"`
	SubmittedAt time.Time  `json:"submittedAt"`
	ApprovedBy  string     `json:"approvedBy,omitempty"`
	ApprovedAt  *time.Time `json:"approvedAt,omitempty"`
	RunID       string     `json:"runId,omitempty"`

	raw []byte // unexpanded plan JSON, re-parsed at start so secrets come from the current env
}

// Service stores plans in memory. TODO: persist in the platform PostgreSQL.
type Service struct {
	policy Policy
	now    func() time.Time

	mu   sync.Mutex
	recs map[string]*Record
}

// New returns a Service enforcing policy.
func New(policy Policy) *Service {
	return &Service{policy: policy, now: func() time.Time { return time.Now().UTC() }, recs: map[string]*Record{}}
}

// Parse applies the policy and parses raw plan JSON.
func (s *Service) Parse(raw []byte) (*plan.Plan, error) {
	if err := s.policy.checkRaw(raw); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	p, err := plan.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if err := s.policy.checkParsed(p); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return p, nil
}

// Submit validates and stores a plan awaiting approval.
func (s *Service) Submit(raw []byte, actor string) (Record, error) {
	p, err := s.Parse(raw)
	if err != nil {
		return Record{}, err
	}
	r := &Record{
		Ref:         newRef("pln_"),
		Status:      StatusPending,
		Summary:     summarize(p),
		SubmittedBy: actor,
		SubmittedAt: s.now(),
		raw:         append([]byte(nil), raw...),
	}
	s.mu.Lock()
	s.recs[r.Ref] = r
	s.mu.Unlock()
	return *r, nil
}

// Get returns a plan by ref.
func (s *Service) Get(ref string) (Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.recs[ref]
	if !ok {
		return Record{}, ErrNotFound
	}
	return *r, nil
}

// List returns all plans, newest first.
func (s *Service) List() []Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Record, 0, len(s.recs))
	for _, r := range s.recs {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SubmittedAt.After(out[j].SubmittedAt) })
	return out
}

// Approve records human approval. Approving twice is a no-op.
func (s *Service) Approve(ref, actor string) (Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.recs[ref]
	if !ok {
		return Record{}, ErrNotFound
	}
	switch r.Status {
	case StatusApproved:
		return *r, nil
	case StatusPending:
		now := s.now()
		r.Status, r.ApprovedBy, r.ApprovedAt = StatusApproved, actor, &now
		return *r, nil
	default:
		return *r, fmt.Errorf("%w: plan is %s", ErrWrongStatus, r.Status)
	}
}

// Reserve moves an approved plan to starting and returns its parsed plan.
// Exactly one caller can reserve a plan; call Started or Release afterwards.
func (s *Service) Reserve(ref string) (*plan.Plan, error) {
	s.mu.Lock()
	r, ok := s.recs[ref]
	if !ok {
		s.mu.Unlock()
		return nil, ErrNotFound
	}
	if r.Status != StatusApproved {
		st := r.Status
		s.mu.Unlock()
		return nil, fmt.Errorf("%w: plan is %s", ErrWrongStatus, st)
	}
	r.Status = StatusStarting
	raw := r.raw
	s.mu.Unlock()

	p, err := s.Parse(raw) // re-parse: secrets come from the environment at start time
	if err != nil {
		s.Release(ref)
		return nil, err
	}
	return p, nil
}

// Started links a reserved plan to its run.
func (s *Service) Started(ref, runID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.recs[ref]; ok && r.Status == StatusStarting {
		r.Status, r.RunID = StatusStarted, runID
	}
}

// Release returns a reserved plan to approved (the run could not be created).
func (s *Service) Release(ref string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.recs[ref]; ok && r.Status == StatusStarting {
		r.Status = StatusApproved
	}
}

func summarize(p *plan.Plan) Summary {
	ep := func(e plan.Endpoint) Endpoint {
		return Endpoint{Env: e.Env, Context: e.Context, Namespace: e.Namespace, Cluster: e.Cluster}
	}
	return Summary{
		PlanID:      p.ID,
		Source:      ep(p.Source),
		Target:      ep(p.Target),
		Files:       p.Files != nil,
		GatewayMode: p.Gateway.Mode,
		FenceGate:   *p.Verify.FenceGate,
		MaxDowntime: p.Thresholds.MaxDowntime.String(),
	}
}

func newRef(prefix string) string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return prefix + hex.EncodeToString(b)
}
