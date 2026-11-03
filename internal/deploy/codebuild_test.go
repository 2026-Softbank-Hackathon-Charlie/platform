package deploy

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs/types"
	"github.com/aws/aws-sdk-go-v2/service/codebuild"
	cbtypes "github.com/aws/aws-sdk-go-v2/service/codebuild/types"
	"sigs.k8s.io/yaml"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/appspec"
)

// fakeCodeBuild answers BatchGetBuilds with builds in turn, repeating the last.
type fakeCodeBuild struct {
	mu       sync.Mutex
	start    *codebuild.StartBuildInput
	startErr error
	builds   []cbtypes.Build
	gets     int
}

func (f *fakeCodeBuild) StartBuild(_ context.Context, in *codebuild.StartBuildInput, _ ...func(*codebuild.Options)) (*codebuild.StartBuildOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.start = in
	if f.startErr != nil {
		return nil, f.startErr
	}
	return &codebuild.StartBuildOutput{Build: &cbtypes.Build{Id: aws.String("hopcharlie-apps:1234"), BuildStatus: cbtypes.StatusTypeInProgress}}, nil
}

func (f *fakeCodeBuild) BatchGetBuilds(_ context.Context, in *codebuild.BatchGetBuildsInput, _ ...func(*codebuild.Options)) (*codebuild.BatchGetBuildsOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b := f.builds[min(f.gets, len(f.builds)-1)]
	f.gets++
	b.Id = aws.String(in.Ids[0])
	return &codebuild.BatchGetBuildsOutput{Builds: []cbtypes.Build{b}}, nil
}

// fakeLogs serves pages of a log stream; the token is the page index.
type fakeLogs struct {
	mu    sync.Mutex
	pages [][]string
}

func (f *fakeLogs) GetLogEvents(_ context.Context, in *cloudwatchlogs.GetLogEventsInput, _ ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.GetLogEventsOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := 0
	if in.NextToken != nil {
		i = int((*in.NextToken)[0] - '0')
	}
	out := &cloudwatchlogs.GetLogEventsOutput{NextForwardToken: aws.String(string(rune('0' + min(i+1, len(f.pages)))))}
	if i < len(f.pages) {
		for _, m := range f.pages[i] {
			out.Events = append(out.Events, cwtypes.OutputLogEvent{Message: aws.String(m + "\n")})
		}
	} else {
		out.NextForwardToken = in.NextToken
	}
	return out, nil
}

func codeBuildRequest() BuildRequest {
	spec, _ := appspec.Check([]byte(shopSpec))
	return BuildRequest{DeploymentID: "dep_1", App: "shop", Repo: "org/shop", Commit: sha("c1"), Services: spec.Services, Token: testToken}
}

func env(in *codebuild.StartBuildInput) map[string]cbtypes.EnvironmentVariable {
	out := map[string]cbtypes.EnvironmentVariable{}
	for _, v := range in.EnvironmentVariablesOverride {
		out[*v.Name] = v
	}
	return out
}

func TestCodeBuildBuilder(t *testing.T) {
	digest0, digest1 := "sha256:"+strings.Repeat("a", 64), "sha256:"+strings.Repeat("b", 64)
	logsAt := &cbtypes.LogsLocation{GroupName: aws.String("/aws/codebuild/hopcharlie-apps"), StreamName: aws.String("1234")}
	cb := &fakeCodeBuild{builds: []cbtypes.Build{
		{BuildStatus: cbtypes.StatusTypeInProgress},
		{BuildStatus: cbtypes.StatusTypeInProgress, Logs: logsAt},
		{BuildStatus: cbtypes.StatusTypeSucceeded, Logs: logsAt, ExportedEnvironmentVariables: []cbtypes.ExportedEnvironmentVariable{
			{Name: aws.String("DIGEST_0"), Value: aws.String(digest0)}, {Name: aws.String("DIGEST_1"), Value: aws.String(digest1)},
		}},
	}}
	logs := &fakeLogs{pages: [][]string{{"[Container] Entering phase BUILD", "building web from ./Dockerfile"}, {"pushed hopcharlie/apps/shop/web@" + digest0}}}
	b := &CodeBuildBuilder{Client: cb, Logs: logs, Project: "hopcharlie-apps", Registry: "123456789012.dkr.ecr.ap-northeast-2.amazonaws.com", PollInterval: 1}

	var started string
	var lines []string
	res, err := b.Build(t.Context(), codeBuildRequest(), BuildHooks{
		Started: func(id string) error { started = id; return nil },
		Log:     func(l string) { lines = append(lines, l) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if started != "hopcharlie-apps:1234" || res.BuildID != started {
		t.Errorf("build id %q, started %q", res.BuildID, started)
	}
	if len(res.Images) != 2 || res.Images[0].Image != "123456789012.dkr.ecr.ap-northeast-2.amazonaws.com/hopcharlie/apps/shop/web@"+digest0 ||
		res.Images[1].Service != "worker" || !strings.HasSuffix(res.Images[1].Image, "/hopcharlie/apps/shop/worker@"+digest1) {
		t.Errorf("images %+v", res.Images)
	}
	if strings.Join(lines, "|") != "[Container] Entering phase BUILD|building web from ./Dockerfile|pushed hopcharlie/apps/shop/web@"+digest0 {
		t.Errorf("log lines %q", lines)
	}

	in := cb.start
	if *in.ProjectName != "hopcharlie-apps" || in.SourceTypeOverride != cbtypes.SourceTypeNoSource || !*in.PrivilegedModeOverride {
		t.Errorf("start input %+v", in)
	}
	e := env(in)
	for k, want := range map[string]string{
		"APP": "shop", "REPO": "org/shop", "COMMIT": sha("c1"), "REGISTRY": b.Registry, "GITHUB_TOKEN": testToken, "SERVICE_COUNT": "2",
		"SERVICE_0_NAME": "web", "SERVICE_0_PATH": ".", "SERVICE_0_DOCKERFILE": "Dockerfile",
		"SERVICE_1_NAME": "worker", "SERVICE_1_PATH": "worker", "SERVICE_1_DOCKERFILE": "Dockerfile",
	} {
		if v, ok := e[k]; !ok || *v.Value != want {
			t.Errorf("%s = %v, want %q", k, v.Value, want)
		}
	}

	// The buildspec is YAML with the digests exported, and nothing from the
	// request (the token least of all) is in its text.
	spec := *in.BuildspecOverride
	if strings.Contains(spec, testToken) || strings.Contains(spec, "org/shop") {
		t.Fatalf("the buildspec carries request values:\n%s", spec)
	}
	var parsed struct {
		Version string `json:"version"`
		Env     struct {
			Shell     string   `json:"shell"`
			Exported  []string `json:"exported-variables"`
			Variables any      `json:"variables"`
		} `json:"env"`
		Phases map[string]struct {
			Commands []string `json:"commands"`
		} `json:"phases"`
	}
	if err := yaml.UnmarshalStrict([]byte(spec), &parsed); err != nil {
		t.Fatalf("buildspec is not YAML: %v\n%s", err, spec)
	}
	build := strings.Join(parsed.Phases["build"].Commands, "\n")
	if parsed.Version != "0.2" || strings.Join(parsed.Env.Exported, ",") != "DIGEST_0,DIGEST_1" ||
		!strings.Contains(build, `https://x-access-token:${GITHUB_TOKEN}@github.com/${REPO}.git`) || !strings.Contains(build, "set-url origin \"https://github.com/${REPO}.git\"") ||
		!strings.Contains(build, "aws ecr create-repository") || !strings.Contains(build, "docker push") || !strings.Contains(build, "set -euo pipefail") {
		t.Errorf("buildspec:\n%s", spec)
	}
}

func TestCodeBuildBuilderFailures(t *testing.T) {
	b := &CodeBuildBuilder{Project: "p", Registry: "r", PollInterval: 1}
	hooks := BuildHooks{}

	b.Client = &fakeCodeBuild{builds: []cbtypes.Build{{BuildStatus: cbtypes.StatusTypeFailed, Phases: []cbtypes.BuildPhase{
		{PhaseType: cbtypes.BuildPhaseTypeProvisioning, PhaseStatus: cbtypes.StatusTypeSucceeded},
		{PhaseType: cbtypes.BuildPhaseTypeBuild, PhaseStatus: cbtypes.StatusTypeFailed, Contexts: []cbtypes.PhaseContext{{Message: aws.String("COMMAND_EXECUTION_ERROR: Error while executing command: docker build. Reason: exit status 1")}}},
	}}}}
	_, err := b.Build(t.Context(), codeBuildRequest(), hooks)
	if err == nil || !strings.Contains(err.Error(), "ended FAILED in BUILD (FAILED): COMMAND_EXECUTION_ERROR") {
		t.Errorf("failed build: %v", err)
	}

	b.Client = &fakeCodeBuild{builds: []cbtypes.Build{{BuildStatus: cbtypes.StatusTypeSucceeded}}}
	if _, err := b.Build(t.Context(), codeBuildRequest(), hooks); err == nil || !strings.Contains(err.Error(), "exported no digest for web") {
		t.Errorf("no digests: %v", err)
	}

	b.Client = &fakeCodeBuild{startErr: errors.New("AccessDeniedException")}
	if _, err := b.Build(t.Context(), codeBuildRequest(), hooks); err == nil || !strings.Contains(err.Error(), "AccessDenied") {
		t.Errorf("start error: %v", err)
	}

	stopped := errors.New("superseded")
	b.Client = &fakeCodeBuild{builds: []cbtypes.Build{{BuildStatus: cbtypes.StatusTypeInProgress}}}
	if _, err := b.Build(t.Context(), codeBuildRequest(), BuildHooks{Started: func(string) error { return stopped }}); !errors.Is(err, stopped) {
		t.Errorf("Started error: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := b.Build(ctx, codeBuildRequest(), hooks); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled: %v", err)
	}
}
