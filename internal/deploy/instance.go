package deploy

import (
	"context"
	"fmt"
	"slices"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
)

// RemoveInstance removes the instance of a branch added to an app (see
// api.AppRemover): it supersedes the instance's unfinished deployments,
// commits the removal of apps/<instance>/ from the gitops repository,
// removes the instance from the traffic gateway and deletes the branch, with
// the instance's deployments, from the store. Each step can be repeated, so a
// failed removal is retried by calling RemoveInstance again. A missing app is
// store.ErrNotFound; a branch the app has not added (its own branch
// included) is store.ErrInstanceNotFound, which matches store.ErrNotFound
// too. Otherwise the errors are RemoveApp's: ErrWritesHeld, ErrGitOps and
// ErrGateway, and the branch is then kept.
func (e *Executor) RemoveInstance(ctx context.Context, app, branch string) error {
	if _, err := e.cfg.Store.GetApp(ctx, app); err != nil {
		return err
	}
	branches, err := e.cfg.Store.ListAppBranches(ctx, app)
	if err != nil {
		return fmt.Errorf("list the branches of %s: %w", app, err)
	}
	i := slices.IndexFunc(branches, func(b store.AppBranch) bool { return b.Branch == branch })
	if i < 0 {
		return fmt.Errorf("app %s has no added branch %q: %w", app, branch, store.ErrInstanceNotFound)
	}
	instance := branches[i].Instance
	list, err := e.cfg.Store.ListInstanceDeployments(ctx, instance, 0)
	if err != nil {
		return fmt.Errorf("list deployments of %s: %w", instance, err)
	}
	if err := e.supersedeAll(ctx, list, "the instance is being deleted"); err != nil {
		return err
	}
	if err := e.removeInstances(ctx, []string{instance}, "delete instance "+instance); err != nil {
		return err
	}
	if err := e.cfg.Store.DeleteAppBranch(ctx, app, branch); err != nil {
		return err
	}
	e.cfg.Log.Info("instance deleted", "app", app, "branch", branch, "instance", instance)
	return nil
}
