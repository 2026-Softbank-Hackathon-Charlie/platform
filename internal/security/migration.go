package security

import (
	"context"
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

// AssessMigrationPrivileged reads current managed app pods for HC-POL-02.
func AssessMigrationPrivileged(ctx context.Context, kube kubernetes.Interface, namespace, app string) apiv1.RunSecurityAssessment {
	a := apiv1.RunSecurityAssessment{
		PolicyVersion: PolicyVersion,
		Result:        apiv1.SecurityControlResultUnknown,
		Reason:        apiv1.RunSecurityAssessmentReasonUnavailable,
		Workload:      "Application workload",
	}
	if len(validation.IsDNS1123Label(namespace)) != 0 || len(validation.IsDNS1123Label(app)) != 0 {
		return a
	}
	a.Workload = namespace + " / App/" + app
	if kube == nil {
		return a
	}

	ctx, cancel := context.WithTimeout(ctx, KubeRequestTimeout)
	defer cancel()
	pods, err := listPods(ctx, kube, namespace, "hopcharlie.dev/app="+app+",hopcharlie.dev/service,!cnpg.io/cluster")
	if err != nil {
		if apierrors.IsForbidden(err) {
			a.Reason = apiv1.RunSecurityAssessmentReasonForbidden
			a.Evidence = ptrTo("Kubernetes API: list pods · 403 Forbidden")
		}
		return a
	}

	var checked, ready int
	var soleWorkload, violation string
	for i := range pods {
		p := &pods[i]
		_, database := p.Labels["cnpg.io/cluster"]
		phase := p.Status.Phase
		if p.Namespace != namespace || p.Labels["hopcharlie.dev/app"] != app || p.Labels["hopcharlie.dev/service"] == "" ||
			database || p.DeletionTimestamp != nil || (phase != corev1.PodRunning && phase != corev1.PodPending) {
			continue
		}
		if phase == corev1.PodRunning && slices.ContainsFunc(p.Status.Conditions, func(c corev1.PodCondition) bool {
			return c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue
		}) {
			ready++
		}
		w := workload(p)
		deployment, known := strings.CutPrefix(w, "Deployment/")
		if !known || len(validation.IsDNS1123Subdomain(deployment)) != 0 {
			w = ""
		}
		if checked == 0 {
			soleWorkload = w
		} else if soleWorkload != w {
			soleWorkload = ""
		}
		checked++
		if violation == "" {
			for _, c := range containers(&p.Spec) {
				if c.sc != nil && isTrue(c.sc.Privileged) {
					violation = fmt.Sprintf("Pod/%s · %s · securityContext.privileged: true", p.Name, c.name)
					break
				}
			}
		}
	}
	if soleWorkload != "" {
		a.Workload = namespace + " / " + soleWorkload
	}
	switch {
	case violation != "":
		a.Result = apiv1.SecurityControlResultFail
		a.Reason = apiv1.RunSecurityAssessmentReasonPrivileged
		a.Evidence = &violation
	case ready == 0:
		a.Reason = apiv1.RunSecurityAssessmentReasonWorkloadNotReady
		a.Evidence = ptrTo("0 Ready and running application Pods in scope")
	default:
		a.Result = apiv1.SecurityControlResultPass
		a.Reason = apiv1.RunSecurityAssessmentReasonClear
		a.Evidence = ptrTo(fmt.Sprintf("%s inspected · %d Ready and running", many(checked, "active application Pod", "active application Pods"), ready))
	}
	return a
}
