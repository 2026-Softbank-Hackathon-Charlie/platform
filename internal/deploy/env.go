package deploy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/codebuild"
	"k8s.io/client-go/kubernetes"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
)

// EnvOptions are what FromEnv does not read from the environment.
type EnvOptions struct {
	Store  store.DeployStore
	GitHub GitHub // nil fails when a builder is configured
	// Gateway is the traffic gateway admin client; nil leaves the gateway alone.
	Gateway Gateway
	// Kube returns an environment's cluster client, for the Service backends.
	Kube func(env string) (kubernetes.Interface, error)
	// AppDomain names the app URL in the events.
	AppDomain string
	Log       *slog.Logger
}

// FromEnv builds the executor from the PLATFORM_* variables (see the README).
// It returns nil, nil when PLATFORM_DEPLOY_BUILDER is not set: the server
// then leaves deployments queued.
func FromEnv(ctx context.Context, getenv func(string) string, o EnvOptions) (*Executor, error) {
	builderKind := getenv("PLATFORM_DEPLOY_BUILDER")
	if builderKind == "" {
		return nil, nil
	}
	if o.GitHub == nil {
		return nil, errors.New("PLATFORM_DEPLOY_BUILDER needs the GitHub App to read platform.yaml and clone repositories")
	}
	or := func(k, def string) string {
		if v := getenv(k); v != "" {
			return v
		}
		return def
	}
	registry := getenv("PLATFORM_IMAGE_REGISTRY")
	if registry == "" {
		return nil, errors.New("PLATFORM_IMAGE_REGISTRY is required with PLATFORM_DEPLOY_BUILDER")
	}

	cfg := Config{Store: o.Store, GitHub: o.GitHub, AppDomain: o.AppDomain, Log: o.Log}
	switch builderKind {
	case "codebuild":
		project := getenv("PLATFORM_CODEBUILD_PROJECT")
		if project == "" {
			return nil, errors.New("PLATFORM_CODEBUILD_PROJECT is required with PLATFORM_DEPLOY_BUILDER=codebuild")
		}
		aws, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(or("PLATFORM_AWS_REGION", "ap-northeast-2")))
		if err != nil {
			return nil, fmt.Errorf("AWS configuration: %w", err)
		}
		cfg.Builder = &CodeBuildBuilder{Client: codebuild.NewFromConfig(aws), Logs: cloudwatchlogs.NewFromConfig(aws), Project: project, Registry: registry}
	case "docker":
		cfg.Builder = &DockerBuilder{Registry: registry, CloneURL: getenv("PLATFORM_DOCKER_CLONE_URL")}
	default:
		return nil, fmt.Errorf("PLATFORM_DEPLOY_BUILDER must be codebuild or docker, not %q", builderKind)
	}

	switch mode := or("PLATFORM_GITOPS_MODE", "github"); mode {
	case "github":
		cfg.GitOps = &GitHubGitOps{
			Repo: or("PLATFORM_GITOPS_REPO", DefaultGitOpsRepo), Branch: or("PLATFORM_GITOPS_BRANCH", DefaultGitOpsBranch),
			Token: o.GitHub.Token, APIURL: getenv("PLATFORM_GITHUB_API_URL"),
		}
	case "git":
		u := getenv("PLATFORM_GITOPS_URL")
		if u == "" {
			return nil, errors.New("PLATFORM_GITOPS_URL is required with PLATFORM_GITOPS_MODE=git")
		}
		cfg.GitOps = &GitCLIGitOps{URL: u, Branch: or("PLATFORM_GITOPS_BRANCH", DefaultGitOpsBranch)}
	default:
		return nil, fmt.Errorf("PLATFORM_GITOPS_MODE must be github or git, not %q", mode)
	}

	argo, err := NewArgoCDClient(getenv("PLATFORM_ARGOCD_KUBECONTEXT"), or("PLATFORM_ARGOCD_NAMESPACE", DefaultArgoNamespace), or("PLATFORM_ARGOCD_APPSET", DefaultArgoAppSet))
	if err != nil {
		return nil, err
	}
	cfg.Argo = argo
	if v := getenv("PLATFORM_ARGOCD_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return nil, fmt.Errorf("PLATFORM_ARGOCD_TIMEOUT must be a positive duration such as 10m, not %q", v)
		}
		cfg.SyncTimeout = d
	}
	if v := getenv("PLATFORM_DEPLOY_WORKERS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 32 {
			return nil, fmt.Errorf("PLATFORM_DEPLOY_WORKERS must be between 1 and 32, not %q", v)
		}
		cfg.Workers = n
	}

	if o.Gateway != nil {
		cfg.Gateway = o.Gateway
		if t := getenv("PLATFORM_APP_BACKEND_TEMPLATE"); t != "" {
			cfg.Backends = TemplateBackends{Template: t}
		} else {
			cfg.Backends = &ServiceBackends{Kube: o.Kube}
		}
	}
	return New(cfg)
}
