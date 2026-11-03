// Package charts tests the Helm charts under deploy/ with helm lint and
// helm template: the app chart hopcharlie-app, which Argo CD renders with
// every gitops file apps/<instance>/<env>.yaml (ADR 0011) as a values file,
// and deploy/argocd, the AppProject and ApplicationSet that do it.
// test/e2e/deploy runs both in a real Argo CD.
//
// The tests need helm on PATH. Without it they are skipped, except in CI.
package charts

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"text/template"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/yaml"
)

var (
	appChart    = filepath.Join("..", "..", "deploy", "charts", "hopcharlie-app")
	appsetChart = filepath.Join("..", "..", "deploy", "argocd")
)

// TestMain reads every file of the charts, so that go test's result cache,
// which sees the files a test opens but not what helm reads, reruns these
// tests when a chart changes.
func TestMain(m *testing.M) {
	for _, dir := range []string{appChart, appsetChart} {
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			_, err = os.ReadFile(path)
			return err
		})
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	os.Exit(m.Run())
}

func helmPath(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("helm")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("helm is not on PATH")
		}
		t.Skip("helm is not on PATH")
	}
	return p
}

// helm runs helm and returns its stdout, or an error with its stderr.
func helm(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(helmPath(t), args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("helm %s: %v: %s", strings.Join(args, " "), err, stderr.String())
	}
	return stdout.String(), nil
}

// writeValues writes values YAML to a file for helm -f.
func writeValues(t *testing.T, values string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "values-*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(values); err != nil {
		t.Fatal(err)
	}
	return f.Name()
}

var docSeparator = regexp.MustCompile(`(?m)^---\s*$`)

// render runs helm template with the values files and returns the documents
// by "Kind/name".
func render(t *testing.T, chart, namespace string, valueFiles ...string) (map[string][]byte, error) {
	t.Helper()
	args := []string{"template", "release", chart, "--namespace", namespace}
	for _, f := range valueFiles {
		args = append(args, "-f", f)
	}
	out, err := helm(t, args...)
	if err != nil {
		return nil, err
	}
	docs := map[string][]byte{}
	for _, doc := range docSeparator.Split(out, -1) {
		var head struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
		}
		if err := yaml.Unmarshal([]byte(doc), &head); err != nil {
			t.Fatalf("rendered YAML does not parse: %v\n%s", err, doc)
		}
		if head.Kind == "" {
			continue // comments only
		}
		key := head.Kind + "/" + head.Metadata.Name
		if _, dup := docs[key]; dup {
			t.Fatalf("%s is rendered twice", key)
		}
		docs[key] = []byte(doc)
	}
	return docs, nil
}

func mustRender(t *testing.T, chart, namespace string, valueFiles ...string) map[string][]byte {
	t.Helper()
	docs, err := render(t, chart, namespace, valueFiles...)
	if err != nil {
		t.Fatal(err)
	}
	return docs
}

func keys(m map[string][]byte) []string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	slices.Sort(ks)
	return ks
}

// decode decodes a rendered document strictly, so a misspelled field fails.
func decode(t *testing.T, docs map[string][]byte, key string, into any) {
	t.Helper()
	doc, ok := docs[key]
	if !ok {
		t.Fatalf("%s is not rendered; have %v", key, keys(docs))
	}
	if err := yaml.UnmarshalStrict(doc, into); err != nil {
		t.Fatalf("%s: %v\n%s", key, err, doc)
	}
}

func envByName(c corev1.Container) map[string]corev1.EnvVar {
	m := map[string]corev1.EnvVar{}
	for _, e := range c.Env {
		m[e.Name] = e
	}
	return m
}

func envNames(c corev1.Container) []string {
	var names []string
	for _, e := range c.Env {
		names = append(names, e.Name)
	}
	return names
}

func secretRef(t *testing.T, e corev1.EnvVar) string {
	t.Helper()
	if e.ValueFrom == nil || e.ValueFrom.SecretKeyRef == nil {
		t.Fatalf("%s is not from a secret: %+v", e.Name, e)
	}
	return e.ValueFrom.SecretKeyRef.Name + "/" + e.ValueFrom.SecretKeyRef.Key
}

func jsonStrings[T any](items []T) []string {
	var out []string
	for _, it := range items {
		b, _ := json.Marshal(it)
		out = append(out, string(b))
	}
	return out
}

func TestLint(t *testing.T) {
	for _, chart := range []string{appChart, appsetChart} {
		files, err := filepath.Glob(filepath.Join(chart, "ci", "*.yaml"))
		if err != nil || len(files) == 0 {
			t.Fatalf("no fixtures in %s/ci (%v)", chart, err)
		}
		for _, f := range files {
			if _, err := helm(t, "lint", "--strict", chart, "-f", f); err != nil {
				t.Error(err)
			}
		}
	}
}

func TestStatelessApp(t *testing.T) {
	docs := mustRender(t, appChart, "demo", filepath.Join(appChart, "ci", "stateless-values.yaml"))
	// The default profile adds no guardrails: an environment opts in.
	want := []string{"Deployment/demo-web", "Deployment/demo-worker", "Namespace/demo", "Service/demo-web", "Service/demo-worker"}
	if got := keys(docs); !slices.Equal(got, want) {
		t.Fatalf("rendered %v, want %v", got, want)
	}

	// The app owns its namespace: Argo CD makes it first and deletes it
	// with the Application.
	var ns corev1.Namespace
	decode(t, docs, "Namespace/demo", &ns)
	for k, v := range map[string]string{
		"hopcharlie.dev/app":                 "demo",
		"hopcharlie.dev/instance":            "demo",
		"pod-security.kubernetes.io/enforce": "restricted",
		"pod-security.kubernetes.io/audit":   "restricted",
		"pod-security.kubernetes.io/warn":    "restricted",
	} {
		if ns.Labels[k] != v {
			t.Errorf("namespace label %s = %q, want %q", k, ns.Labels[k], v)
		}
	}
	if ns.Annotations["argocd.argoproj.io/sync-wave"] != "-1" {
		t.Errorf("namespace annotations %v, want sync-wave -1", ns.Annotations)
	}

	var web appsv1.Deployment
	decode(t, docs, "Deployment/demo-web", &web)
	if *web.Spec.Replicas != 2 {
		t.Errorf("replicas %d, want 2", *web.Spec.Replicas)
	}
	// A new version that never gets ready must leave the old pods serving.
	ru := web.Spec.Strategy.RollingUpdate
	if web.Spec.Strategy.Type != appsv1.RollingUpdateDeploymentStrategyType || ru == nil ||
		*ru.MaxUnavailable != intstr.FromInt32(0) || *ru.MaxSurge != intstr.FromInt32(1) {
		t.Errorf("strategy %+v, want RollingUpdate maxUnavailable 0 maxSurge 1", web.Spec.Strategy)
	}
	if web.Spec.ProgressDeadlineSeconds == nil || *web.Spec.ProgressDeadlineSeconds != 180 {
		t.Errorf("progressDeadlineSeconds %v, want 180", web.Spec.ProgressDeadlineSeconds)
	}
	sel := map[string]string{"hopcharlie.dev/app": "demo", "hopcharlie.dev/service": "web"}
	if !mapsEqual(web.Spec.Selector.MatchLabels, sel) {
		t.Errorf("selector %v, want %v", web.Spec.Selector.MatchLabels, sel)
	}
	pod := web.Spec.Template.Spec
	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
		t.Error("the service account token is mounted")
	}
	// The restricted Pod Security Standard on the app namespaces.
	psc := pod.SecurityContext
	if psc == nil || psc.RunAsNonRoot == nil || !*psc.RunAsNonRoot || psc.SeccompProfile == nil ||
		psc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Errorf("pod securityContext %+v is not restricted", psc)
	}
	c := pod.Containers[0]
	sc := c.SecurityContext
	if sc == nil || sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation ||
		sc.Capabilities == nil || !slices.Equal(sc.Capabilities.Drop, []corev1.Capability{"ALL"}) {
		t.Errorf("container securityContext %+v is not restricted", sc)
	}
	if !strings.Contains(c.Image, "@sha256:") {
		t.Errorf("image %s", c.Image)
	}
	if got, want := envNames(c), []string{"PORT", "DEMO_MESSAGE", "LOG_LEVEL"}; !slices.Equal(got, want) {
		t.Errorf("env %v, want %v", got, want)
	}
	env := envByName(c)
	if env["PORT"].Value != "8080" || env["LOG_LEVEL"].Value != "0755" {
		t.Errorf("PORT %q, LOG_LEVEL %q", env["PORT"].Value, env["LOG_LEVEL"].Value)
	}
	for name, p := range map[string]*corev1.Probe{"startup": c.StartupProbe, "readiness": c.ReadinessProbe, "liveness": c.LivenessProbe} {
		if p == nil || p.HTTPGet == nil || p.HTTPGet.Path != "/healthz" || p.HTTPGet.Port != intstr.FromString("http") {
			t.Errorf("%s probe %+v, want GET /healthz on http", name, p)
		}
	}
	// Serves on while the nodes take it out of the Service.
	if c.Lifecycle == nil || c.Lifecycle.PreStop == nil || c.Lifecycle.PreStop.Sleep == nil || c.Lifecycle.PreStop.Sleep.Seconds != 5 {
		t.Errorf("lifecycle %+v, want a 5 s preStop sleep", c.Lifecycle)
	}
	if len(c.Ports) != 1 || c.Ports[0].Name != "http" || c.Ports[0].ContainerPort != 8080 {
		t.Errorf("ports %+v", c.Ports)
	}

	// Without healthCheck: ready once the port accepts, never restarted by a probe.
	var worker appsv1.Deployment
	decode(t, docs, "Deployment/demo-worker", &worker)
	wc := worker.Spec.Template.Spec.Containers[0]
	if wc.ReadinessProbe == nil || wc.ReadinessProbe.TCPSocket == nil || wc.LivenessProbe != nil || wc.StartupProbe != nil {
		t.Errorf("worker probes: readiness %+v liveness %+v startup %+v, want a TCP readiness probe only",
			wc.ReadinessProbe, wc.LivenessProbe, wc.StartupProbe)
	}
	if got := envNames(wc); !slices.Equal(got, []string{"PORT"}) || wc.Env[0].Value != "9090" {
		t.Errorf("worker env %+v", wc.Env)
	}

	for _, name := range []string{"web", "worker"} {
		var s corev1.Service
		decode(t, docs, "Service/demo-"+name, &s)
		if s.Spec.Type != corev1.ServiceTypeClusterIP || len(s.Annotations) != 0 {
			t.Errorf("Service demo-%s: type %s annotations %v", name, s.Spec.Type, s.Annotations)
		}
		if s.Spec.Selector["hopcharlie.dev/service"] != name || s.Spec.Ports[0].TargetPort != intstr.FromString("http") {
			t.Errorf("Service demo-%s: %+v", name, s.Spec)
		}
	}
}

func TestStatefulApp(t *testing.T) {
	docs := mustRender(t, appChart, "moimroom", filepath.Join(appChart, "ci", "stateful-values.yaml"))
	want := []string{"Cluster/moimroom-db", "Deployment/moimroom-web", "LimitRange/hopcharlie-app", "Namespace/moimroom",
		"NetworkPolicy/hopcharlie-app", "ResourceQuota/hopcharlie-app", "Service/moimroom-web"}
	if got := keys(docs); !slices.Equal(got, want) {
		t.Fatalf("rendered %v, want %v", got, want)
	}

	var cluster struct {
		APIVersion string `json:"apiVersion"`
		Spec       struct {
			Instances int `json:"instances"`
			Storage   struct {
				Size         string `json:"size"`
				StorageClass string `json:"storageClass"`
			} `json:"storage"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal(docs["Cluster/moimroom-db"], &cluster); err != nil {
		t.Fatal(err)
	}
	if cluster.APIVersion != "postgresql.cnpg.io/v1" || cluster.Spec.Instances != 1 ||
		cluster.Spec.Storage.Size != "2Gi" || cluster.Spec.Storage.StorageClass != "gp3" {
		t.Errorf("Cluster %+v", cluster)
	}

	var d appsv1.Deployment
	decode(t, docs, "Deployment/moimroom-web", &d)
	if d.Annotations["hopcharlie.dev/commit"] != "0f2fa18d0c2e4a1b9f3e5d7c6b8a9e0f1a2b3c4d" || d.Annotations["hopcharlie.dev/deployment"] != "dep-2" {
		t.Errorf("annotations %v, want the gitops file's commit and deployment", d.Annotations)
	}
	if len(d.Spec.Template.Annotations) != 0 {
		t.Errorf("pod annotations %v: a new deployment of the same commit would restart the pods", d.Spec.Template.Annotations)
	}
	pod := d.Spec.Template.Spec
	if len(pod.ImagePullSecrets) != 1 || pod.ImagePullSecrets[0].Name != "ecr-pull" {
		t.Errorf("imagePullSecrets %+v", pod.ImagePullSecrets)
	}
	c := pod.Containers[0]
	wantEnv := []string{"PORT", "DATABASE_URL", "S3_ENDPOINT", "S3_BUCKET", "S3_REGION", "S3_PATH_STYLE", "S3_ACCESS_KEY", "S3_SECRET_KEY", "LOG_LEVEL"}
	if got := envNames(c); !slices.Equal(got, wantEnv) {
		t.Errorf("env %v, want %v", got, wantEnv)
	}
	env := envByName(c)
	if got := secretRef(t, env["DATABASE_URL"]); got != "moimroom-db-app/uri" {
		t.Errorf("DATABASE_URL from %s, want moimroom-db-app/uri (CNPG's app secret)", got)
	}
	for name, want := range map[string]string{
		"S3_ENDPOINT":   "https://s3.ap-northeast-2.amazonaws.com",
		"S3_BUCKET":     "hopcharlie-eks-moimroom-photos",
		"S3_REGION":     "ap-northeast-2",
		"S3_PATH_STYLE": "false",
	} {
		if env[name].Value != want {
			t.Errorf("%s = %q, want %q", name, env[name].Value, want)
		}
	}
	if got := secretRef(t, env["S3_ACCESS_KEY"]); got != "app-s3/accessKey" {
		t.Errorf("S3_ACCESS_KEY from %s", got)
	}
	if got := secretRef(t, env["S3_SECRET_KEY"]); got != "app-s3/secretKey" {
		t.Errorf("S3_SECRET_KEY from %s", got)
	}

	var s corev1.Service
	decode(t, docs, "Service/moimroom-web", &s)
	if s.Spec.Type != corev1.ServiceTypeLoadBalancer || s.Annotations["service.beta.kubernetes.io/aws-load-balancer-scheme"] != "internal" {
		t.Errorf("public Service: type %s annotations %v, want the profile's internal load balancer", s.Spec.Type, s.Annotations)
	}

	// The namespace's guardrails.
	var quota corev1.ResourceQuota
	decode(t, docs, "ResourceQuota/hopcharlie-app", &quota)
	if q := quota.Spec.Hard[corev1.ResourceRequestsCPU]; q.String() != "2" || len(quota.Spec.Hard) != 7 {
		t.Errorf("quota %v", quota.Spec.Hard)
	}
	var limits corev1.LimitRange
	decode(t, docs, "LimitRange/hopcharlie-app", &limits)
	if l := limits.Spec.Limits; len(l) != 1 || l[0].Type != corev1.LimitTypeContainer ||
		l[0].Default.Memory().String() != "256Mi" || l[0].DefaultRequest.Cpu().String() != "50m" {
		t.Errorf("limits %+v", limits.Spec.Limits)
	}
	var np networkingv1.NetworkPolicy
	decode(t, docs, "NetworkPolicy/hopcharlie-app", &np)
	if len(np.Spec.PodSelector.MatchLabels) != 0 || !slices.Equal(np.Spec.PolicyTypes, []networkingv1.PolicyType{"Ingress", "Egress"}) {
		t.Errorf("policy applies to %v, types %v; want every pod, both directions", np.Spec.PodSelector, np.Spec.PolicyTypes)
	}
	var from []networkingv1.NetworkPolicyPeer
	for _, r := range np.Spec.Ingress {
		from = append(from, r.From...)
	}
	wantFrom := []string{
		`{"podSelector":{}}`,
		`{"namespaceSelector":{"matchLabels":{"kubernetes.io/metadata.name":"cnpg-system"}}}`,
		`{"namespaceSelector":{"matchLabels":{"kubernetes.io/metadata.name":"charlie-platform"}}}`,
		`{"ipBlock":{"cidr":"10.0.0.0/16"}}`,
	}
	if got := jsonStrings(from); !slices.Equal(got, wantFrom) {
		t.Errorf("ingress from %v, want %v", got, wantFrom)
	}
	wantTo := []string{
		`{"to":[{"podSelector":{}}]}`,
		`{"ports":[{"protocol":"UDP","port":53},{"protocol":"TCP","port":53}]}`,
		`{"ports":[{"protocol":"TCP","port":443}]}`,
	}
	if got := jsonStrings(np.Spec.Egress); !slices.Equal(got, wantTo) {
		t.Errorf("egress %v, want %v", got, wantTo)
	}
}

func TestRejectsBadValues(t *testing.T) {
	const img = `"r/a/web@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"`
	file := func(services, resources string) string {
		return "app: a\nenv: kind\nnamespace: a\nvalues:\n  app: a\n  services:\n" + services + "  resources: " + resources + "\n"
	}
	web := func(extra string) string { return "    - {name: web, image: " + img + ", port: 8080" + extra + "}\n" }
	two := func(a, b string) string {
		return "    - {name: " + a + ", image: " + img + ", port: 1, public: true}\n    - {name: " + b + ", image: " + img + ", port: 2}\n"
	}
	for _, c := range []struct{ name, values, want string }{
		{"no services", "values:\n  app: a\n  services: []\n", "minItems: got 0, want 1"},
		{"bad app name", strings.Replace(file(web(""), "[]"), "  app: a", "  app: A", 1), "'A' does not match pattern"},
		{"image by tag", file("    - {name: web, image: r/a/web:latest, port: 8080}\n", "[]"), "'r/a/web:latest' does not match pattern"},
		{"PORT in env", file(web(`, env: {PORT: "1"}`), "[]"), "invalid propertyName 'PORT'"},
		{"S3_ prefix in env", file(web(", env: {S3_BUCKET: x}"), "[]"), "invalid propertyName 'S3_BUCKET'"},
		{"env not a string", file(web(", env: {COUNT: 4}"), "[]"), "got number, want string"},
		{"unknown service key", file(web(", cpu: 2"), "[]"), "'cpu' not allowed"},
		{"too many replicas", file(web(", replicas: 6"), "[]"), "maximum: got 6, want 5"},
		{"unknown resource type", file(web(""), "[{name: cache, type: redis}]"), "value must be one of 'postgres', 's3'"},
		{"unknown top-level key", file(web(""), "[]") + "extra: 1\n", "'extra' not allowed"},
		{"two public services", strings.Replace(file(two("web", "api"), "[]"), "port: 2}", "port: 2, public: true}", 1), "only one service may be public (web, api)"},
		{"two services with one name", file(two("web", "web"), "[]"), "two services are named web"},
		{"two postgres resources", file(web(""), "[{name: a, type: postgres}, {name: b, type: postgres}]"), "only one postgres resource is allowed (a and b)"},
		{"s3 without a profile", file(web(""), "[{name: files, type: s3}]"), "profile.s3.endpoint and profile.s3.credentialsSecret are required"},
		{"quota without its size", file(web(""), "[]") + "profile: {quota: {enabled: true}}\n", "profile.quota.hard is required"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := render(t, appChart, "a", writeValues(t, c.values))
			if err == nil {
				t.Fatalf("rendered; want an error with %q", c.want)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error %v, want %q in it", err, c.want)
			}
		})
	}
}

// gitopsFile is a gitops file as internal/deploy renders it (ADR 0011), for
// the instance named namespace.
func gitopsFile(env, namespace string) string {
	return `# Generated by the HopCharlie platform. Do not edit.
app: "moimroom"
env: "` + env + `"
namespace: "` + namespace + `"
commit: "0f2fa18d0c2e4a1b9f3e5d7c6b8a9e0f1a2b3c4d"
deployment: "dep-1"
values:
  app: "moimroom"
  services:
    - name: "web"
      image: "r/hopcharlie/apps/moimroom/web@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
      port: 8080
      healthCheck: "/healthz"
      public: true
      replicas: 1
      env:
        "KEY": "0755"
  resources:
    - name: "db"
      type: "postgres"
    - name: "photos"
      type: "s3"
`
}

// argoParams are the parameters Argo CD's git files generator gives a gitops
// file in goTemplate mode: its keys, parsed as the generator parses them,
// and path.
func argoParams(t *testing.T, filePath, content string) map[string]any {
	t.Helper()
	var params map[string]any
	if err := yaml.Unmarshal([]byte(content), &params); err != nil {
		t.Fatal(err)
	}
	dir := filePath[:strings.LastIndex(filePath, "/")]
	params["path"] = map[string]any{"path": dir, "filename": filePath[len(dir)+1:]}
	return params
}

// argoRender renders an ApplicationSet template string as Argo CD does, with
// the functions the templates here use.
func argoRender(t *testing.T, text string, params map[string]any) string {
	t.Helper()
	tm, err := template.New("argo").Option("missingkey=error").Funcs(template.FuncMap{
		"toJson": func(v any) (string, error) { b, err := json.Marshal(v); return string(b), err },
	}).Parse(text)
	if err != nil {
		t.Fatalf("%v\n%s", err, text)
	}
	var out bytes.Buffer
	if err := tm.Execute(&out, params); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

type appProject struct {
	Spec struct {
		SourceRepos                []string            `json:"sourceRepos"`
		Destinations               []map[string]string `json:"destinations"`
		ClusterResourceWhitelist   []map[string]string `json:"clusterResourceWhitelist"`
		NamespaceResourceWhitelist []map[string]string `json:"namespaceResourceWhitelist"`
	} `json:"spec"`
}

type applicationSet struct {
	Spec struct {
		GoTemplate        bool     `json:"goTemplate"`
		GoTemplateOptions []string `json:"goTemplateOptions"`
		Generators        []struct {
			Git struct {
				RepoURL             string              `json:"repoURL"`
				Files               []map[string]string `json:"files"`
				RequeueAfterSeconds int                 `json:"requeueAfterSeconds"`
			} `json:"git"`
		} `json:"generators"`
		Template struct {
			Metadata struct {
				Name       string            `json:"name"`
				Labels     map[string]string `json:"labels"`
				Finalizers []string          `json:"finalizers"`
			} `json:"metadata"`
			Spec struct {
				Destination map[string]string `json:"destination"`
				SyncPolicy  struct {
					Automated   map[string]bool `json:"automated"`
					SyncOptions []string        `json:"syncOptions"`
				} `json:"syncPolicy"`
			} `json:"spec"`
		} `json:"template"`
		TemplatePatch string `json:"templatePatch"`
	} `json:"spec"`
}

// applicationPatch is the part of an Application that templatePatch sets.
type applicationPatch struct {
	Spec struct {
		Destination map[string]string `json:"destination"`
		Sources     []struct {
			RepoURL        string `json:"repoURL"`
			TargetRevision string `json:"targetRevision"`
			Path           string `json:"path"`
			Ref            string `json:"ref"`
			Helm           *struct {
				ReleaseName  string         `json:"releaseName"`
				ValueFiles   []string       `json:"valueFiles"`
				ValuesObject map[string]any `json:"valuesObject"`
			} `json:"helm"`
		} `json:"sources"`
	} `json:"spec"`
}

func TestAppProject(t *testing.T) {
	docs := mustRender(t, appsetChart, "argocd",
		filepath.Join(appsetChart, "ci", "eks-values.yaml"),
		writeValues(t, "environments:\n  kind:\n    cluster: {name: kind}\n"))
	var project appProject
	if err := yaml.Unmarshal(docs["AppProject/hopcharlie-apps"], &project); err != nil {
		t.Fatal(err)
	}
	ps := project.Spec
	if !slices.Equal(ps.SourceRepos, []string{
		"https://github.com/2026-Softbank-Hackathon-Charlie/infra.git",
		"https://github.com/2026-Softbank-Hackathon-Charlie/gitops.git",
	}) {
		t.Errorf("sourceRepos %v, want the chart's and the gitops repository", ps.SourceRepos)
	}
	// Any namespace but the system and platform ones, in each environment's cluster.
	var want []string
	for _, n := range []string{"argocd", "charlie-platform", "charlie-system", "cnpg-system", "default", "kube-node-lease", "kube-public", "kube-system", "observability"} {
		want = append(want, "!"+n)
	}
	want = append(want, "*")
	for _, cluster := range []map[string]string{{"server": "https://kubernetes.default.svc"}, {"name": "kind"}} {
		var got []string
		for _, d := range ps.Destinations {
			if d["server"] == cluster["server"] && d["name"] == cluster["name"] {
				got = append(got, d["namespace"])
			}
		}
		if !slices.Equal(got, want) {
			t.Errorf("destinations in %v: %v, want %v", cluster, got, want)
		}
	}
	// The app's own namespace (the chart renders it); nothing else cluster-scoped.
	if got := jsonStrings(ps.ClusterResourceWhitelist); !slices.Equal(got, []string{`{"group":"","kind":"Namespace"}`}) {
		t.Errorf("clusterResourceWhitelist %v, want Namespace only", got)
	}
	// Exactly what the chart renders.
	var kinds []string
	for _, k := range ps.NamespaceResourceWhitelist {
		kinds = append(kinds, k["kind"])
	}
	slices.Sort(kinds)
	if !slices.Equal(kinds, []string{"Cluster", "Deployment", "LimitRange", "NetworkPolicy", "ResourceQuota", "Service"}) {
		t.Errorf("namespaceResourceWhitelist %v", ps.NamespaceResourceWhitelist)
	}
}

// TestApplicationSet renders deploy/argocd, then does what Argo CD does with
// a gitops file: renders the ApplicationSet's template and templatePatch with
// the file as parameters, and renders the chart as the patched Application's
// sources say (the file as a values file, then the profile).
func TestApplicationSet(t *testing.T) {
	docs := mustRender(t, appsetChart, "argocd",
		filepath.Join(appsetChart, "ci", "eks-values.yaml"),
		writeValues(t, "environments:\n  kind:\n    cluster: {name: kind}\n    profile: {publicService: {type: NodePort}}\n"))
	var set applicationSet
	if err := yaml.Unmarshal(docs["ApplicationSet/hopcharlie-apps"], &set); err != nil {
		t.Fatal(err)
	}
	s := set.Spec
	if !s.GoTemplate || !slices.Contains(s.GoTemplateOptions, "missingkey=error") {
		t.Errorf("goTemplate %v %v", s.GoTemplate, s.GoTemplateOptions)
	}
	if len(s.Generators) != 1 || s.Generators[0].Git.RepoURL != "https://github.com/2026-Softbank-Hackathon-Charlie/gitops.git" ||
		s.Generators[0].Git.RequeueAfterSeconds != 30 {
		t.Fatalf("generators %+v", s.Generators)
	}
	var paths []string
	for _, f := range s.Generators[0].Git.Files {
		paths = append(paths, f["path"])
	}
	if !slices.Equal(paths, []string{"apps/*/eks.yaml", "apps/*/kind.yaml"}) {
		t.Errorf("files %v: one glob per environment, so another environment's file makes no Application", paths)
	}
	tm := s.Template
	if !slices.Contains(tm.Metadata.Finalizers, "resources-finalizer.argocd.argoproj.io") {
		t.Errorf("finalizers %v", tm.Metadata.Finalizers)
	}
	if !tm.Spec.SyncPolicy.Automated["prune"] || !tm.Spec.SyncPolicy.Automated["selfHeal"] ||
		slices.Contains(tm.Spec.SyncPolicy.SyncOptions, "CreateNamespace=true") {
		t.Errorf("syncPolicy %+v", tm.Spec.SyncPolicy)
	}

	for _, c := range []struct {
		env, instance string
		destination   map[string]string
		serviceType   corev1.ServiceType
	}{
		{"eks", "moimroom", map[string]string{"server": "https://kubernetes.default.svc"}, corev1.ServiceTypeLoadBalancer},
		// A later branch deploy: its own namespace, the same app.
		{"eks", "moimroom--feature-x", map[string]string{"server": "https://kubernetes.default.svc"}, corev1.ServiceTypeLoadBalancer},
		{"kind", "moimroom", map[string]string{"name": "kind"}, corev1.ServiceTypeNodePort},
	} {
		t.Run(c.env+"/"+c.instance, func(t *testing.T) {
			filePath := "apps/" + c.instance + "/" + c.env + ".yaml"
			content := gitopsFile(c.env, c.instance)
			params := argoParams(t, filePath, content)

			if got, want := argoRender(t, tm.Metadata.Name, params), c.instance+"-"+c.env; got != want {
				t.Errorf("Application %s, want %s", got, want)
			}
			if got := argoRender(t, tm.Spec.Destination["namespace"], params); got != c.instance {
				t.Errorf("destination namespace %s, want %s", got, c.instance)
			}
			if got := argoRender(t, tm.Metadata.Labels["hopcharlie.dev/app"], params); got != "moimroom" {
				t.Errorf("app label %s", got)
			}

			var patch applicationPatch
			out := argoRender(t, s.TemplatePatch, params)
			if err := yaml.UnmarshalStrict([]byte(out), &patch); err != nil {
				t.Fatalf("patch: %v\n%s", err, out)
			}
			if !mapsEqual(patch.Spec.Destination, c.destination) {
				t.Errorf("destination %v, want %v", patch.Spec.Destination, c.destination)
			}
			src := patch.Spec.Sources
			if len(src) != 2 || src[0].Helm == nil || src[1].Ref != "gitops" ||
				src[0].RepoURL != "https://github.com/2026-Softbank-Hackathon-Charlie/infra.git" || src[0].Path != "helm/charts/hopcharlie-app" ||
				src[1].RepoURL != "https://github.com/2026-Softbank-Hackathon-Charlie/gitops.git" || src[1].TargetRevision != "main" {
				t.Fatalf("sources %+v, want the chart and the gitops repository as $gitops", src)
			}
			if got, want := src[0].Helm.ValueFiles, []string{"$gitops/" + filePath}; !slices.Equal(got, want) {
				t.Errorf("valueFiles %v, want %v", got, want)
			}

			// The chart with the file and the profile, as Argo CD's repo server runs it.
			profile, _ := json.Marshal(src[0].Helm.ValuesObject)
			rendered, err := render(t, appChart, c.instance, writeValues(t, content), writeValues(t, string(profile)))
			if c.env == "kind" {
				// No s3 profile in this environment: the chart refuses the app.
				if err == nil || !strings.Contains(err.Error(), "profile.s3.endpoint") {
					t.Errorf("rendered an s3 app without an s3 profile: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var svc corev1.Service
			decode(t, rendered, "Service/moimroom-web", &svc)
			if svc.Spec.Type != c.serviceType {
				t.Errorf("public Service type %s, want %s", svc.Spec.Type, c.serviceType)
			}
			var d appsv1.Deployment
			decode(t, rendered, "Deployment/moimroom-web", &d)
			env := envByName(d.Spec.Template.Spec.Containers[0])
			if want := "hopcharlie-eks-" + c.instance + "-photos"; env["S3_BUCKET"].Value != want || env["KEY"].Value != "0755" {
				t.Errorf("S3_BUCKET %q (want %q), KEY %q", env["S3_BUCKET"].Value, want, env["KEY"].Value)
			}
			if got := secretRef(t, env["DATABASE_URL"]); got != "moimroom-db-app/uri" {
				t.Errorf("DATABASE_URL from %s", got)
			}
			if _, ok := rendered["NetworkPolicy/hopcharlie-app"]; !ok {
				t.Errorf("no NetworkPolicy; rendered %v", keys(rendered))
			}
			var ns corev1.Namespace
			decode(t, rendered, "Namespace/"+c.instance, &ns)
			if ns.Labels["hopcharlie.dev/app"] != "moimroom" || ns.Labels["hopcharlie.dev/instance"] != c.instance {
				t.Errorf("namespace labels %v", ns.Labels)
			}
		})
	}
}

func mapsEqual[V comparable](a, b map[string]V) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}
