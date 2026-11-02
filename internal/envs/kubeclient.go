package envs

import (
	"errors"
	"fmt"
	"io/fs"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// NewKubeClient builds a client-go clientset for env's kubeconfig and context,
// with the same loading rules as NewKubeProber: an explicit kubeconfig path or
// the default rules, and the context must exist. Every request is bounded by
// timeout. It never contacts the cluster; errors are short and name no file
// paths.
func NewKubeClient(env EnvConfig, timeout time.Duration) (kubernetes.Interface, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if env.Kubeconfig != "" {
		rules.ExplicitPath = env.Kubeconfig
	}
	raw, err := rules.Load()
	if errors.Is(err, fs.ErrNotExist) {
		return nil, errors.New("kubeconfig file not found")
	}
	if err != nil {
		return nil, errors.New("kubeconfig could not be read")
	}
	if _, ok := raw.Contexts[env.KubeContext]; !ok {
		return nil, fmt.Errorf("kube context %q not found in kubeconfig", env.KubeContext)
	}
	rc, err := clientcmd.NewNonInteractiveClientConfig(*raw, env.KubeContext, &clientcmd.ConfigOverrides{}, rules).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("kube context %q: invalid kubeconfig", env.KubeContext)
	}
	rc.UserAgent = userAgent
	rc.Timeout = timeout
	kube, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return nil, fmt.Errorf("kube context %q: invalid client configuration", env.KubeContext)
	}
	return kube, nil
}

// DescribeError turns a client-go error into a short message without request
// URLs, file paths or credentials, the way environment statuses report them.
func DescribeError(op string, err error) string { return describe(op, err) }
