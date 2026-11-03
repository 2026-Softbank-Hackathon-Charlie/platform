package deploy

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/codebuild"
	cbtypes "github.com/aws/aws-sdk-go-v2/service/codebuild/types"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
)

// CodeBuildAPI is the part of the CodeBuild client the builder calls.
type CodeBuildAPI interface {
	StartBuild(ctx context.Context, in *codebuild.StartBuildInput, opts ...func(*codebuild.Options)) (*codebuild.StartBuildOutput, error)
	BatchGetBuilds(ctx context.Context, in *codebuild.BatchGetBuildsInput, opts ...func(*codebuild.Options)) (*codebuild.BatchGetBuildsOutput, error)
}

// CloudWatchLogsAPI is the part of the CloudWatch Logs client the builder calls.
type CloudWatchLogsAPI interface {
	GetLogEvents(ctx context.Context, in *cloudwatchlogs.GetLogEventsInput, opts ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.GetLogEventsOutput, error)
}

// CodeBuildBuilder builds on AWS CodeBuild. Every build runs on Project with
// an inline buildspec (Buildspec) and no source: the buildspec clones the
// repository at the commit with the GitHub token, creates the ECR repository
// hopcharlie/apps/<app>/<service> when it is missing, builds and pushes
// <Registry>/hopcharlie/apps/<app>/<service>:<commit> per service, and
// exports the digests. The project needs privileged mode (docker) and a role
// that may push to ECR and create repositories under hopcharlie/apps/.
type CodeBuildBuilder struct {
	Client  CodeBuildAPI
	Logs    CloudWatchLogsAPI // nil: no build output in the events
	Project string
	// Registry is the ECR registry, <account>.dkr.ecr.<region>.amazonaws.com.
	Registry string
	// PollInterval is how often the build and its log are read (default 5s).
	PollInterval time.Duration
}

// Buildspec is the inline buildspec of every build. It reads everything from
// the environment variables the builder sets, so nothing from a repository
// is ever part of the buildspec text: APP, REPO, COMMIT, REGISTRY,
// GITHUB_TOKEN, SERVICE_COUNT and SERVICE_<i>_NAME, _PATH, _DOCKERFILE. The
// token is only in the URL of the fetch: the command text CodeBuild logs
// shows the variable, not its value, and the remote is reset to a URL
// without it before any Dockerfile runs. exportedVariables lists DIGEST_0 to
// DIGEST_<n-1>.
func Buildspec(services int) string {
	var b strings.Builder
	b.WriteString(`version: 0.2
env:
  shell: bash
  exported-variables:
`)
	for i := range services {
		fmt.Fprintf(&b, "    - DIGEST_%d\n", i)
	}
	b.WriteString(`phases:
  pre_build:
    commands:
      - aws ecr get-login-password | docker login --username AWS --password-stdin "$REGISTRY"
  build:
    commands:
      - |
        set -euo pipefail
        git init -q "${CODEBUILD_SRC_DIR:-$PWD}/app"
        cd "${CODEBUILD_SRC_DIR:-$PWD}/app"
        git remote add origin "https://x-access-token:${GITHUB_TOKEN}@github.com/${REPO}.git"
        git fetch -q --depth 1 origin "$COMMIT"
        git checkout -q FETCH_HEAD
        git remote set-url origin "https://github.com/${REPO}.git"
        unset GITHUB_TOKEN
        for i in $(seq 0 $((SERVICE_COUNT - 1))); do
          name_var="SERVICE_${i}_NAME"; path_var="SERVICE_${i}_PATH"; file_var="SERVICE_${i}_DOCKERFILE"
          name="${!name_var}"; context="${!path_var}"; dockerfile="${!file_var}"
          repository="hopcharlie/apps/${APP}/${name}"
          aws ecr describe-repositories --repository-names "$repository" >/dev/null 2>&1 \
            || aws ecr create-repository --repository-name "$repository" --image-scanning-configuration scanOnPush=true >/dev/null
          image="${REGISTRY}/${repository}:${COMMIT}"
          echo "building ${name} from ${context}/${dockerfile}"
          docker build -t "$image" -f "${context}/${dockerfile}" "$context"
          docker push "$image"
          digest="$(docker inspect --format '{{index .RepoDigests 0}}' "$image")"
          export "DIGEST_${i}=${digest#*@}"
          echo "pushed ${repository}@${digest#*@}"
        done
`)
	return b.String()
}

// Build starts the build, follows it, and returns the pushed images.
func (c *CodeBuildBuilder) Build(ctx context.Context, req BuildRequest, hooks BuildHooks) (BuildResult, error) {
	env := []cbtypes.EnvironmentVariable{
		plain("APP", req.App), plain("REPO", req.Repo), plain("COMMIT", req.Commit),
		plain("REGISTRY", c.Registry), plain("GITHUB_TOKEN", req.Token),
		plain("SERVICE_COUNT", strconv.Itoa(len(req.Services))),
	}
	for i, sv := range req.Services {
		p := "SERVICE_" + strconv.Itoa(i) + "_"
		env = append(env, plain(p+"NAME", sv.Name), plain(p+"PATH", sv.Path), plain(p+"DOCKERFILE", sv.Dockerfile))
	}
	out, err := c.Client.StartBuild(ctx, &codebuild.StartBuildInput{
		ProjectName:                  aws.String(c.Project),
		SourceTypeOverride:           cbtypes.SourceTypeNoSource,
		BuildspecOverride:            aws.String(Buildspec(len(req.Services))),
		PrivilegedModeOverride:       aws.Bool(true),
		EnvironmentVariablesOverride: env,
	})
	if err != nil {
		return BuildResult{}, fmt.Errorf("start CodeBuild build on %s: %w", c.Project, err)
	}
	if out.Build == nil || out.Build.Id == nil {
		return BuildResult{}, errors.New("CodeBuild started a build without an id")
	}
	id := *out.Build.Id
	if err := hooks.started(id); err != nil {
		return BuildResult{}, err
	}

	poll := c.PollInterval
	if poll <= 0 {
		poll = 5 * time.Second
	}
	var logs logCursor
	for {
		got, err := c.Client.BatchGetBuilds(ctx, &codebuild.BatchGetBuildsInput{Ids: []string{id}})
		if err != nil {
			if ctx.Err() != nil {
				return BuildResult{}, ctx.Err()
			}
			hooks.log("could not read build " + id + ": " + err.Error())
		} else if len(got.Builds) == 1 {
			b := got.Builds[0]
			c.readLogs(ctx, &logs, b.Logs, hooks)
			if b.BuildStatus != cbtypes.StatusTypeInProgress && b.BuildStatus != "" {
				c.readLogs(ctx, &logs, b.Logs, hooks) // the last lines arrive with the final status
				return c.result(id, req, b)
			}
		}
		select {
		case <-ctx.Done():
			return BuildResult{}, ctx.Err()
		case <-time.After(poll):
		}
	}
}

func (c *CodeBuildBuilder) result(id string, req BuildRequest, b cbtypes.Build) (BuildResult, error) {
	if b.BuildStatus != cbtypes.StatusTypeSucceeded {
		return BuildResult{}, fmt.Errorf("CodeBuild build %s ended %s%s", id, b.BuildStatus, phaseFailure(b.Phases))
	}
	digests := map[string]string{}
	for _, v := range b.ExportedEnvironmentVariables {
		if v.Name != nil && v.Value != nil {
			digests[*v.Name] = *v.Value
		}
	}
	res := BuildResult{BuildID: id}
	for i, sv := range req.Services {
		d := digests["DIGEST_"+strconv.Itoa(i)]
		if !digestPattern.MatchString(d) {
			return BuildResult{}, fmt.Errorf("CodeBuild build %s succeeded but exported no digest for %s (DIGEST_%d=%q)", id, sv.Name, i, d)
		}
		res.Images = append(res.Images, store.DeploymentImage{Service: sv.Name, Image: ImageRepository(c.Registry, req.App, sv.Name) + "@" + d})
	}
	return res, nil
}

// phaseFailure names the first phase that did not succeed and why.
func phaseFailure(phases []cbtypes.BuildPhase) string {
	for _, p := range phases {
		if p.PhaseStatus == "" || p.PhaseStatus == cbtypes.StatusTypeSucceeded || p.PhaseStatus == cbtypes.StatusTypeInProgress {
			continue
		}
		msg := " in " + string(p.PhaseType) + " (" + string(p.PhaseStatus) + ")"
		for _, ctx := range p.Contexts {
			if ctx.Message != nil && *ctx.Message != "" {
				msg += ": " + *ctx.Message
				break
			}
		}
		return msg
	}
	return ""
}

// logCursor is how far the build's log stream has been read.
type logCursor struct {
	token string
}

// readLogs passes the log lines written since the last call to hooks.Log.
// Failures are ignored: the log is shown on a best-effort basis.
func (c *CodeBuildBuilder) readLogs(ctx context.Context, cur *logCursor, loc *cbtypes.LogsLocation, hooks BuildHooks) {
	if c.Logs == nil || loc == nil || loc.GroupName == nil || loc.StreamName == nil {
		return
	}
	for range 20 { // at most 20 pages per poll; the rest comes next time
		in := &cloudwatchlogs.GetLogEventsInput{LogGroupName: loc.GroupName, LogStreamName: loc.StreamName, StartFromHead: aws.Bool(true)}
		if cur.token != "" {
			in.NextToken = aws.String(cur.token)
		}
		out, err := c.Logs.GetLogEvents(ctx, in)
		if err != nil {
			return
		}
		for _, ev := range out.Events {
			if ev.Message != nil {
				hooks.log(strings.TrimRight(*ev.Message, "\r\n"))
			}
		}
		if out.NextForwardToken == nil || *out.NextForwardToken == cur.token {
			return
		}
		cur.token = *out.NextForwardToken
		if len(out.Events) == 0 {
			return
		}
	}
}

func plain(name, value string) cbtypes.EnvironmentVariable {
	return cbtypes.EnvironmentVariable{Name: aws.String(name), Value: aws.String(value), Type: cbtypes.EnvironmentVariableTypePlaintext}
}
