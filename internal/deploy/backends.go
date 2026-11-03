package deploy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/appspec"
)

// Backends finds where an app's public service answers in an environment,
// for the traffic gateway.
type Backends interface {
	URL(ctx context.Context, app, env string, svc appspec.Service) (string, error)
}

// TemplateBackends builds the URL from a template with {app}, {service},
// {port} and {env}, e.g. http://{app}-{service}.{app}.svc:{port} when the
// gateway runs in the same cluster as the app.
type TemplateBackends struct {
	Template string
}

func (t TemplateBackends) URL(_ context.Context, app, env string, svc appspec.Service) (string, error) {
	return strings.NewReplacer("{app}", app, "{service}", svc.Name, "{port}", strconv.Itoa(svc.Port), "{env}", env).Replace(t.Template), nil
}

// ServiceBackends reads the Service <app>-<service> in namespace <app> of the
// environment's cluster and uses its load balancer's hostname or IP, with the
// Service port that targets the service's port.
type ServiceBackends struct {
	// Kube returns the client of an environment's cluster.
	Kube func(env string) (kubernetes.Interface, error)

	mu      sync.Mutex
	clients map[string]kubernetes.Interface
}

func (s *ServiceBackends) URL(ctx context.Context, app, env string, svc appspec.Service) (string, error) {
	kube, err := s.client(env)
	if err != nil {
		return "", err
	}
	name := ServiceName(app, svc.Name)
	got, err := kube.CoreV1().Services(app).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("read Service %s/%s: %w", app, name, err)
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
		return "", fmt.Errorf("Service %s/%s has no load balancer address yet", app, name)
	}
	if len(got.Spec.Ports) == 0 {
		return "", fmt.Errorf("Service %s/%s has no ports", app, name)
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
