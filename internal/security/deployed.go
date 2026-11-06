package security

import (
	"context"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

// DeploymentRecords reads the platform's deployments of an instance, newest
// first (store.Store).
type DeploymentRecords interface {
	ListInstanceDeployments(ctx context.Context, instance string, limit int) ([]store.Deployment, error)
}

// deploymentLookback is how many deployments of an instance are searched for
// the latest one that reached an environment.
const deploymentLookback = 50

// CompareDeployed sets deployed on each running image of r: whether the
// running digest is the one the latest deployment that reached the
// environment recorded for the service named like the container (SEC-05). It
// reads each instance's deployments once and sets summary.driftedContainers.
func CompareDeployed(ctx context.Context, r *apiv1.SecurityVulnerabilityReport, recs DeploymentRecords) {
	if recs == nil {
		return
	}
	type listed struct {
		deps []store.Deployment
		err  error
	}
	cache := map[string]listed{}
	drifted := 0
	for i := range r.RunningImages {
		img := &r.RunningImages[i]
		var got listed
		if img.Instance != nil {
			var ok bool
			if got, ok = cache[*img.Instance]; !ok {
				got.deps, got.err = recs.ListInstanceDeployments(ctx, *img.Instance, deploymentLookback)
				cache[*img.Instance] = got
			}
		}
		d := compareOne(*img, got.deps, got.err)
		img.Deployed = &d
		if d.State == apiv1.SecurityDeployedImageStateMismatch {
			drifted++
		}
	}
	r.Summary.DriftedContainers = &drifted
}

func compareOne(img apiv1.SecurityRunningImage, deps []store.Deployment, err error) apiv1.SecurityDeployedImage {
	out := func(state apiv1.SecurityDeployedImageState, reason string) apiv1.SecurityDeployedImage {
		return apiv1.SecurityDeployedImage{State: state, Reason: &reason}
	}
	switch {
	case img.Instance == nil:
		return out(apiv1.SecurityDeployedImageStateUnknown, "the pod has no instance label of the app chart, so no deployment record applies")
	case err != nil:
		return out(apiv1.SecurityDeployedImageStateUnknown, "the deployment records could not be read: "+shorten(err.Error(), 160))
	}
	dep, ok := reached(deps, img.Environment)
	if !ok {
		return out(apiv1.SecurityDeployedImageStateNoRecord, "no deployment of instance "+*img.Instance+" reached "+img.Environment+" among the latest records")
	}
	var recorded string
	for _, di := range dep.Images {
		if di.Service == img.Container {
			recorded = di.Image
		}
	}
	if recorded == "" {
		d := out(apiv1.SecurityDeployedImageStateNoRecord, "deployment "+dep.ID+" has no service named "+img.Container)
		d.DeploymentId = &dep.ID
		return d
	}
	want := ImageDigest("", recorded)
	d := apiv1.SecurityDeployedImage{DeploymentId: &dep.ID, Image: &recorded}
	if want != "" {
		d.Digest = &want
	}
	var reason string
	switch {
	case want == "":
		d.State, reason = apiv1.SecurityDeployedImageStateUnknown, "the recorded image has no digest"
	case img.Digest == nil:
		d.State, reason = apiv1.SecurityDeployedImageStateUnknown, "the runtime reported no digest for the container"
	case *img.Digest == want:
		d.State, reason = apiv1.SecurityDeployedImageStateMatch, "the running digest is the one deployment "+dep.ID+" recorded"
	default:
		d.State = apiv1.SecurityDeployedImageStateMismatch
		reason = "the running digest differs from the one deployment " + dep.ID + " recorded: a rollout in progress, a change outside the platform, or an image index and one of its platform manifests"
	}
	d.Reason = &reason
	return d
}

// reached is the newest deployment whose environment env is healthy or
// degraded (synced to the cluster).
func reached(deps []store.Deployment, env string) (store.Deployment, bool) {
	for _, d := range deps {
		for _, e := range d.Environments {
			if e.Env == env && (e.Status == string(apiv1.DeploymentEnvironmentStatusHealthy) || e.Status == string(apiv1.DeploymentEnvironmentStatusDegraded)) {
				return d, true
			}
		}
	}
	return store.Deployment{}, false
}
