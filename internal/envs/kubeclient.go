package envs

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/metadata"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// NewKubeClient builds a client-go clientset for env's kubeconfig and context,
// with the same loading rules as NewKubeProber: an explicit kubeconfig path or
// the default rules, and the context must exist. Every request is bounded by
// timeout. It never contacts the cluster; errors are short and name no file
// paths.
func NewKubeClient(env EnvConfig, timeout time.Duration) (kubernetes.Interface, error) {
	rc, err := kubeRESTConfig(env, timeout)
	if err != nil {
		return nil, err
	}
	kube, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return nil, errors.New("invalid kube client configuration")
	}
	return kube, nil
}

// NewReadClients uses the same environment binding and timeout as NewKubeClient.
// Metadata negotiation is used for Secret existence; the dynamic client is for
// non-Secret resources only. The clients themselves do not restrict RBAC rights.
func NewReadClients(env EnvConfig, timeout time.Duration) (metadata.Interface, dynamic.Interface, error) {
	rc, err := kubeRESTConfig(env, timeout)
	if err != nil {
		return nil, nil, err
	}
	hc, err := rest.HTTPClientFor(rc)
	if err != nil {
		return nil, nil, errors.New("invalid kube client configuration")
	}
	// client-go's metadata client normally advertises a full-object JSON
	// fallback. Strip that fallback before the request leaves the process.
	metadataHTTP := *hc
	metadataHTTP.Transport = metadataOnlyTransport{base: hc.Transport}
	meta, err := metadata.NewForConfigAndClient(rc, &metadataHTTP)
	if err != nil {
		return nil, nil, errors.New("invalid metadata client configuration")
	}
	dyn, err := dynamic.NewForConfigAndClient(rc, hc)
	if err != nil {
		return nil, nil, errors.New("invalid dynamic client configuration")
	}
	return meta, dyn, nil
}

func kubeRESTConfig(env EnvConfig, timeout time.Duration) (*rest.Config, error) {
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
	return rc, nil
}

type metadataOnlyTransport struct{ base http.RoundTripper }

func (t metadataOnlyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	r.Header.Set("Accept", "application/json;as=PartialObjectMetadata;g=meta.k8s.io;v=v1")
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	response, err := base.RoundTrip(r)
	if err != nil || response.StatusCode < 200 || response.StatusCode >= 300 {
		return response, err
	}
	// Validate before client-go can silently convert a full object response.
	// Kubernetes normally honors negotiation; a non-conforming server is unknown.
	const maxMetadata = 1 << 20
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxMetadata+1))
	response.Body.Close()
	var typ metav1.TypeMeta
	if err != nil || len(raw) > maxMetadata || json.Unmarshal(raw, &typ) != nil || typ.APIVersion != "meta.k8s.io/v1" || typ.Kind != "PartialObjectMetadata" {
		return nil, errors.New("metadata response unavailable or invalid")
	}
	response.Body = io.NopCloser(bytes.NewReader(raw))
	return response, nil
}

// DescribeError turns a client-go error into a short message without request
// URLs, file paths or credentials, the way environment statuses report them.
func DescribeError(op string, err error) string { return describe(op, err) }
