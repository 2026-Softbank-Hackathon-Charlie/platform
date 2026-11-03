package deploy

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/appspec"
)

// The test binary doubles as a fake docker CLI when HOPCHARLIE_FAKE_DOCKER
// is set: DockerBuilder runs it in place of docker.
func TestMain(m *testing.M) {
	if dir := os.Getenv("HOPCHARLIE_FAKE_DOCKER"); dir != "" {
		os.Exit(fakeDocker(dir, os.Args[1:]))
	}
	os.Exit(m.Run())
}

// fakeDocker builds by hashing the Dockerfile, pushes by remembering the
// digest in dir, and inspects by reading it back.
func fakeDocker(dir string, args []string) int {
	flag := func(name string) string {
		if i := slices.Index(args, name); i >= 0 && i+1 < len(args) {
			return args[i+1]
		}
		return ""
	}
	state := func(tag string) string {
		sum := sha256.Sum256([]byte(tag))
		return filepath.Join(dir, hex.EncodeToString(sum[:8]))
	}
	switch args[0] {
	case "build":
		tag, file := flag("-t"), flag("-f")
		content, err := os.ReadFile(file)
		if err != nil {
			fmt.Fprintln(os.Stderr, "ERROR: failed to read dockerfile:", err)
			return 1
		}
		fmt.Println("#1 [internal] load build definition from " + filepath.Base(file))
		fmt.Println("#2 naming to " + tag + " done")
		sum := sha256.Sum256(append(content, tag...))
		return writeOr1(state(tag)+".built", "sha256:"+hex.EncodeToString(sum[:]))
	case "push":
		b, err := os.ReadFile(state(args[1]) + ".built")
		if err != nil {
			fmt.Fprintln(os.Stderr, "An image does not exist locally with the tag:", args[1])
			return 1
		}
		fmt.Println("The push refers to repository [" + args[1] + "]")
		return writeOr1(state(args[1])+".pushed", string(b))
	case "image": // image inspect --format {{json .RepoDigests}} <tag>
		tag := args[len(args)-1]
		b, err := os.ReadFile(state(tag) + ".pushed")
		if err != nil {
			fmt.Println("[]")
			return 0
		}
		repo := tag[:strings.LastIndex(tag, ":")]
		fmt.Printf("[%q]\n", repo+"@"+string(b))
		return 0
	case "echo":
		fmt.Println(strings.Join(args[1:], " "))
		return 0
	}
	fmt.Fprintln(os.Stderr, "fake docker: unknown command", args)
	return 1
}

func writeOr1(path, content string) int {
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func TestDockerBuilder(t *testing.T) {
	isolateGit(t)
	t.Setenv("HOPCHARLIE_FAKE_DOCKER", t.TempDir())
	src := t.TempDir()
	gitCmd(t, src, "init", "-q")
	for p, c := range map[string]string{"platform.yaml": shopSpec, "Dockerfile": "FROM scratch\n", "worker/Dockerfile": "FROM busybox\n"} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(src, p)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(src, p), []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitCmd(t, src, "add", "-A")
	gitCmd(t, src, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "app")
	commit := gitCmd(t, src, "rev-parse", "HEAD")

	b := &DockerBuilder{Registry: "localhost:5001", CloneURL: src, Docker: os.Args[0], WorkDir: t.TempDir()}
	spec, _ := appspec.Check([]byte(shopSpec))
	var started string
	var lines []string
	res, err := b.Build(t.Context(), BuildRequest{DeploymentID: "dep_1", App: "shop", Repo: "org/shop", Commit: commit, Services: spec.Services, Token: testToken},
		BuildHooks{Started: func(id string) error { started = id; return nil }, Log: func(l string) { lines = append(lines, l) }})
	if err != nil {
		t.Fatalf("build: %v\n%s", err, strings.Join(lines, "\n"))
	}
	if !strings.HasPrefix(started, "docker-") || res.BuildID != started || len(res.Images) != 2 {
		t.Fatalf("result %+v, started %q", res, started)
	}
	for i, name := range []string{"web", "worker"} {
		im := res.Images[i]
		if im.Service != name || !strings.HasPrefix(im.Image, "localhost:5001/hopcharlie/apps/shop/"+name+"@sha256:") || !digestPattern.MatchString(im.Image[strings.Index(im.Image, "@")+1:]) {
			t.Errorf("image %+v", im)
		}
	}
	if res.Images[0].Image == res.Images[1].Image {
		t.Error("both services got the same image")
	}
	log := strings.Join(lines, "\n")
	if !strings.Contains(log, "building worker from worker/Dockerfile") || !strings.Contains(log, "naming to localhost:5001/hopcharlie/apps/shop/web:"+commit) {
		t.Errorf("log:\n%s", log)
	}

	// A service whose Dockerfile is missing fails the build.
	spec.Services[1].Dockerfile = "Missing.dockerfile"
	_, err = b.Build(t.Context(), BuildRequest{App: "shop", Repo: "org/shop", Commit: commit, Services: spec.Services}, BuildHooks{})
	if err == nil || !strings.Contains(err.Error(), "docker build worker") || !strings.Contains(err.Error(), "failed to read dockerfile") {
		t.Errorf("missing Dockerfile: %v", err)
	}
	// An unknown commit fails the checkout.
	if _, err := b.Build(t.Context(), BuildRequest{App: "shop", Commit: strings.Repeat("0", 40), Services: spec.Services}, BuildHooks{}); err == nil || !strings.Contains(err.Error(), "check out") {
		t.Errorf("unknown commit: %v", err)
	}
}

func TestRunnerRedactsTheToken(t *testing.T) {
	t.Setenv("HOPCHARLIE_FAKE_DOCKER", t.TempDir())
	var lines []string
	r := runner{token: testToken, log: func(l string) { lines = append(lines, l) }}
	if err := r.run(t.Context(), "", os.Args[0], "echo", "https://x-access-token:"+testToken+"@github.com/org/shop.git"); err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 || strings.Contains(lines[0], testToken) || !strings.Contains(lines[0], "x-access-token:***@") {
		t.Fatalf("lines %q", lines)
	}
	err := r.run(t.Context(), "", os.Args[0], "build", "-t", "x:"+testToken, "-f", "missing-"+testToken)
	if err == nil || strings.Contains(err.Error(), testToken) {
		t.Fatalf("error %v", err)
	}
}

func TestPushedDigest(t *testing.T) {
	d := "sha256:" + strings.Repeat("e", 64)
	got, err := pushedDigest("localhost:5001/hopcharlie/apps/shop/web", []byte(`["docker.io/library/x@`+d+`","localhost:5001/hopcharlie/apps/shop/web@`+d+`"]`+"\n"))
	if err != nil || got != "localhost:5001/hopcharlie/apps/shop/web@"+d {
		t.Fatalf("%q %v", got, err)
	}
	if _, err := pushedDigest("localhost:5001/hopcharlie/apps/shop/web", []byte(`[]`)); err == nil {
		t.Fatal("no digest accepted")
	}
}
