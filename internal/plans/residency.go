package plans

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/preflight"
)

// ResidencyGate decides whether a parsed plan may start (gate.Gate).
type ResidencyGate interface {
	Check(ctx context.Context, p *plan.Plan) (preflight.ResidencyVerdict, error)
}

// SetResidencyGate makes Reserve refuse plans the residency policy denies.
// Without a gate every approved plan may start, as before.
func (s *Service) SetResidencyGate(g ResidencyGate) { s.residency = g }

// Residency outcomes recorded on a reservation.
const (
	ResidencyNotConfigured  = "not_configured"
	ResidencyAllowed        = "allowed"
	ResidencyRollbackExempt = "rollback_exempt"
)

// ResidencyCheck is what the gate decided for a reservation, for the audit log.
type ResidencyCheck struct {
	Outcome      string
	PolicyDigest string
	// Buckets is what the gate measured about the files copy's buckets.
	Buckets []preflight.ObservedBucket
	Content *preflight.FileInspection
}

// ResidencyDenied is the error Reserve returns when the policy refuses a plan.
// It matches ErrResidencyDenied with errors.Is.
type ResidencyDenied struct {
	Verdict preflight.ResidencyVerdict
}

// ErrResidencyDenied is matched by every ResidencyDenied.
var ErrResidencyDenied = errors.New("residency policy denies this migration")

func (e *ResidencyDenied) Error() string {
	return fmt.Sprintf("%v: %s", ErrResidencyDenied, strings.Join(ReasonTexts(e.Verdict.Reasons), "; "))
}

func (e *ResidencyDenied) Is(target error) bool { return target == ErrResidencyDenied }

// ReasonTexts renders reasons as "code", "code db.schema.table" or "code
// remote:bucket". Reasons hold only validated identifiers and fixed codes,
// never DSNs or SQL errors.
func ReasonTexts(rs []preflight.Reason) []string {
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		t := r.Code
		if d := r.Dataset; d != nil {
			t += " " + d.Database + "." + d.Schema + "." + d.Table
		}
		if r.Bucket != "" {
			t += " " + r.Bucket
		}
		if r.Prefix != "" {
			t += " " + r.Prefix
		}
		if r.Replica != "" {
			t += " -> " + r.Replica
		}
		out = append(out, t)
	}
	return out
}

// checkResidency runs the gate for a reservation. A rollback plan is exempt:
// the server builds it (Rollback) as the reverse of a run that already moved
// the data, it returns the data to the environment it came from, and blocking
// it could keep an application in a failing environment.
func (s *Service) checkResidency(ctx context.Context, rollbackOf string, p *plan.Plan) (ResidencyCheck, error) {
	switch {
	case s.residency == nil:
		return ResidencyCheck{Outcome: ResidencyNotConfigured}, nil
	case rollbackOf != "":
		return ResidencyCheck{Outcome: ResidencyRollbackExempt}, nil
	}
	v, err := s.residency.Check(ctx, p)
	if err != nil || !v.Allowed {
		v.Allowed = false
		if len(v.Reasons) == 0 {
			v.Reasons = []preflight.Reason{{Code: "evaluation_failed"}}
		}
		return ResidencyCheck{PolicyDigest: v.PolicyDigest, Buckets: v.Buckets, Content: v.Content}, &ResidencyDenied{Verdict: v}
	}
	return ResidencyCheck{Outcome: ResidencyAllowed, PolicyDigest: v.PolicyDigest, Buckets: v.Buckets, Content: v.Content}, nil
}

// BucketEvidence renders what the gate measured, one line per bucket, e.g.
// "minio:photos hosting=self location=us-east-1 endpoint=100.64.0.7
// prefixes=(root),rooms/ replicas=- tiered=false". No credentials or hosts.
func BucketEvidence(bs []preflight.ObservedBucket) []string {
	list := func(xs []string) string {
		if len(xs) == 0 {
			return "-"
		}
		return strings.Join(xs, ",")
	}
	out := make([]string, 0, len(bs))
	for _, b := range bs {
		o := b.Observation
		loc := o.Location
		if loc == "" {
			loc = "-"
		}
		if o.LocationType != "" && o.LocationType != "region" {
			loc += "(" + o.LocationType + ")"
		}
		prefixes := make([]string, len(o.Prefixes))
		for i, p := range o.Prefixes {
			if p == "" {
				p = "(root)"
			}
			prefixes[i] = p
		}
		out = append(out, fmt.Sprintf("%s hosting=%s location=%s endpoint=%s prefixes=%s replicas=%s tiered=%t",
			b.Bucket, o.Hosting, loc, list(o.EndpointIPs), list(prefixes), list(o.Replicas), o.Tiered))
	}
	return out
}
