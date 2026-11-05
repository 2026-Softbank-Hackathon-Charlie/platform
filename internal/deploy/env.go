package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strconv"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/codebuild"
	"k8s.io/client-go/kubernetes"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/sealing"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/secrets"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/storage"
)

// EnvOptions are what FromEnv does not read from the environment.
type EnvOptions struct {
	Store  Store
	GitHub GitHub // nil fails when a builder is configured
	// Gateway is the traffic gateway admin client; nil leaves the gateway alone.
	Gateway Gateway
	// Kube returns an environment's cluster client, for the Service backends.
	Kube func(env string) (kubernetes.Interface, error)
	// AppDomain names the app URL in the events.
	AppDomain string
	// Secrets, Sealer and Storage are Config's: the app secrets' cipher and
	// each environment's sealer and storage driver.
	Secrets *secrets.Cipher
	Sealer  func(env string) (*sealing.Sealer, error)
	Storage func(env string) (storage.Driver, error)
	Log     *slog.Logger
	// Envs are the environments of PLATFORM_ENVS_FILE; a backend template
	// for another one is logged.
	Envs []string
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
	gkeRegistry := getenv("PLATFORM_GKE_IMAGE_REGISTRY")
	gkeCredentials := getenv("PLATFORM_GKE_IMAGE_CREDENTIAL_CONFIG")
	if (gkeRegistry == "") != (gkeCredentials == "") {
		return nil, errors.New("PLATFORM_GKE_IMAGE_REGISTRY and PLATFORM_GKE_IMAGE_CREDENTIAL_CONFIG must be set together")
	}
	if gkeRegistry != "" {
		if builderKind != "codebuild" {
			return nil, errors.New("GKE image publication requires PLATFORM_DEPLOY_BUILDER=codebuild")
		}
		if !regexp.MustCompile(`^[a-z0-9][a-z0-9-]*-docker\.pkg\.dev/[a-z0-9][a-z0-9-]*/[a-z0-9][a-z0-9._-]*$`).MatchString(gkeRegistry) {
			return nil, errors.New("PLATFORM_GKE_IMAGE_REGISTRY must be an Artifact Registry prefix: <location>-docker.pkg.dev/<project>/<repository>")
		}
		var adc struct {
			Type             string `json:"type"`
			CredentialSource struct {
				EnvironmentID string `json:"environment_id"`
			} `json:"credential_source"`
		}
		if err := json.Unmarshal([]byte(gkeCredentials), &adc); err != nil {
			return nil, errors.New("PLATFORM_GKE_IMAGE_CREDENTIAL_CONFIG must be valid AWS external_account (aws1) ADC JSON")
		}
		if adc.Type != "external_account" || adc.CredentialSource.EnvironmentID != "aws1" {
			return nil, errors.New("PLATFORM_GKE_IMAGE_CREDENTIAL_CONFIG must be AWS external_account (aws1) ADC JSON")
		}
	}

	cfg := Config{Store: o.Store, GitHub: o.GitHub, AppDomain: o.AppDomain, Log: o.Log, Secrets: o.Secrets, Sealer: o.Sealer, Storage: o.Storage, GKEImageRegistry: gkeRegistry}
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
		cfg.Builder = &CodeBuildBuilder{
			Client: codebuild.NewFromConfig(aws), Logs: cloudwatchlogs.NewFromConfig(aws), Project: project, Registry: registry,
			GKERegistry: gkeRegistry, GKECredentialConfig: gkeCredentials,
		}
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
		b, err := backendsFromEnv(getenv("PLATFORM_APP_BACKEND_TEMPLATE"), getenv("PLATFORM_APP_BACKEND_TEMPLATES"), o.Kube)
		if err != nil {
			return nil, err
		}
		cfg.Backends = b
		warnUnknownEnvs(b, o.Envs, o.Log)
	}
	return New(cfg)
}

// warnUnknownEnvs logs each environment of PLATFORM_APP_BACKEND_TEMPLATES
// that is not in envs (PLATFORM_ENVS_FILE), most likely a typo: it is not an
// error, as the list may name an environment before the file gets it.
func warnUnknownEnvs(b Backends, envs []string, log *slog.Logger) {
	eb, ok := b.(EnvBackends)
	if !ok || len(envs) == 0 {
		return
	}
	if log == nil {
		log = slog.Default()
	}
	for env := range eb.Templates {
		if !slices.Contains(envs, env) {
			log.Warn("PLATFORM_APP_BACKEND_TEMPLATES names an environment that is not in PLATFORM_ENVS_FILE", "env", env)
		}
	}
}

// backendsFromEnv picks the Backends. PLATFORM_APP_BACKEND_TEMPLATES
// (templates) gives the environments it names their template; every other
// environment uses PLATFORM_APP_BACKEND_TEMPLATE (template), or reads its
// Service's load balancer when that is empty too.
func backendsFromEnv(template, templates string, kube func(env string) (kubernetes.Interface, error)) (Backends, error) {
	var fallback Backends = &ServiceBackends{Kube: kube}
	if template != "" {
		if err := checkTemplate(template); err != nil {
			return nil, fmt.Errorf("PLATFORM_APP_BACKEND_TEMPLATE: %w", err)
		}
		fallback = TemplateBackends{Template: template}
	}
	if templates == "" {
		return fallback, nil
	}
	perEnv, err := parseBackendTemplates(templates)
	if err != nil {
		return nil, fmt.Errorf("PLATFORM_APP_BACKEND_TEMPLATES: %w", err)
	}
	b := EnvBackends{Templates: map[string]TemplateBackends{}, Fallback: fallback}
	for env, t := range perEnv {
		b.Templates[env] = TemplateBackends{Template: t}
	}
	return b, nil
}
