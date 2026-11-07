package apps

import (
	"context"
	"fmt"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

// OnboardTarget is the target side of an onboarding's cutover plan (ADR
// 0042): the registered app's database in env, its bucket ("" without an
// s3 resource) and gateway backend.
type OnboardTarget struct {
	Endpoint       apiv1.EnginePlanEndpoint
	Bucket         string
	GatewayBackend string
	GatewayURL     string // the app's gateway scope, "" without a gateway
}

// OnboardTarget returns app's endpoint in env as Draft would make it, and
// puts its password into the process environment like Draft.
func (c *Catalog) OnboardTarget(ctx context.Context, app, env string) (OnboardTarget, error) {
	if c.registry == nil {
		return OnboardTarget{}, fmt.Errorf("%w: no registry of apps", ErrInvalid)
	}
	a, d, ok, err := c.registry.app(ctx, app)
	if err != nil {
		return OnboardTarget{}, err
	}
	if !ok {
		return OnboardTarget{}, fmt.Errorf("%w: app %s has no database pair", ErrInvalid, app)
	}
	ep, e, err := c.endpoint(a, env, "target")
	if err != nil {
		return OnboardTarget{}, err
	}
	if err := c.setPassword(d, e.PasswordEnv); err != nil {
		return OnboardTarget{}, err
	}
	out := OnboardTarget{Endpoint: ep, Bucket: e.Bucket}
	if c.gatewayURL != "" {
		envCfg, _ := c.env(env)
		out.GatewayBackend, out.GatewayURL = envCfg.GatewayBackend, c.gatewayURL
	}
	return out, nil
}
