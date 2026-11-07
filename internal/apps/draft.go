package apps

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/plans"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

// ErrInvalid means the draft request does not fit the configuration.
var ErrInvalid = errors.New("invalid draft request")

// Draft builds the engine plan that moves app req.App from req.Source to
// req.Target. Nothing is stored; the caller submits the plan like any other.
//
//   - id is <app>-<source>-to-<target>
//   - context is each environment's kubeContext; namespace, cluster and DSN come from the app
//   - the DSN reads the password as ${passwordEnv}, expanded on the server at start
//   - files are moved when both environments have a bucket, unless req.Files is false
//   - the gateway is the app's scope in the platform gateway, <gateway>/apps/<app>
//     (mode http), when the server has one, otherwise none
//
// An app that is not in the apps file may be a registered app with a
// database pair (WithRegistry, ADR 0030): its DSNs go to the hosts of the
// host templates, its files move like an apps file app's when the server
// moves registered apps' files (Registry.BucketPrefixes, ADR 0032), and its
// password is put into the server's environment now, for the draft's
// source measurement.
func (c *Catalog) Draft(ctx context.Context, req apiv1.PlanDraftRequest) (apiv1.EnginePlan, error) {
	a, ok := c.app(req.App)
	var pair *store.AppDatabase
	if !ok && c.registry != nil {
		ra, d, found, err := c.registry.app(ctx, req.App)
		if err != nil {
			return apiv1.EnginePlan{}, err
		}
		a, ok, pair = ra, found, &d
	}
	if !ok {
		return apiv1.EnginePlan{}, fmt.Errorf("%w: unknown app %q", ErrInvalid, req.App)
	}
	if req.Source == req.Target {
		return apiv1.EnginePlan{}, fmt.Errorf("%w: source and target are both %q", ErrInvalid, req.Source)
	}
	src, srcApp, err := c.endpoint(a, req.Source, "source")
	if err != nil {
		return apiv1.EnginePlan{}, err
	}
	dst, dstApp, err := c.endpoint(a, req.Target, "target")
	if err != nil {
		return apiv1.EnginePlan{}, err
	}

	id := fmt.Sprintf("%s-%s-to-%s", a.Name, req.Source, req.Target)
	if len(id) > 63 {
		return apiv1.EnginePlan{}, fmt.Errorf("%w: plan id %q is longer than 63 characters", ErrInvalid, id)
	}
	p := apiv1.EnginePlan{Id: id, Source: src, Target: dst}
	if pair != nil && len(a.Environments) > 2 {
		p.AllowSourceRewire = ptr(true)
	}

	wantFiles := req.Files == nil || *req.Files
	switch {
	case wantFiles && srcApp.Bucket != "" && dstApp.Bucket != "":
		f := alloc(&p.Files)
		f.Source, f.Target = srcApp.Bucket, dstApp.Bucket
	case wantFiles && (a.filesRequired || (req.Files != nil && *req.Files)):
		return apiv1.EnginePlan{}, fmt.Errorf("%w: files needs a bucket for %s in both %s and %s", ErrInvalid, a.Name, req.Source, req.Target)
	}

	g := alloc(&p.Gateway)
	if c.gatewayURL != "" {
		srcEnv, _ := c.env(req.Source)
		dstEnv, _ := c.env(req.Target)
		g.Mode = ptr(apiv1.EnginePlanGatewayModeHttp)
		// The app's own scope in the gateway (ADR 0012): the root would hold
		// and route the gateway's default app instead.
		g.Url, g.Token = ptr(plans.GatewayAppURL(c.gatewayURL, a.Name)), ptr("${GATEWAY_TOKEN}")
		g.SourceBackend, g.TargetBackend = ptr(srcEnv.GatewayBackend), ptr(dstEnv.GatewayBackend)
	} else {
		g.Mode = ptr(apiv1.EnginePlanGatewayModeNone)
	}

	if len(a.Database.Schemas) > 0 || req.WrittenRecordsStream != nil {
		v := alloc(&p.Verify)
		if len(a.Database.Schemas) > 0 {
			s := append([]string(nil), a.Database.Schemas...)
			v.Schemas = &s
		}
		if req.WrittenRecordsStream != nil {
			name := *req.WrittenRecordsStream
			if !plans.StreamName.MatchString(name) {
				return apiv1.EnginePlan{}, fmt.Errorf("%w: writtenRecordsStream %q must match %s", ErrInvalid, name, plans.StreamName)
			}
			v.WrittenRecords = ptr("stream:" + name)
		}
	}
	if pair != nil {
		for _, e := range []Environment{srcApp, dstApp} {
			if err := c.setPassword(*pair, e.PasswordEnv); err != nil {
				return apiv1.EnginePlan{}, err
			}
		}
	}
	return p, nil
}

func (c *Catalog) endpoint(a App, envName, role string) (apiv1.EnginePlanEndpoint, Environment, error) {
	env, ok := c.env(envName)
	if !ok {
		return apiv1.EnginePlanEndpoint{}, Environment{}, fmt.Errorf("%w: unknown %s environment %q", ErrInvalid, role, envName)
	}
	e, ok := a.Environments[envName]
	if !ok {
		return apiv1.EnginePlanEndpoint{}, Environment{}, fmt.Errorf("%w: app %q is not configured in environment %q", ErrInvalid, a.Name, envName)
	}
	dsn := fmt.Sprintf("postgres://%s:${%s}@%s/%s?sslmode=require", a.Database.User, e.PasswordEnv, hostPort(e), a.Database.Name)
	return apiv1.EnginePlanEndpoint{
		Env:       envName,
		Context:   ptr(env.KubeContext),
		Namespace: cmp.Or(e.Namespace, a.Namespace),
		Cluster:   e.CnpgCluster,
		Dsn:       dsn,
	}, e, nil
}

func hostPort(e Environment) string { return net.JoinHostPort(e.DBHost, strconv.Itoa(e.DBPort)) }

func ptr[T any](v T) *T { return &v }

// alloc sets *p to a new zero value and returns it. The generated plan type
// nests anonymous structs, which have no name to write a literal with.
func alloc[T any](p **T) *T {
	*p = new(T)
	return *p
}
