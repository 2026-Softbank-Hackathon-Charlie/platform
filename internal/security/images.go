package security

import (
	"regexp"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

// Labels the platform's app chart sets on every workload (ADR 0015).
const (
	labelPartOf   = "app.kubernetes.io/part-of"
	labelInstance = "app.kubernetes.io/instance"
)

var digestRE = regexp.MustCompile(`@(sha256:[0-9a-f]{64})$`)

// ImageDigest returns the registry digest of a container: the repo digest
// in the status's imageID ("repo@sha256:..."), or else the digest the spec
// pins. A bare "sha256:..." imageID is the runtime's local image id, not a
// registry digest, and is not used. It returns "" when there is neither.
func ImageDigest(statusImageID, specImage string) string {
	if m := digestRE.FindStringSubmatch(strings.TrimPrefix(statusImageID, "docker-pullable://")); m != nil {
		return m[1]
	}
	if m := digestRE.FindStringSubmatch(specImage); m != nil {
		return m[1]
	}
	return ""
}

// runningImages records the app containers of a running pod, once per
// workload, container and digest: replicas of one version are one entry,
// and a rollout that runs two digests is two.
func (k *kubeChecks) runningImages(p *corev1.Pod, w string) {
	status := map[string]string{}
	for _, cs := range p.Status.ContainerStatuses {
		status[cs.Name] = cs.ImageID
	}
	var app, instance *string
	if a := p.Labels[labelPartOf]; a != "" {
		app = &a
		if in := p.Labels[labelInstance]; in == p.Namespace {
			instance = &in
		}
	}
	for _, c := range p.Spec.Containers {
		d := ImageDigest(status[c.Name], c.Image)
		key := w + "\x00" + c.Name + "\x00" + d
		if k.imageSeen[key] {
			continue
		}
		k.imageSeen[key] = true
		img := apiv1.SecurityRunningImage{Environment: k.env, Namespace: p.Namespace, Workload: strings.TrimPrefix(w, p.Namespace+"/"),
			Container: c.Name, Image: c.Image, App: app, Instance: instance, ScanState: apiv1.SecurityRunningImageScanStateNotScanned}
		if d != "" {
			img.Digest = &d
		} else {
			img.ScanState = apiv1.SecurityRunningImageScanStateUnknownDigest
		}
		k.images = append(k.images, img)
	}
}
