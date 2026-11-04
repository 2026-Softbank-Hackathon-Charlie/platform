package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"runtime"
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

func TestCodeBuildBuilderGKE(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	cb := &fakeCodeBuild{builds: []cbtypes.Build{{BuildStatus: cbtypes.StatusTypeSucceeded,
		ExportedEnvironmentVariables: []cbtypes.ExportedEnvironmentVariable{
			{Name: aws.String("DIGEST_0"), Value: aws.String(digest)},
			{Name: aws.String("DIGEST_1"), Value: aws.String(digest)},
		}}}}
	b := &CodeBuildBuilder{Client: cb, Project: "p", Registry: "ecr.test",
		GKERegistry: "region-docker.pkg.dev/project/apps", GKECredentialConfig: `{"type":"external_account","credential_source":{"environment_id":"aws1"}}`}
	result, err := b.Build(t.Context(), codeBuildRequest(), BuildHooks{})
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"GKE_IMAGE_REGISTRY": b.GKERegistry, "GKE_IMAGE_CREDENTIAL_CONFIG": b.GKECredentialConfig} {
		got, ok := env(cb.start)[name]
		if !ok || aws.ToString(got.Value) != want {
			t.Errorf("CodeBuild environment %s = %q, want %q", name, aws.ToString(got.Value), want)
		}
	}
	if !strings.Contains(aws.ToString(cb.start.BuildspecOverride), "google.auth.aws") {
		t.Error("GKE configuration did not enable AR publication")
	}
	if strings.Contains(aws.ToString(cb.start.BuildspecOverride), b.GKECredentialConfig) {
		t.Error("ADC configuration must be passed as an environment variable")
	}
	if result.Images[0].Image != "ecr.test/hopcharlie/apps/shop/web@"+digest {
		t.Errorf("stored image must remain the canonical ECR reference: %+v", result.Images)
	}
}

func TestCodeBuildBuilderRequiresBothGKESettings(t *testing.T) {
	for _, b := range []*CodeBuildBuilder{{GKERegistry: "registry"}, {GKECredentialConfig: "{}"}} {
		if _, err := b.Build(t.Context(), codeBuildRequest(), BuildHooks{}); err == nil {
			t.Error("a partial GKE image configuration was accepted")
		}
	}
}

func TestCodeBuildGKEPublicationProgram(t *testing.T) {
	var parsed struct {
		Phases map[string]struct{ Commands []string }
	}
	if err := yaml.Unmarshal([]byte(buildspec(2, true)), &parsed); err != nil {
		t.Fatal(err)
	}
	build := strings.Join(parsed.Phases["build"].Commands, "\n")
	const marker = "python3 -I - <<'PY'\n"
	_, program, ok := strings.Cut(build, marker)
	if !ok {
		t.Fatal("no isolated Python publication program")
	}
	program, _, ok = strings.Cut(program, "\nPY\n")
	if !ok || !strings.Contains(build[:strings.Index(build, marker)], "done\n") {
		t.Fatal("AR authentication must follow the entire service build loop")
	}
	if strings.Contains(Buildspec(2), "google.auth") || strings.Contains(Buildspec(2), "GKE_IMAGE") {
		t.Fatal("ECR-only buildspec enables GCP publication")
	}
	names := []string{"python3", "python"}
	if runtime.GOOS == "windows" {
		names = []string{"python", "python3"}
	}
	var python string
	for _, name := range names {
		if path, err := exec.LookPath(name); err == nil {
			python = path
			break
		}
	}
	if python == "" {
		t.Skip("Python 3 is required to execute the emitted CodeBuild publication program")
	}
	encoded, err := json.Marshal(program)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []string{"success", "auth failure", "login failure", "push failure", "inspect failure", "digest mismatch"} {
		t.Run(test, func(t *testing.T) {
			name, _ := json.Marshal(test)
			script := "case = " + string(name) + "\n" + codeBuildGKEStubs + "\nexit_code = 0\ntry:\n    exec(" + string(encoded) + ")\nexcept SystemExit as error:\n    exit_code = error.code\n"
			script += `
expected = ["metadata", "auth"]
if case != "auth failure":
    expected += ["login"]
if case not in ("auth failure", "login failure"):
    expected += ["tag:web", "push:web"]
if case not in ("auth failure", "login failure", "push failure"):
    expected += ["inspect:web"]
if case == "success":
    expected += ["tag:worker", "push:worker", "inspect:worker"]
assert calls == expected, (calls, expected)
assert exit_code == (0 if case == "success" else 1), exit_code
assert all(not os.path.exists(path) for path in docker_configs), "publishing credentials were retained"
`
			out, err := exec.CommandContext(t.Context(), python, "-I", "-c", script).CombinedOutput()
			if err != nil {
				t.Fatalf("emitted publication program: %v\n%s", err, out)
			}
			if strings.Contains(string(out), "DO_NOT_LOG_CREDENTIAL") {
				t.Fatalf("publication error exposed credential material: %s", out)
			}
		})
	}
}

const codeBuildGKEStubs = `
import hashlib, io, json, os, subprocess, sys, tempfile, types, urllib.request
secret = "DO_NOT_LOG_CREDENTIAL"
manifest = '{"schemaVersion":2,"note":"☃"}\r\n'.encode()
digest = "sha256:" + hashlib.sha256(manifest).hexdigest()
os.environ.update(GKE_IMAGE_REGISTRY="asia-northeast3-docker.pkg.dev/project/apps",
                  GKE_IMAGE_CREDENTIAL_CONFIG='{"type":"external_account","credential_source":{"environment_id":"aws1"}}',
                  AWS_CONTAINER_CREDENTIALS_RELATIVE_URI="/v2/credentials/test", AWS_REGION="ap-northeast-2",
                  REGISTRY="ecr.test", APP="shop", COMMIT="commit", SERVICE_COUNT="2",
                  SERVICE_0_NAME="web", SERVICE_1_NAME="worker", DIGEST_0=digest, DIGEST_1=digest)
calls, docker_configs = [], set()
def module(name):
    value = types.ModuleType(name)
    sys.modules[name] = value
    return value
google = module("google")
google.auth = module("google.auth")
google.auth.aws = module("google.auth.aws")
google.auth.transport = module("google.auth.transport")
google.auth.transport.requests = module("google.auth.transport.requests")
google.auth.transport.requests.Request = lambda: object()
class Credentials:
    @classmethod
    def from_info(cls, info, scopes):
        assert info["credential_source"]["environment_id"] == "aws1"
        assert scopes == ["https://www.googleapis.com/auth/cloud-platform"]
        return cls()
    def refresh(self, request):
        calls.append("auth")
        assert os.environ["AWS_SECRET_ACCESS_KEY"] == secret
        assert os.environ["AWS_SESSION_TOKEN"] == secret
        assert os.environ["AWS_REGION"] == "ap-northeast-2"
        if case == "auth failure":
            raise RuntimeError(secret)
        self.token = secret
google.auth.aws.Credentials = Credentials
def metadata(url, timeout):
    assert url == "http://169.254.170.2/v2/credentials/test" and timeout == 10
    calls.append("metadata")
    return io.BytesIO(json.dumps({"AccessKeyId": secret, "SecretAccessKey": secret, "Token": secret}).encode())
urllib.request.urlopen = metadata
def docker(args, env, check, **kwargs):
    assert check and secret not in " ".join(args)
    assert all(key not in env for key in ("AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN"))
    assert os.path.isdir(env["DOCKER_CONFIG"])
    docker_configs.add(env["DOCKER_CONFIG"])
    assert len(docker_configs) == 1
    operation = args[1]
    if operation == "login":
        calls.append("login")
        assert args[2:] == ["--username", "oauth2accesstoken", "--password-stdin", "asia-northeast3-docker.pkg.dev"]
        assert kwargs["input"] == secret.encode()
    else:
        name = args[-1].rsplit("/", 1)[1].split(":", 1)[0]
        assert args[-1] == "asia-northeast3-docker.pkg.dev/project/apps/shop/" + name + ":commit"
        if operation == "tag":
            assert args[2] == "ecr.test/hopcharlie/apps/shop/" + name + ":commit"
        if operation == "buildx":
            assert args[2:5] == ["imagetools", "inspect", "--raw"]
            operation = "inspect"
        calls.append(operation + ":" + name)
    if case == operation + " failure":
        raise subprocess.CalledProcessError(1, args, stderr=secret)
    return subprocess.CompletedProcess(args, 0, stdout=b"different" if case == "digest mismatch" else manifest)
subprocess.run = docker
`
