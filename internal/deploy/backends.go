package deploy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/appspec"
)

// Backends finds where the public service of an app's instance answers in an
// environment, for the traffic gateway. The instance is the namespace; the
// Service is named after the app (ServiceName).
type Backends interface {
	URL(ctx context.Context, app, instance, env string, svc appspec.Service) (string, error)
}

// TemplateBackends builds the URL from a template with {app}, {instance},
// {service}, {port} and {env}, e.g.
// http://{app}-{service}.{instance}.svc:{port} when the gateway runs in the
// same cluster as the app. {instance} is the app name for the default
// instance.
type TemplateBackends struct {
	Template string
}

func (t TemplateBackends) URL(_ context.Context, app, instance, env string, svc appspec.Service) (string, error) {
	return strings.NewReplacer("{app}", app, "{instance}", instance, "{service}", svc.Name, "{port}", strconv.Itoa(svc.Port), "{env}", env).Replace(t.Template), nil
}

// EnvBackends builds the URL from the template of the environment, and asks
// Fallback for an environment that has none, so one environment can move to
// a shared entry while the others keep their load balancers.
type EnvBackends struct {
	Templates map[string]TemplateBackends
	Fallback  Backends
}

func (b EnvBackends) URL(ctx context.Context, app, instance, env string, svc appspec.Service) (string, error) {
	if t, ok := b.Templates[env]; ok {
		return t.URL(ctx, app, instance, env, svc)
	}
	if b.Fallback == nil {
		return "", fmt.Errorf("no backend template for environment %s", env)
	}
	return b.Fallback.URL(ctx, app, instance, env, svc)
}

// envName is an environment name as PLATFORM_ENVS_FILE allows it.
var envName = regexp.MustCompile(`^[a-z0-9-]+$`)

// ParseBackendTemplates reads PLATFORM_APP_BACKEND_TEMPLATE. The value is
// either one template for every environment (the first entry has no
// "<env>=" in front) or a comma-separated list of <env>=<template>, one per
// environment that uses a template; perEnv is nil for the first form.
func ParseBackendTemplates(v string) (all string, perEnv map[string]string, err error) {
	entries := strings.Split(v, ",")
	if name, _, ok := strings.Cut(entries[0], "="); !ok || !envName.MatchString(strings.TrimSpace(name)) {
		return v, nil, nil
	}
	perEnv = map[string]string{}
	for _, e := range entries {
		name, t, ok := strings.Cut(strings.TrimSpace(e), "=")
		if !ok || !envName.MatchString(name) {
			return "", nil, fmt.Errorf("%q is not <env>=<template>", e)
		}
		if _, dup := perEnv[name]; dup {
			return "", nil, fmt.Errorf("environment %s has two templates", name)
		}
		if !strings.HasPrefix(t, "http://") && !strings.HasPrefix(t, "https://") {
			return "", nil, fmt.Errorf("the template of %s must start with http:// or https://", name)
		}
		perEnv[name] = t
	}
	return "", perEnv, nil
}

// ServiceBackends reads the Service <app>-<service> in namespace <instance>
// of the environment's cluster and uses its load balancer's hostname or IP,
// with the Service port that targets the service's port.
type ServiceBackends struct {
	// Kube returns the client of an environment's cluster.
	Kube func(env string) (kubernetes.Interface, error)

	mu      sync.Mutex
	clients map[string]kubernetes.Interface
}

func (s *ServiceBackends) URL(ctx context.Context, app, instance, env string, svc appspec.Service) (string, error) {
	kube, err := s.client(env)
	if err != nil {
		return "", err
	}
	ns, name := Namespace(instance), ServiceName(app, svc.Name)
	got, err := kube.CoreV1().Services(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("read Service %s/%s: %w", ns, name, err)
	}
	var host string
	for _, in := range got.Status.LoadBalancer.Ingress {
		if host = in.Hostname; host == "" {
			host = in.IP
		}
		if host != "" {
			break
		}
	}
	if host == "" {
		return "", fmt.Errorf("Service %s/%s has no load balancer address yet", ns, name)
	}
	if len(got.Spec.Ports) == 0 {
		return "", fmt.Errorf("Service %s/%s has no ports", ns, name)
	}
	port := got.Spec.Ports[0].Port
	for _, p := range got.Spec.Ports {
		if int(p.Port) == svc.Port || p.TargetPort.IntValue() == svc.Port {
			port = p.Port
			break
		}
	}
	if port == 80 {
		return "http://" + host, nil
	}
	return "http://" + net.JoinHostPort(host, strconv.Itoa(int(port))), nil
}

// probeTimeout bounds one ProbeBackend request; a var for the tests.
var probeTimeout = 3 * time.Second

// probeClient sends ProbeBackend's requests the way the gateway sends its
// own (HTTP_PROXY included), on a new connection each time, as a load
// balancer's addresses change while it comes up, and without following
// redirects.
var probeClient = &http.Client{
	Transport: func() http.RoundTripper {
		t := http.DefaultTransport.(*http.Transport).Clone()
		t.DisableKeepAlives = true
		return t
	}(),
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// ProbeBackend checks once whether a public service answers at its backend
// URL, and is Config.Probe's default. With a healthCheck path it GETs that
// path and wants a 2xx or 3xx. Without one it GETs / and takes any answer
// but 502, 503 and 504, which a proxy on the way gives when nothing answers
// behind it; when that request gets no answer in time, a TCP connection to
// the server is enough (it is up, only slow at /). The error says briefly
// why, in the same words while the cause stays the same.
func ProbeBackend(ctx context.Context, backendURL, healthCheck string) error {
	path := healthCheck
	if path == "" {
		path = "/"
	}
	reqCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, strings.TrimSuffix(backendURL, "/")+path, nil)
	if err != nil {
		return err
	}
	resp, err := probeClient.Do(req)
	if err != nil {
		if healthCheck == "" && ctx.Err() == nil && isTimeout(err) && dials(ctx, req.URL) {
			return nil
		}
		return errors.New(probeReason(err))
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	resp.Body.Close()
	switch c := resp.StatusCode; {
	case healthCheck != "" && (c < 200 || c > 399):
		return fmt.Errorf("%s answered %s", healthCheck, resp.Status)
	case healthCheck == "" && (c == http.StatusBadGateway || c == http.StatusServiceUnavailable || c == http.StatusGatewayTimeout):
		return fmt.Errorf("/ answered %s", resp.Status)
	}
	return nil
}

// dials reports whether a TCP connection to u's host and port opens.
func dials(ctx context.Context, u *url.URL) bool {
	port := u.Port()
	if port == "" {
		port = "80"
		if u.Scheme == "https" {
			port = "443"
		}
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	c, err := (&net.Dialer{}).DialContext(ctx, "tcp", net.JoinHostPort(u.Hostname(), port))
	if err != nil {
		return false
	}
	c.Close()
	return true
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// probeReason says why a probe got no answer, leaving out the addresses,
// which change from one try to the next: an error it does not know is
// given by its innermost system call or network error.
func probeReason(err error) string {
	var dns *net.DNSError
	var sys *os.SyscallError
	var op *net.OpError
	var ue *url.Error
	switch {
	case errors.As(err, &dns):
		return "its name does not resolve: " + dns.Err
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection refused"
	case errors.Is(err, syscall.ECONNRESET), errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return "the connection closed without an answer"
	case isTimeout(err):
		return fmt.Sprintf("no answer within %s", probeTimeout)
	case errors.As(err, &sys):
		return sys.Err.Error()
	case errors.As(err, &op):
		return op.Err.Error()
	case errors.As(err, &ue):
		return ue.Err.Error()
	}
	return err.Error()
}

func (s *ServiceBackends) client(env string) (kubernetes.Interface, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.clients[env]; ok {
		return c, nil
	}
	if s.Kube == nil {
		return nil, errors.New("no kube client for " + env)
	}
	c, err := s.Kube(env)
	if err != nil {
		return nil, err
	}
	if s.clients == nil {
		s.clients = map[string]kubernetes.Interface{}
	}
	s.clients[env] = c
	return c, nil
}
