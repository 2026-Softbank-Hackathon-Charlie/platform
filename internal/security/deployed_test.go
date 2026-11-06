package security

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

type fakeRecords struct {
	deps  map[string][]store.Deployment
	err   error
	calls int
}

func (f *fakeRecords) ListInstanceDeployments(_ context.Context, instance string, _ int) ([]store.Deployment, error) {
	f.calls++
	return f.deps[instance], f.err
}

func deployment(id, env, status string, images ...store.DeploymentImage) store.Deployment {
	return store.Deployment{ID: id, Instance: "shop", Images: images, Environments: []store.DeploymentEnvironment{{Env: env, Status: status}}}
}

func runningIn(env, container, digest string, instance *string) apiv1.SecurityRunningImage {
	img := apiv1.SecurityRunningImage{Environment: env, Namespace: "shop", Workload: "Deployment/shop-" + container, Container: container, Image: "r/app:v1", Instance: instance}
	if digest != "" {
		img.Digest = &digest
	}
	return img
}

func TestCompareDeployed(t *testing.T) {
	d1, d2 := "sha256:"+strings.Repeat("1", 64), "sha256:"+strings.Repeat("2", 64)
	shop := "shop"
	recs := &fakeRecords{deps: map[string][]store.Deployment{"shop": {
		// Newest first: dep_new never reached gke, so dep_old is gke's deployment.
		deployment("dep_new", "eks", "healthy", store.DeploymentImage{Service: "web", Image: "ecr/web@" + d2}),
		deployment("dep_fail", "gke", "failed", store.DeploymentImage{Service: "web", Image: "ecr/web@" + d2}),
		deployment("dep_old", "gke", "degraded", store.DeploymentImage{Service: "web", Image: "ecr/web@" + d1}),
	}}}
	r := apiv1.SecurityVulnerabilityReport{RunningImages: []apiv1.SecurityRunningImage{
		runningIn("eks", "web", d2, &shop),    // match dep_new
		runningIn("gke", "web", d2, &shop),    // gke runs d2, but dep_old recorded d1
		runningIn("gke", "worker", d1, &shop), // no service worker
		runningIn("k3s", "web", d1, &shop),    // nothing reached k3s
		runningIn("eks", "web", "", &shop),    // no runtime digest
		runningIn("eks", "web", d2, nil),      // no instance label
	}}
	CompareDeployed(context.Background(), &r, recs)
	want := []struct {
		state apiv1.SecurityDeployedImageState
		dep   string
	}{
		{apiv1.SecurityDeployedImageStateMatch, "dep_new"},
		{apiv1.SecurityDeployedImageStateMismatch, "dep_old"},
		{apiv1.SecurityDeployedImageStateNoRecord, "dep_old"},
		{apiv1.SecurityDeployedImageStateNoRecord, ""},
		{apiv1.SecurityDeployedImageStateUnknown, "dep_new"},
		{apiv1.SecurityDeployedImageStateUnknown, ""},
	}
	for i, w := range want {
		d := r.RunningImages[i].Deployed
		if d == nil || d.State != w.state || d.Reason == nil {
			t.Fatalf("image %d: %+v, want %s", i, d, w.state)
		}
		if got := ""; d.DeploymentId != nil {
			got = *d.DeploymentId
			if got != w.dep {
				t.Errorf("image %d: deployment %s, want %s", i, got, w.dep)
			}
		} else if w.dep != "" {
			t.Errorf("image %d: no deployment, want %s", i, w.dep)
		}
	}
	if r.Summary.DriftedContainers == nil || *r.Summary.DriftedContainers != 1 {
		t.Fatalf("drifted: %v", r.Summary.DriftedContainers)
	}
	if recs.calls != 1 {
		t.Fatalf("read the records %d times, want once per instance", recs.calls)
	}
}

// Records that cannot be read make the comparison unknown, never a match.
func TestCompareDeployedUnreadable(t *testing.T) {
	shop := "shop"
	r := apiv1.SecurityVulnerabilityReport{RunningImages: []apiv1.SecurityRunningImage{runningIn("eks", "web", "sha256:"+strings.Repeat("1", 64), &shop)}}
	CompareDeployed(context.Background(), &r, &fakeRecords{err: errors.New("db down")})
	if d := r.RunningImages[0].Deployed; d.State != apiv1.SecurityDeployedImageStateUnknown || !strings.Contains(*d.Reason, "db down") {
		t.Fatalf("%+v", d)
	}
	// Without records nothing is compared and the field stays absent.
	r2 := apiv1.SecurityVulnerabilityReport{RunningImages: []apiv1.SecurityRunningImage{runningIn("eks", "web", "", &shop)}}
	CompareDeployed(context.Background(), &r2, nil)
	if r2.RunningImages[0].Deployed != nil || r2.Summary.DriftedContainers != nil {
		t.Fatalf("compared without records: %+v", r2)
	}
}
