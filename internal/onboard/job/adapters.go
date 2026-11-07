package job

import (
	"context"
	"errors"
	"fmt"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/onboard/connect"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/onboard/discover"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/onboard/vm"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/onboard/facts"
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

func (a connector) Connect(ctx context.Context, req ConnectRequest) (Connected, error) {
	res, err := a.c.Connect(ctx, connect.Input{ID: req.ID, Facts: req.Facts})
	out := Connected{Rotated: res.PasswordRotated}
	for _, c := range res.Changes {
		out.Changes = append(out.Changes, c.OnboardingLegacyChange)
	}
	if s := res.ReplicationPassword; s != nil {
		out.ReplicationPassword = Sealed{s.Nonce, s.Ciphertext}
	}
	var se *vm.StepError
	if errors.As(err, &se) && se.ExitCode == ConnectExitPendingConfig {
		err = &Blocker{Code: facts.BlockPendingConfig, Message: "the source database has settings that wait for a restart or a configuration file with errors, so connect cannot reload it safely; restart or fix the database container, then resume: " + se.Message}
	}
	return out, err
}

// The exit codes of the VM scripts the job reads (infra HopCharlie-Connect).
const (
	ConnectExitPendingConfig = 8 // connect.sh: settings wait for a restart
	DisconnectExitDBStopped  = 4 // disconnect.sh: the database container is stopped
)

// Disconnect undoes everything connect did on the VM; connect does not
// undo a part, so decommissioning's removal of the device takes the rest
// with it, which is harmless on an instance about to stop.
func (a connector) Disconnect(ctx context.Context, req ConnectRequest, _ []apiv1.OnboardingLegacyChange) ([]apiv1.OnboardingLegacyChange, error) {
	out, err := a.c.Disconnect(ctx, connect.Input{ID: req.ID, Facts: req.Facts})
	var se *vm.StepError
	if errors.As(err, &se) && se.ExitCode == DisconnectExitDBStopped {
		err = fmt.Errorf("incomplete: the source database container is stopped, so its replication account, hba and wal_keep_size lines were not removed; start it and delete again: %w", err)
	}
	return out, err
}

func (a connector) StopInstance(ctx context.Context, req ConnectRequest) error {
	if a.stop == nil {
		return ErrNotConfigured
	}
	return a.stop(ctx, req)
}
