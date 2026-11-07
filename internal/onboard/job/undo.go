package job

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/gateway"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/onboard"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

// undo is DELETE's work (ADR 0042 "DELETE"): the hopai Job and the test
// app first; up to awaiting_approval the PR is closed; from connecting on
// what was done to the source is undone in reverse order, and the gateway
// app and the registered app are deleted. The merged PR stays. Then the row
// goes. A step that fails leaves the failure and deleting true; another
// DELETE tries again.
func (r *Runner) undo(ctx context.Context, x *rec) error {
	if err := r.undoSteps(ctx, x); err != nil {
		_, uerr := r.update(ctx, x.row.ID, nil, func(x *rec) (string, error) {
			x.doc.Failure, x.doc.Internal.DeleteRequested = ptr("deletion: "+err.Error()), false
			return "", nil
		})
		r.event(ctx, x, apiv1.OnboardingEventKindError, "deletion: "+err.Error(), nil)
		return errors.Join(err, uerr)
	}
	return r.cfg.Store.DeleteOnboarding(ctx, x.row.ID)
}

func (r *Runner) undoSteps(ctx context.Context, x *rec) error {
	if r.cfg.AI != nil {
		if err := r.cfg.AI.StopOnboard(ctx, x.row.ID); err != nil {
			// Without the delete RBAC the Job ends at its deadline.
			r.event(ctx, x, apiv1.OnboardingEventKindLog, "hopai not stopped: "+err.Error(), nil)
		}
	}
	if x.doc.Internal.TestApp || x.doc.Check != nil {
		if r.cfg.Apps == nil {
			return ErrNotConfigured
		}
		if err := r.cfg.Apps.Remove(ctx, onboard.TestApp(x.row.ID)); err != nil {
			return fmt.Errorf("remove the test app: %w", err)
		}
	}
	if stateOrder[x.effectiveState()] <= stateOrder[store.OnboardingAwaitingApproval] {
		if x.doc.Pr != nil && (x.doc.Pr.Merged == nil || !*x.doc.Pr.Merged) && r.cfg.GitHub != nil {
			if err := r.cfg.GitHub.ClosePR(ctx, x.row.Repo, x.doc.Pr.Number); err != nil {
				return fmt.Errorf("close the PR: %w", err)
			}
		}
		return nil
	}
	// The registered app first, so nothing routes or deploys to what is
	// undone below.
	if x.doc.Internal.GatewayReady && r.cfg.Gateway != nil {
		var ae *gateway.APIError
		if err := r.cfg.Gateway.DeleteApp(ctx, x.row.App); err != nil && !(errors.As(err, &ae) && ae.Status == 404) {
			return fmt.Errorf("delete the gateway app: %w", err)
		}
	}
	if x.doc.Internal.Registered && r.cfg.Apps != nil {
		if err := r.cfg.Apps.Remove(ctx, x.row.App); err != nil {
			return fmt.Errorf("delete the app: %w", err)
		}
	}
	connected := false
	for i := len(x.doc.LegacyChanges) - 1; i >= 0; i-- {
		c := x.doc.LegacyChanges[i]
		if c.UndoneAt != nil {
			continue
		}
		var err error
		switch c.Kind {
		case apiv1.OnboardingLegacyChangeKindDbRouteCreated:
			if r.cfg.Routes == nil {
				return ErrNotConfigured
			}
			err = r.cfg.Routes.RemoveLegacy(ctx, x.row.App)
		case apiv1.OnboardingLegacyChangeKindWorkflowDisabled:
			id := r.workflowID(x, c.Target)
			if id == 0 || r.cfg.GitHub == nil {
				return fmt.Errorf("workflow %s cannot be enabled again", c.Target)
			}
			err = r.cfg.GitHub.EnableWorkflow(ctx, x.row.Repo, id)
		default: // the VM's: the connector undoes them together, in reverse
			if connected {
				continue
			}
			connected = true
			if r.cfg.Connect == nil {
				return ErrNotConfigured
			}
			var mine []apiv1.OnboardingLegacyChange
			for _, c := range x.doc.LegacyChanges {
				if c.UndoneAt == nil && c.Kind != apiv1.OnboardingLegacyChangeKindDbRouteCreated && c.Kind != apiv1.OnboardingLegacyChangeKindWorkflowDisabled {
					mine = append(mine, c)
				}
			}
			undone, derr := r.cfg.Connect.Disconnect(ctx, r.connectReq(x), mine)
			r.markUndone(ctx, x.row.ID, undone)
			if derr != nil {
				return fmt.Errorf("undo on the source: %w", derr)
			}
			continue
		}
		if err != nil {
			return fmt.Errorf("undo %s %s: %w", c.Kind, c.Target, err)
		}
		now := r.cfg.Now()
		r.markUndone(ctx, x.row.ID, []apiv1.OnboardingLegacyChange{{Kind: c.Kind, Target: c.Target, UndoneAt: &now}})
	}
	return nil
}

func (r *Runner) workflowID(x *rec, path string) int64 {
	if x.facts == nil || x.facts.GitHub == nil {
		return 0
	}
	for _, w := range x.facts.GitHub.Workflows {
		if strings.EqualFold(w.Path, path) {
			return w.ID
		}
	}
	return 0
}
