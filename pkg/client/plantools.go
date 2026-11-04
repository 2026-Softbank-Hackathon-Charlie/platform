package client

import (
	"context"
	"fmt"
	"io"
	"net/http"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

// AppendWrittenRecords adds writes a load generator saw succeed to a
// written-records stream (at most 5000 per call).
func (c *Client) AppendWrittenRecords(ctx context.Context, stream string, recs []apiv1.WrittenRecord) (*apiv1.WrittenRecordsAccepted, error) {
	return call[apiv1.WrittenRecordsAccepted](ctx, c, request{method: http.MethodPost, path: "/v1/written-records/%s", params: []string{stream},
		body: apiv1.WrittenRecordsRequest{Records: recs}})
}

// GetWrittenRecordStats returns how many records a stream holds.
func (c *Client) GetWrittenRecordStats(ctx context.Context, stream string) (*apiv1.WrittenRecordStats, error) {
	return call[apiv1.WrittenRecordStats](ctx, c, request{method: http.MethodGet, path: "/v1/written-records/%s", params: []string{stream}})
}

// PutPlanAttachment uploads a file a pending plan refers to as "attachment:<name>".
func (c *Client) PutPlanAttachment(ctx context.Context, ref, name string, content []byte) (*apiv1.PlanAttachment, error) {
	if content == nil {
		content = []byte{}
	}
	return call[apiv1.PlanAttachment](ctx, c, request{method: http.MethodPut, path: "/v1/plans/%s/attachments/%s", params: []string{ref, name},
		body: content, header: http.Header{"Content-Type": {"application/octet-stream"}}})
}

// GetPlanAttachment downloads an attachment as uploaded.
func (c *Client) GetPlanAttachment(ctx context.Context, ref, name string) ([]byte, error) {
	r := request{method: http.MethodGet, path: "/v1/plans/%s/attachments/%s", params: []string{ref, name},
		header: http.Header{"Accept": {"application/octet-stream"}}}
	resp, err := c.send(ctx, r)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("client: read %s %s response: %w", r.method, r.path, err)
	}
	return b, nil
}

// ListApps returns the apps the server can draft plans for.
func (c *Client) ListApps(ctx context.Context) (*apiv1.AppList, error) {
	return call[apiv1.AppList](ctx, c, request{method: http.MethodGet, path: "/v1/apps"})
}

// DraftPlan builds an engine plan from the server's app configuration. The
// draft is not stored; submit it with SubmitPlan. req.SourceStats is ignored:
// use DraftPlanWithSourceStats for the source's numbers.
func (c *Client) DraftPlan(ctx context.Context, req apiv1.PlanDraftRequest) (*apiv1.EnginePlan, error) {
	req.SourceStats = nil
	return call[apiv1.EnginePlan](ctx, c, request{method: http.MethodPost, path: "/v1/plans/draft", body: req})
}

// DraftPlanWithSourceStats is DraftPlan that also measures the source
// database (about five seconds). The plan is in Plan; SourceStats is nil and
// SourceStatsReason set when the server could not measure.
func (c *Client) DraftPlanWithSourceStats(ctx context.Context, req apiv1.PlanDraftRequest) (*apiv1.PlanDraftResult, error) {
	on := true
	req.SourceStats = &on
	return call[apiv1.PlanDraftResult](ctx, c, request{method: http.MethodPost, path: "/v1/plans/draft", body: req})
}

// RollbackMigration migrates a succeeded run back and returns the new run.
func (c *Client) RollbackMigration(ctx context.Context, runID string, req apiv1.RollbackRequest) (*apiv1.StartMigrationResponse, error) {
	return call[apiv1.StartMigrationResponse](ctx, c, request{method: http.MethodPost, path: "/v1/migrations/%s/rollback", params: []string{runID}, body: req})
}

// InspectBucket measures a bucket the residency policy registers and
// compares its first-level prefixes with the policy. With prefix it also
// describes the objects under that prefix (counts only, never keys).
func (c *Client) InspectBucket(ctx context.Context, bucket, prefix string) (*apiv1.BucketInspection, error) {
	req := apiv1.BucketInspectRequest{Bucket: bucket}
	if prefix != "" {
		req.Prefix = &prefix
	}
	return call[apiv1.BucketInspection](ctx, c, request{method: http.MethodPost, path: "/v1/residency/buckets/inspect", body: req})
}
