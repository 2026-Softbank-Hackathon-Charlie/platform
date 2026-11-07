package job

import (
	"context"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/onboard/connect"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/onboard/discover"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

// FromDiscoverer is the Discoverer of internal/onboard/discover.
func FromDiscoverer(d *discover.Discoverer) Discoverer { return discoverer{d} }

type discoverer struct{ d *discover.Discoverer }

func (a discoverer) Discover(ctx context.Context, req DiscoverRequest) (Discovered, error) {
	res, err := a.d.Discover(ctx, discover.Input{ID: req.ID, Repo: req.Repo, ServiceURL: req.ServiceURL, TargetEnv: req.TargetEnv})
	if err != nil {
		return Discovered{}, err
	}
	out := Discovered{Facts: res.Facts, HumanActions: res.HumanActions, KeyVersion: res.KeyVersion, KeyCheck: res.KeyCheck}
	if s := res.Secrets; s != nil {
		out.Secrets = Sealed{s.Nonce, s.Ciphertext}
	}
	if s := res.SuperuserPassword; s != nil {
		out.SuperuserPassword = Sealed{s.Nonce, s.Ciphertext}
	}
	return out, nil
}

// FromConnector is the Connector of internal/onboard/connect; stop stops
// the instance (EC2 StopInstances, which connect does not do).
func FromConnector(c *connect.Connector, stop func(ctx context.Context, req ConnectRequest) error) Connector {
	return connector{c, stop}
}

type connector struct {
	c    *connect.Connector
	stop func(ctx context.Context, req ConnectRequest) error
}

func (a connector) Connect(ctx context.Context, req ConnectRequest) ([]apiv1.OnboardingLegacyChange, Sealed, error) {
	res, err := a.c.Connect(ctx, connect.Input{ID: req.ID, Facts: req.Facts})
	var pw Sealed
	if s := res.ReplicationPassword; s != nil {
		pw = Sealed{s.Nonce, s.Ciphertext}
	}
	return res.Changes, pw, err
}

// Disconnect undoes everything connect did on the VM; connect does not
// undo a part, so decommissioning's removal of the device takes the rest
// with it, which is harmless on an instance about to stop.
func (a connector) Disconnect(ctx context.Context, req ConnectRequest, _ []apiv1.OnboardingLegacyChange) ([]apiv1.OnboardingLegacyChange, error) {
	return a.c.Disconnect(ctx, connect.Input{ID: req.ID, Facts: req.Facts})
}

func (a connector) StopInstance(ctx context.Context, req ConnectRequest) error {
	if a.stop == nil {
		return ErrNotConfigured
	}
	return a.stop(ctx, req)
}
