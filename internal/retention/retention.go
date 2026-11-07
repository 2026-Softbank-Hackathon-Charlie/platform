// Package retention defines the persisted identity of a source retirement.
// Policy is server-owned; submitted plans cannot enable deletion.
package retention

import (
	"context"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"
)

const Event = "source_retention"

type Pod struct {
	Name string `json:"name"`
	UID  string `json:"uid"`
}

// Job contains no DSNs, kubeconfigs or credentials. UIDs bind retries to the
// objects seen before migration, never a later cluster with the same name.
type Job struct {
	RunID     string        `json:"runId"`
	SourceUID string        `json:"sourceUID"`
	TargetUID string        `json:"targetUID,omitempty"`
	Pods      []Pod         `json:"pods"`
	KeepFor   time.Duration `json:"keepForNanos"`
	DueAt     *time.Time    `json:"dueAt,omitempty"`
	Status    string        `json:"status"`
	Reason    string        `json:"reason,omitempty"`
}

type Backend interface {
	Capture(context.Context, *plan.Plan) (Job, error)
	Check(context.Context, *plan.Plan, Job, time.Time) error
	// Remove returns true only after the original cluster and its captured
	// database pods are absent. PVCs and other resources are retained.
	Remove(context.Context, *plan.Plan, Job) (bool, error)
}
