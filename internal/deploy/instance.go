package deploy

import (
	"context"
	"errors"
)

// errInstancesNotReady is returned until the executor deploys added
// branches as their own instances (ADR 0015).
var errInstancesNotReady = errors.New("removing an added branch's instance is not available yet")

// RemoveInstance removes the instance of an added branch of an app (see
// api.AppRemover). The executor does not deploy added branches yet, so
// there is nothing to remove.
func (e *Executor) RemoveInstance(_ context.Context, _, _ string) error {
	return errInstancesNotReady
}
