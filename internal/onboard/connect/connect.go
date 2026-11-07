// Package connect is the source side of an onboarding's connecting state
// (ADR 0042, "3. 승인과 원본 연결", steps 2 to 4) and its undoing for
// DELETE: tag the instance, mint a Tailscale auth key, run
// HopCharlie-Connect, and seal the replication password it returns.
// Disabling the workflows (step 1) and the management cluster's route
// (step 5) are the onboarding job's.
package connect

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/onboard"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/onboard/vm"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/secrets"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/onboard/facts"
)

// Tagger is the part of the EC2 client that tags the instance.
type Tagger interface {
	CreateTags(ctx context.Context, in *ec2.CreateTagsInput, opts ...func(*ec2.Options)) (*ec2.CreateTagsOutput, error)
	DeleteTags(ctx context.Context, in *ec2.DeleteTagsInput, opts ...func(*ec2.Options)) (*ec2.DeleteTagsOutput, error)
}

// VM runs the documents on an instance (vm.Runner of the instance's
// region).
type VM interface {
	Connect(ctx context.Context, instance string, p vm.ConnectParams) (vm.Connected, error)
	Disconnect(ctx context.Context, instance, dbContainer, app string) ([]apiv1.OnboardingLegacyChange, error)
}

// Connector connects and disconnects a source. EC2 and VM take the
// instance's region.
type Connector struct {
	EC2       func(region string) Tagger
	VM        func(region string) VM
	Tailscale Tailnet
	Cipher    *secrets.Cipher
	Now       func() time.Time
}

// Input is the onboarding: its id and the facts discovery wrote.
type Input struct {
	ID    string
	Facts facts.Facts
}

// Result is what Connect did.
type Result struct {
	// Changes are in the order done, once per kind and target, for the
	// row's legacyChanges; on an error they are what was done before it.
	// New false is a change an earlier run made (the row has it already,
	// unless that run's output was lost).
	Changes []vm.Change
	// ReplicationPassword is hopcharlie_replicator's password sealed for
	// the row (secrets.OnboardingReplicationPassword), with KeyVersion and
	// KeyCheck. Every run sets a new one; PasswordRotated says the role
	// existed, so a Secret made from the old password must be replaced.
	ReplicationPassword *Sealed
	PasswordRotated     bool
	KeyVersion          int
	KeyCheck            string
}

// add appends a change unless one of the same kind and target is there.
func (r *Result) add(c vm.Change) {
	for _, x := range r.Changes {
		if x.Kind == c.Kind && x.Target == c.Target {
			return
		}
	}
	r.Changes = append(r.Changes, c)
}

// Sealed is a nonce and ciphertext pair.
type Sealed struct{ Nonce, Ciphertext []byte }

// params checks the facts connecting needs and returns the document's
// parameters but the auth key.
func params(f facts.Facts) (region, instance string, p vm.ConnectParams, err error) {
	if f.Instance == nil || f.Instance.Cloud != facts.CloudAWS || f.Instance.ID == "" {
		return "", "", p, errors.New("connect: the facts have no AWS instance")
	}
	if f.VM == nil || f.DB == nil || f.App == "" {
		return "", "", p, errors.New("connect: the facts have no inspected VM and database")
	}
	// The network both the database and its service name are on: the
	// project's default network first.
	var dbNets []string
	for _, c := range f.VM.Containers {
		if c.Name == f.DB.Container {
			dbNets = c.Networks
		}
	}
	var net *facts.Network
	for i, n := range f.VM.Networks {
		if !slices.Contains(dbNets, n.Name) || n.Subnet == "" {
			continue
		}
		if net == nil || n.Name == f.VM.Project+"_default" {
			net = &f.VM.Networks[i]
		}
	}
	if net == nil {
		return "", "", p, fmt.Errorf("connect: container %s is on no network with a subnet", f.DB.Container)
	}
	return f.Instance.Region, f.Instance.ID, vm.ConnectParams{DBContainer: f.DB.Container, Network: net.Name,
		Subnet: net.Subnet, Hostname: onboard.TailnetHost(f.App), App: f.App}, nil
}

func (c *Connector) now() time.Time {
	if c.Now != nil {
		return c.Now().UTC()
	}
	return time.Now().UTC()
}

// Connect tags the instance and runs HopCharlie-Connect. It is safe to
// call again after a failure: every step finds what an earlier call did.
func (c *Connector) Connect(ctx context.Context, in Input) (Result, error) {
	var res Result
	region, instance, p, err := params(in.Facts)
	if err != nil {
		return res, err
	}
	if c.Cipher == nil {
		return res, errors.New("connect: the secrets key (PLATFORM_SECRETS_KEY) is not set")
	}
	if c.Tailscale == nil {
		return res, fmt.Errorf("connect: no Tailscale OAuth client (%s)", TailscaleClientIDEnv)
	}
	_, err = c.EC2(region).CreateTags(ctx, &ec2.CreateTagsInput{Resources: []string{instance},
		Tags: []ec2types.Tag{{Key: aws.String(onboard.InstanceTag), Value: aws.String(in.ID)}}})
	if err != nil {
		return res, fmt.Errorf("connect: tag %s: %w", instance, err)
	}
	res.add(vm.Change{OnboardingLegacyChange: apiv1.OnboardingLegacyChange{Kind: apiv1.OnboardingLegacyChangeKindInstanceTagged, Target: instance, At: c.now()}, New: true})

	if p.TailscaleAuthKey, err = c.Tailscale.AuthKey(ctx, "hopcharlie onboarding "+in.ID); err != nil {
		return res, err
	}
	out, err := c.VM(region).Connect(ctx, instance, p)
	for _, ch := range out.Changes {
		res.add(ch)
	}
	if err != nil {
		return res, err
	}
	nonce, ct, err := c.Cipher.SealOnboarding(in.ID, secrets.OnboardingReplicationPassword, out.ReplicationPassword)
	if err != nil {
		return res, err
	}
	res.ReplicationPassword = &Sealed{Nonce: nonce, Ciphertext: ct}
	res.PasswordRotated = out.PasswordRotated
	res.KeyVersion, res.KeyCheck = secrets.KeyVersion, c.Cipher.KeyCheck()
	return res, nil
}

// Disconnect undoes Connect: HopCharlie-Disconnect on the VM, the
// Tailscale device and the instance tag. It returns what it undid, each
// with UndoneAt; the job marks the row's changes of the same kind and
// target. Running it again undoes nothing more.
func (c *Connector) Disconnect(ctx context.Context, in Input) ([]apiv1.OnboardingLegacyChange, error) {
	f := in.Facts
	if f.Instance == nil || f.Instance.Cloud != facts.CloudAWS || f.DB == nil || f.App == "" {
		return nil, errors.New("disconnect: the facts have no AWS instance and database")
	}
	region, instance := f.Instance.Region, f.Instance.ID
	out, err := c.VM(region).Disconnect(ctx, instance, f.DB.Container, f.App)
	if err != nil {
		return out, err
	}
	now := c.now()
	if c.Tailscale != nil {
		host := onboard.TailnetHost(f.App)
		found, err := c.Tailscale.DeleteDevice(ctx, host)
		if err != nil {
			return out, err
		}
		if found && !slices.ContainsFunc(out, func(ch apiv1.OnboardingLegacyChange) bool {
			return ch.Kind == apiv1.OnboardingLegacyChangeKindTailscaleDeviceJoined
		}) {
			out = append(out, apiv1.OnboardingLegacyChange{Kind: apiv1.OnboardingLegacyChangeKindTailscaleDeviceJoined, Target: host, UndoneAt: &now})
		}
	}
	_, err = c.EC2(region).DeleteTags(ctx, &ec2.DeleteTagsInput{Resources: []string{instance},
		Tags: []ec2types.Tag{{Key: aws.String(onboard.InstanceTag)}}})
	if err != nil {
		return out, fmt.Errorf("disconnect: untag %s: %w", instance, err)
	}
	out = append(out, apiv1.OnboardingLegacyChange{Kind: apiv1.OnboardingLegacyChangeKindInstanceTagged, Target: instance, UndoneAt: &now})
	return out, nil
}
