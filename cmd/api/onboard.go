package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/kube"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/airun"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/dbroute"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/envs"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/gateway"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/github"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/onboard"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/onboard/connect"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/onboard/discover"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/onboard/job"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/plans"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/runs"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/secrets"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

// onboardingConfig is the onboarding runner's ports (ADR 0042) from what
// the server has; a missing one fails only the steps that need it.
func onboardingConfig(ctx context.Context, log *slog.Logger, envSvc *envs.Service, cipher *secrets.Cipher, gh *github.Client, ai *airun.Runner,
	gw *gateway.Client, routes *dbroute.Manager, planSvc *plans.Service, runner *runs.Runner, workloads func(string) (dynamic.Interface, error)) *job.Config {
	cfg := &job.Config{Secrets: cipher, Envs: envSvc.Names(), Log: log, SourceDB: job.PG{}, Files: job.Rclone{},
		DNS:     job.Resolver{Domain: envOr("PLATFORM_APP_DOMAIN", "hopcharlie.dev")},
		Cutover: cutover{planSvc, runner}, Kube: kubeOps{clients: workloads, engine: engineClients(envSvc)}}
	if gh != nil {
		cfg.GitHub = gh
	}
	if ai != nil {
		cfg.AI = ai
	}
	if gw != nil {
		cfg.Gateway = gw
	}
	if routes != nil {
		cfg.Routes = routes
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		log.Warn("onboarding: no AWS configuration; discovery and connect of AWS VMs fail", "err", err)
		return cfg
	}
	d := &discover.Discoverer{Web: discover.NetWeb{}, AWS: discover.NewAWSLocator(awsCfg, discover.Regions(os.Getenv)),
		Inspector: discover.SSMInspector{Config: awsCfg}, Images: discover.GHCR{}, Cipher: cipher}
	if gh != nil {
		d.GitHub = gh
	}
	if p := os.Getenv("PLATFORM_ONBOARD_GCP_PROJECT"); p != "" {
		if l, err := discover.NewGCPLocator(ctx, p); err != nil {
			log.Warn("onboarding: GCP instances are not looked up", "err", err)
		} else {
			d.GCP = l
		}
	}
	cfg.Discover = job.FromDiscoverer(d)
	ec2For := func(region string) *ec2.Client {
		c := awsCfg.Copy()
		c.Region = region
		return ec2.NewFromConfig(c)
	}
	conn := &connect.Connector{
		EC2:    func(region string) connect.Tagger { return ec2For(region) },
		VM:     func(region string) connect.VM { return discover.SSMInspector{Config: awsCfg}.Runner(region) },
		Cipher: cipher,
	}
	if ts, err := connect.TailscaleFromEnv(ctx, os.Getenv); err != nil {
		log.Warn("onboarding: no Tailscale OAuth client; connect fails", "err", err)
	} else {
		conn.Tailscale = ts
	}
	cfg.Connect = job.FromConnector(conn, func(ctx context.Context, req job.ConnectRequest) ([]apiv1.OnboardingLegacyChange, error) {
		return stopInstance(ctx, ec2For, req)
	})
	return cfg
}

// engineClients are the migration engine's own clients of the environments
// (kube.Dynamic with the server's kubeconfig at the env's context).
func engineClients(envSvc *envs.Service) func(string) (dynamic.Interface, error) {
	return func(env string) (dynamic.Interface, error) {
		c, ok := envSvc.Config(env)
		if !ok {
			return nil, errors.New("environment " + env + " is not in PLATFORM_ENVS_FILE")
		}
		return kube.Dynamic("", c.KubeContext)
	}
}

// stopInstance stops the onboarding's instance after checking its tag is
// the onboarding id (the IAM condition of StopInstances needs the tag), then
// removes the tag.
func stopInstance(ctx context.Context, ec2For func(string) *ec2.Client, req job.ConnectRequest) ([]apiv1.OnboardingLegacyChange, error) {
	in := req.Facts.Instance
	if in == nil || in.ID == "" {
		return nil, errors.New("the facts have no instance")
	}
	c := ec2For(in.Region)
	out, err := c.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{in.ID}})
	if err != nil {
		return nil, err
	}
	tagged := false
	for _, res := range out.Reservations {
		for _, i := range res.Instances {
			for _, t := range i.Tags {
				if t.Key != nil && *t.Key == onboard.InstanceTag && t.Value != nil && *t.Value == req.ID {
					tagged = true
				}
			}
		}
	}
	if !tagged {
		return nil, fmt.Errorf("instance %s does not carry %s=%s, so it is not stopped", in.ID, onboard.InstanceTag, req.ID)
	}
	if _, err := c.StopInstances(ctx, &ec2.StopInstancesInput{InstanceIds: []string{in.ID}}); err != nil {
		return nil, err
	}
	key := onboard.InstanceTag
	if _, err := c.DeleteTags(ctx, &ec2.DeleteTagsInput{Resources: []string{in.ID}, Tags: []ec2types.Tag{{Key: &key}}}); err != nil {
		return nil, fmt.Errorf("stopped %s, but its tag stays: %w", in.ID, err)
	}
	now := time.Now().UTC()
	return []apiv1.OnboardingLegacyChange{{Kind: apiv1.OnboardingLegacyChangeKindInstanceTagged, Target: in.ID, UndoneAt: &now}}, nil
}

// cutover is job.Cutover over the plan service and the runner.
type cutover struct {
	plans *plans.Service
	runs  *runs.Runner
}

func (c cutover) ReserveOnboarding(ctx context.Context, raw []byte, actor string, scope plans.OnboardingScope) (plans.Reserved, string, error) {
	return c.plans.ReserveOnboarding(ctx, raw, actor, scope)
}
func (c cutover) Started(ctx context.Context, ref, runID string) error {
	return c.plans.Started(ctx, ref, runID)
}
func (c cutover) Release(ctx context.Context, ref string) { c.plans.Release(ctx, ref) }
func (c cutover) Start(ctx context.Context, o runs.StartOptions) (apiv1.Run, error) {
	return c.runs.Start(ctx, o)
}
func (c cutover) Get(ctx context.Context, id string) (apiv1.Run, error) { return c.runs.Get(ctx, id) }
func (c cutover) FindRun(ctx context.Context, planID string) (apiv1.Run, bool, error) {
	list, err := c.runs.List(ctx)
	if err != nil {
		return apiv1.Run{}, false, err
	}
	for _, r := range list { // newest first
		if r.PlanId == planID {
			return r, true, nil
		}
	}
	return apiv1.Run{}, false, nil
}

// kubeOps is job.Kube with the workload clusters' clients: clients reads,
// engine is the identity a migration run patches replica.primary with
// (the server's kubeconfig at the environment's context, as the engine
// builds it from a plan's target.context), which the workload admission
// policy lets change replica.source from legacy to the peer (infra #149).
type kubeOps struct {
	clients func(string) (dynamic.Interface, error)
	engine  func(string) (dynamic.Interface, error)
}

var (
	deploymentsGVR = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
	clustersGVR    = schema.GroupVersionResource{Group: "postgresql.cnpg.io", Version: "v1", Resource: "clusters"}
)

// StartSeconds is from the Deployment's creation to its Available condition.
func (k kubeOps) StartSeconds(ctx context.Context, env, namespace, name string) (float64, error) {
	dyn, err := k.clients(env)
	if err != nil {
		return 0, err
	}
	u, err := dyn.Resource(deploymentsGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return 0, err
	}
	var d struct {
		Metadata struct {
			CreationTimestamp time.Time `json:"creationTimestamp"`
		} `json:"metadata"`
		Status struct {
			Conditions []struct {
				Type, Status       string
				LastTransitionTime time.Time `json:"lastTransitionTime"`
			} `json:"conditions"`
		} `json:"status"`
	}
	b, _ := json.Marshal(u.Object)
	if err := json.Unmarshal(b, &d); err != nil {
		return 0, err
	}
	for _, c := range d.Status.Conditions {
		if c.Type == "Available" && c.Status == "True" {
			return c.LastTransitionTime.Sub(d.Metadata.CreationTimestamp).Seconds(), nil
		}
	}
	return 0, errors.New("the Deployment is not available")
}

// SetReplicaSource merge-patches the Cluster's spec.replica.source.
func (k kubeOps) SetReplicaSource(ctx context.Context, env, namespace, cluster, source string) error {
	dyn, err := k.engine(env)
	if err != nil {
		return err
	}
	patch, _ := json.Marshal(map[string]any{"spec": map[string]any{"replica": map[string]any{"source": source}}})
	_, err = dyn.Resource(clustersGVR).Namespace(namespace).Patch(ctx, cluster, types.MergePatchType, patch, metav1.PatchOptions{FieldManager: "hopcharlie-platform"})
	return err
}
