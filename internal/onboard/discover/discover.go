// Package discover is the discovering state of an onboarding (ADR 0042,
// "1. 조사"): it reads GitHub, DNS and HTTPS, the cloud, the VM and its
// database, and returns one facts.Facts with the blockers and warnings,
// plus the environment values the VM encrypted, sealed with the secrets
// key (ADR 0019) for the onboarding row. It only reads; nothing on the VM
// or in the cloud changes.
//
// The values are never logged and never in Facts or an error: they are
// decrypted in memory, sealed and dropped.
package discover

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/onboard"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/onboard/vm"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/secrets"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/onboard/facts"
)

// Inspector runs HopCharlie-Inspect on an AWS instance (vm.Runner.Inspect,
// with the SSM client of the instance's region).
type Inspector interface {
	Inspect(ctx context.Context, region, instance string) (*vm.Inspection, error)
}

// Discoverer runs a discovery. GitHub, Web, AWS, Inspector, Images and
// Cipher are required; GCP may be nil (a GCE VM is then not found).
type Discoverer struct {
	GitHub    GitHub
	Web       Web
	AWS       Locator
	GCP       Locator
	Inspector Inspector
	Images    Images
	Cipher    *secrets.Cipher
	// AppSlug is the GitHub App's slug for the installation link (default
	// hopcharlie-platform).
	AppSlug string
	// Zone is the domain apps live under (default hopcharlie.dev).
	Zone string
	Now  func() time.Time
}

// Input is what the onboarding was started with.
type Input struct {
	ID         string // onb-<12 hex>
	Repo       string // owner/name
	ServiceURL string // https://<label>.hopcharlie.dev
	// TargetEnv is the environment asked for; empty picks the VM's cloud's
	// (eks for AWS, gke for GCP).
	TargetEnv string
}

// Sealed is one value sealed for an onboarding row's nonce and ciphertext
// columns (secrets.SealOnboarding).
type Sealed struct {
	Nonce, Ciphertext []byte
}

// Result is a discovery's result.
type Result struct {
	Facts facts.Facts
	// Secrets is the containers' environment values,
	// {"<container>": {"<NAME>": "<value>"}} as JSON (the row's secrets),
	// and SuperuserPassword the fence user's password; nil when the VM was
	// not inspected or there is none. KeyVersion and KeyCheck go with them.
	Secrets           *Sealed
	SuperuserPassword *Sealed
	KeyVersion        int
	KeyCheck          string
	// HumanActions are what a person has to do before discovery can go on
	// or the onboarding can reach ready: install_github_app (not done:
	// resume after it), delete_dns_record (done later, in syncing).
	HumanActions []apiv1.OnboardingHumanAction
}

// Blocked reports whether the onboarding cannot go on to patching.
func (r Result) Blocked() bool { return len(r.Facts.Report.Blockers) > 0 }

// Waiting reports whether discovery waits for a person (the App is not
// installed); resume runs it again.
func (r Result) Waiting() bool {
	return r.Facts.GitHub != nil && !r.Facts.GitHub.Installed
}

// Apply writes the facts and the sealed values to the onboarding row.
func (r Result) Apply(o *store.Onboarding) error {
	b, err := json.Marshal(r.Facts)
	if err != nil {
		return err
	}
	o.Facts = b
	if r.Secrets != nil || r.SuperuserPassword != nil {
		o.KeyVersion, o.KeyCheck = r.KeyVersion, r.KeyCheck
	}
	if r.Secrets != nil {
		o.SecretsNonce, o.SecretsCiphertext = r.Secrets.Nonce, r.Secrets.Ciphertext
	}
	if r.SuperuserPassword != nil {
		o.SuperuserPasswordNonce, o.SuperuserPasswordCiphertext = r.SuperuserPassword.Nonce, r.SuperuserPassword.Ciphertext
	}
	return nil
}

var appHost = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`)

// Discover runs every read. An error is a failure to read (retry or
// resume); what rules the app out is in Facts.Report.Blockers.
func (d *Discoverer) Discover(ctx context.Context, in Input) (Result, error) {
	now := time.Now
	if d.Now != nil {
		now = d.Now
	}
	zone := d.Zone
	if zone == "" {
		zone = "hopcharlie.dev"
	}
	f := facts.Facts{Version: facts.Version, Repo: in.Repo, ServiceURL: in.ServiceURL,
		Report: facts.Report{Blockers: []facts.Finding{}, Warnings: []facts.Finding{}}}
	r := report{&f.Report}
	res := Result{}

	host := ""
	if u, err := url.Parse(in.ServiceURL); err == nil && u.Scheme == "https" && (u.Path == "" || u.Path == "/") && u.User == nil && u.Port() == "" && u.RawQuery == "" {
		host = strings.ToLower(u.Hostname())
	}
	label, ok := strings.CutSuffix(host, "."+zone)
	if !ok || !appHost.MatchString(label) {
		r.block(facts.BlockDomainOutOfScope, in.ServiceURL, "the service address must be https://<label>.%s", zone)
	} else {
		f.App = label
	}

	// GitHub.
	repo, err := readRepo(ctx, d.GitHub, in.Repo)
	if err != nil {
		return res, fmt.Errorf("github: %w", err)
	}
	f.GitHub = &repo.Facts
	if !repo.Facts.Installed {
		slug := d.AppSlug
		if slug == "" {
			slug = "hopcharlie-platform"
		}
		link := "https://github.com/apps/" + slug + "/installations/new"
		res.HumanActions = append(res.HumanActions, apiv1.OnboardingHumanAction{Kind: apiv1.OnboardingHumanActionKindInstallGithubApp, Url: &link})
	}

	// DNS and HTTPS.
	if host != "" {
		h, err := probeHTTPS(ctx, d.Web, host, zone)
		if err != nil {
			return res, err
		}
		f.HTTPS = &h
		if !h.Responds {
			r.block(facts.BlockNotHTTPS, host, "%s does not answer over HTTPS with a valid certificate", host)
		}
		if h.Record != nil {
			res.HumanActions = append(res.HumanActions, apiv1.OnboardingHumanAction{Kind: apiv1.OnboardingHumanActionKindDeleteDnsRecord,
				Record: &apiv1.OnboardingDNSRecord{Name: h.Record.Name, Type: h.Record.Type, Value: h.Record.Value}})
		}
	}

	// The cloud.
	var inspection *vm.Inspection
	if f.HTTPS != nil && len(f.HTTPS.Addresses) > 0 {
		ip := f.HTTPS.Addresses[0]
		inst, err := d.AWS.FindInstance(ctx, ip)
		if err != nil {
			return res, err
		}
		if inst == nil && d.GCP != nil {
			if inst, err = d.GCP.FindInstance(ctx, ip); err != nil {
				return res, err
			}
		}
		f.Instance = inst
		switch {
		case inst == nil:
			r.block(facts.BlockInstanceNotFound, ip, "no EC2 or GCE instance has the public address %s", ip)
		case inst.Cloud == facts.CloudGCP:
			r.block(facts.BlockGCPInspection, inst.ID, "the VM is on GCP; inspecting inside a GCP VM is the next step")
		default:
			if inst.AutoScalingGroup != "" {
				r.block(facts.BlockAutoScalingGroup, inst.AutoScalingGroup, "instance %s belongs to auto scaling group %s", inst.ID, inst.AutoScalingGroup)
			}
			if inst.EKSCluster != "" {
				r.block(facts.BlockEKSNode, inst.EKSCluster, "instance %s is a node of EKS cluster %s", inst.ID, inst.EKSCluster)
			}
			if !inst.SSMManaged {
				r.block(facts.BlockNotSSMManaged, inst.ID, "instance %s is not managed by SSM (it needs the SSM agent and an instance profile with AmazonSSMManagedInstanceCore)", inst.ID)
			}
			if inst.SSMManaged && inst.AutoScalingGroup == "" && inst.EKSCluster == "" {
				if inspection, err = d.Inspector.Inspect(ctx, inst.Region, inst.ID); err != nil {
					return res, fmt.Errorf("inspect %s: %w", inst.ID, err)
				}
			}
		}
	}

	// Inside the VM.
	var in2 inspected
	if inspection != nil {
		in2 = fromInspection(&f, inspection.Document, inspection.Values, repo.ProxyTargets, r)
	}
	env := in.TargetEnv
	if env == "" && f.Instance != nil {
		env = map[string]string{facts.CloudAWS: "eks", facts.CloudGCP: "gke"}[f.Instance.Cloud]
	}
	if inspection != nil {
		if err := chooseTarget(ctx, d.Images, &f, env, r); err != nil {
			return res, fmt.Errorf("choose the database image: %w", err)
		}
		if f.App != "" && f.DB != nil {
			cluster := f.App + "-" + f.DB.Service
			if n := len(onboard.EgressService(cluster)); n > onboard.MaxNameLen {
				r.block(facts.BlockNameTooLong, onboard.EgressService(cluster), "%s is %d characters; a Service name has at most %d", onboard.EgressService(cluster), n, onboard.MaxNameLen)
			}
		}
	} else if f.Target == nil && env != "" {
		f.Target = &facts.Target{Env: env, Reindex: []string{}}
	}

	// Drift: what runs, or was last deployed, is not the default branch's
	// head.
	if g := f.GitHub; g != nil && g.Installed {
		drift := facts.Drift{Head: g.Head}
		if f.Target != nil && f.VM != nil {
			for _, c := range f.VM.Containers {
				if c.Service == f.Target.PublicService {
					drift.RunningRevision = c.Revision
				}
			}
		}
		var last *facts.WorkflowRun
		for _, w := range g.Workflows {
			if w.DeploysToVM && w.LastSuccess != nil && (last == nil || w.LastSuccess.At.After(last.At)) {
				last = w.LastSuccess
			}
		}
		if last != nil {
			drift.LastDeployCommit = last.Commit
		}
		if drift.RunningRevision != "" && drift.RunningRevision != g.Head {
			r.warn(facts.WarnDriftRevision, drift.RunningRevision, "the running image was built from %s, not the default branch's head %s", short(drift.RunningRevision), short(g.Head))
		}
		if drift.LastDeployCommit != "" && drift.LastDeployCommit != g.Head {
			r.warn(facts.WarnDriftDeploy, drift.LastDeployCommit, "the last successful deployment was of %s, not the default branch's head %s", short(drift.LastDeployCommit), short(g.Head))
		}
		if drift.RunningRevision != "" || drift.LastDeployCommit != "" {
			f.Report.Drift = &drift
		}
	}

	f.At = now().UTC()
	res.Facts = f

	// Seal the values for the row, and drop them.
	if inspection != nil {
		if d.Cipher == nil {
			return res, errors.New("discover: the secrets key (PLATFORM_SECRETS_KEY) is not set")
		}
		b, err := json.Marshal(inspection.Values)
		if err != nil {
			return res, err
		}
		nonce, ct, err := d.Cipher.SealOnboarding(in.ID, secrets.OnboardingSecrets, string(b))
		if err != nil {
			return res, err
		}
		res.Secrets = &Sealed{Nonce: nonce, Ciphertext: ct}
		if in2.superuserPassword != "" {
			nonce, ct, err := d.Cipher.SealOnboarding(in.ID, secrets.OnboardingSuperuserPassword, in2.superuserPassword)
			if err != nil {
				return res, err
			}
			res.SuperuserPassword = &Sealed{Nonce: nonce, Ciphertext: ct}
		}
		res.KeyVersion, res.KeyCheck = secrets.KeyVersion, d.Cipher.KeyCheck()
	}
	return res, nil
}

func short(sha string) string { return sha[:min(len(sha), 12)] }
