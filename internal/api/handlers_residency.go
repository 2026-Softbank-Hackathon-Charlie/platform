package api

import (
	"errors"
	"net/http"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/preflight/gate"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

// inspectBucket measures a bucket the residency policy registers and
// compares its prefixes with the policy (POST /v1/residency/buckets/inspect).
// It answers with counts and identifiers only; it never allows a run.
func (s *Server) inspectBucket(w http.ResponseWriter, r *http.Request) {
	var req apiv1.BucketInspectRequest
	if !s.decode(w, r, &req, true) {
		return
	}
	if req.Bucket == "" {
		s.fail(w, r, http.StatusBadRequest, "invalid_request", "bucket is required")
		return
	}
	prefix := ""
	if req.Prefix != nil {
		prefix = *req.Prefix
	}
	if s.d.Buckets == nil {
		s.fail(w, r, http.StatusServiceUnavailable, "residency_not_configured", "this server has no residency policy (PLATFORM_RESIDENCY_POLICY_FILE)")
		return
	}
	in, err := s.d.Buckets.Inspect(r.Context(), req.Bucket, prefix)
	var probe *gate.ProbeError
	switch {
	case errors.Is(err, gate.ErrUnknownBucket):
		s.fail(w, r, http.StatusNotFound, "bucket_not_registered", "the residency policy does not register this bucket")
		return
	case errors.Is(err, gate.ErrUnknownPrefix):
		s.fail(w, r, http.StatusNotFound, "prefix_not_listed", "prefix is not a listed first-level prefix of the bucket")
		return
	case errors.As(err, &probe):
		noteAudit(r, "probe", probe.Code)
		s.fail(w, r, http.StatusBadGateway, "bucket_probe_failed", "the bucket could not be measured: "+probe.Code)
		return
	case err != nil:
		s.internal(w, r, err)
		return
	}
	noteAudit(r, "bucket", in.Bucket)
	if prefix != "" {
		noteAudit(r, "prefix", prefix)
	}
	writeJSON(w, http.StatusOK, in)
}
