package appspec

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"

	specdoc "github.com/2026-Softbank-Hackathon-Charlie/platform/api"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

const full = `
name: moimroom
services:
  - name: web
    path: web
    dockerfile: deploy/Dockerfile
    port: 8080
    healthCheck: /healthz
    public: true
    replicas: 2
    env:
      LOG_LEVEL: info
      FEATURE_X: "true"
  - name: worker
    port: 9000
  - name: jobs
    memory: 512Mi
    cpu: 250m
resources:
  - name: db
    type: postgres
  - name: photos
    type: s3
`

func TestParse(t *testing.T) {
	s, err := Parse([]byte(full))
	if err != nil {
		t.Fatal(err)
	}
	want := Spec{
		Name: "moimroom",
		Services: []Service{
			{Name: "web", Path: "web", Dockerfile: "deploy/Dockerfile", Port: 8080, HealthCheck: "/healthz", Public: true, Replicas: 2,
				Env: map[string]string{"LOG_LEVEL": "info", "FEATURE_X": "true"}},
			// Defaults: path ".", dockerfile "Dockerfile", replicas 1.
			{Name: "worker", Path: ".", Dockerfile: "Dockerfile", Port: 9000, Replicas: 1},
			// No port: a worker.
			{Name: "jobs", Path: ".", Dockerfile: "Dockerfile", Replicas: 1, Memory: "512Mi", CPU: "250m"},
		},
		Resources: []Resource{{Name: "db", Type: TypePostgres}, {Name: "photos", Type: TypeS3}},
	}
	if !reflect.DeepEqual(s, want) {
		t.Fatalf("Parse =\n%+v\nwant\n%+v", s, want)
	}
	if errs := s.Validate(); errs != nil {
		t.Fatalf("Validate = %v, want nil", errs)
	}

	// No services at all still gives an empty list, not nil, so the JSON is
	// an array as the API's AppSpec requires.
	s, err = Parse([]byte("name: x\n"))
	if err != nil || s.Services == nil || len(s.Services) != 0 || s.Resources != nil {
		t.Fatalf("Parse(name only) = %+v, %v", s, err)
	}
	// A comment-only file decodes to nothing; Validate reports what is missing.
	s, err = Parse([]byte("# nothing yet\n"))
	if err != nil || s.Name != "" {
		t.Fatalf("Parse(comment) = %+v, %v", s, err)
	}
}

func TestParseErrors(t *testing.T) {
	for _, c := range []struct {
		name, in, want string
	}{
		{"empty", "", "platform.yaml is empty"},
		{"blank", " \n\t\n", "platform.yaml is empty"},
		{"too large", "name: x\n" + strings.Repeat("#", MaxSize), "at most 65536 are read"},
		{"unknown top-level key", "name: x\nversion: 2\n", `unknown field "version"`},
		{"unknown service key", "name: x\nservices:\n  - name: web\n    port: 80\n    image: nginx\n", `unknown field "image"`},
		{"unknown resource key", "name: x\nresources:\n  - name: db\n    type: postgres\n    size: 10Gi\n", `unknown field "size"`},
		{"duplicate key", "name: x\nname: y\n", `key "name" already set`},
		{"port not a number", "name: x\nservices:\n  - name: web\n    port: http\n", "cannot unmarshal"},
		{"env not a mapping", "name: x\nservices:\n  - name: web\n    port: 80\n    env: [A]\n", "cannot unmarshal"},
		{"not a mapping", "- name: x\n", "cannot unmarshal"},
		{"syntax", "name: [x\n", "yaml:"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := Parse([]byte(c.in))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("Parse error = %v, want it to contain %q", err, c.want)
			}
			if !strings.HasPrefix(err.Error(), FileName) {
				t.Errorf("error %q does not name %s", err, FileName)
			}
			for _, noise := range []string{"error converting YAML to JSON", "error unmarshaling JSON", "while decoding JSON"} {
				if strings.Contains(err.Error(), noise) {
					t.Errorf("error %q keeps the library prefix %q", err, noise)
				}
			}
		})
	}
	if _, err := Parse([]byte("name: x\n" + strings.Repeat("#", MaxSize-len("name: x\n")))); err != nil {
		t.Fatalf("a file of exactly MaxSize bytes: %v", err)
	}
}

// YAML reads some plain scalars as numbers, booleans or null; as env values
// they would reach the app changed, so only strings are taken.
func TestParseEnvValuesMustBeStrings(t *testing.T) {
	for _, v := range []string{"4", "true", "0755", "yes", "1.10", "~", "null", "0x1F", "1e3", ""} {
		in := "name: x\nservices:\n  - name: web\n    port: 80\n    env:\n      A_OK: fine\n      WORKERS: " + v + "\n"
		_, err := Parse([]byte(in))
		var fe FieldError
		if !errors.As(err, &fe) || fe.Path != "services[0].env.WORKERS" || !strings.Contains(fe.Message, "must be a string") {
			t.Errorf("WORKERS: %s: err = %v, want a FieldError at services[0].env.WORKERS", v, err)
		}
		if spec, errs := Check([]byte(in)); spec != nil || len(errs) != 1 || errs[0] != fe {
			t.Errorf("WORKERS: %s: Check = %v, %v, want no spec and %v", v, spec, errs, fe)
		}
	}
	s, err := Parse([]byte("name: x\nservices:\n  - name: web\n    port: 80\n    env:\n      A: \"0755\"\n      B: 'yes'\n      C: \"\"\n      D: plain text\n"))
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]string{"A": "0755", "B": "yes", "C": "", "D": "plain text"}; !reflect.DeepEqual(s.Services[0].Env, want) {
		t.Fatalf("env = %v, want %v", s.Services[0].Env, want)
	}
}

// valid returns a spec that passes; each case breaks one thing.
func valid() Spec {
	return Spec{
		Name: "shop",
		Services: []Service{
			{Name: "web", Path: ".", Dockerfile: "Dockerfile", Port: 8080, Public: true, Replicas: 1, HealthCheck: "/healthz"},
			{Name: "api", Path: "services/api", Dockerfile: "Dockerfile", Port: 9000, Replicas: 1},
		},
		Resources: []Resource{{Name: "db", Type: TypePostgres}},
	}
}

func TestValidate(t *testing.T) {
	for _, c := range []struct {
		name string
		edit func(s *Spec)
		want []string // "path: message substring"
	}{
		{"valid", func(*Spec) {}, nil},
		{"name missing", func(s *Spec) { s.Name = "" }, []string{"name: is required"}},
		{"name uppercase", func(s *Spec) { s.Name = "Shop" }, []string{"name: must be 3 to 32"}},
		{"name leading hyphen", func(s *Spec) { s.Name = "-shop" }, []string{"name: must be"}},
		{"name trailing hyphen", func(s *Spec) { s.Name = "shop-" }, []string{"name: must be"}},
		{"name underscore", func(s *Spec) { s.Name = "my_shop" }, []string{"name: must be"}},
		{"name 32 characters", func(s *Spec) { s.Name = strings.Repeat("a", 32) }, nil},
		{"name 33 characters", func(s *Spec) { s.Name = strings.Repeat("a", 33) }, []string{"name: must be"}},
		{"name one character", func(s *Spec) { s.Name = "a" }, []string{"name: must be 3 to 32"}},
		{"name two characters", func(s *Spec) { s.Name = "ab" }, []string{"name: must be 3 to 32"}},
		{"name three characters", func(s *Spec) { s.Name = "abc" }, nil},
		{"name with --", func(s *Spec) { s.Name = "a--b" }, []string{"name: must not contain --"}},
		{"name with a single hyphen", func(s *Spec) { s.Name = "a-b" }, nil},
		{"name starts with a digit", func(s *Spec) { s.Name = "1shop" }, nil},
		{"no services", func(s *Spec) { s.Services = nil; s.Resources = nil }, []string{"services: needs at least one service"}},
		{"five services", func(s *Spec) { s.Services = services(5) }, nil},
		{"six services", func(s *Spec) { s.Services = services(6) }, []string{"services: has 6 services; at most 5"}},
		{"service name missing", func(s *Spec) { s.Services[1].Name = "" }, []string{"services[1].name: is required"}},
		{"service name starts with a digit", func(s *Spec) { s.Services[1].Name = "2fa" }, []string{"services[1].name: must be 1 to 24"}},
		{"service name dot", func(s *Spec) { s.Services[1].Name = "api.v2" }, []string{"services[1].name: must be"}},
		{"service name 24 characters", func(s *Spec) { s.Services[1].Name = "a" + strings.Repeat("b", 23) }, nil},
		{"service name 25 characters", func(s *Spec) { s.Services[1].Name = "a" + strings.Repeat("b", 24) }, []string{"services[1].name: must be"}},
		{"service names repeat", func(s *Spec) { s.Services[1].Name = "web" }, []string{`services[1].name: "web" is also the name of services[0]`}},
		{"absolute path", func(s *Spec) { s.Services[1].Path = "/srv/api" }, []string{"services[1].path: must be a relative path"}},
		{"backslash path", func(s *Spec) { s.Services[1].Path = `services\api` }, []string{"services[1].path: must be a relative path"}},
		{"path with ..", func(s *Spec) { s.Services[1].Path = "services/../../etc" }, []string{"services[1].path: must not contain .."}},
		{"path ..", func(s *Spec) { s.Services[1].Path = ".." }, []string{"services[1].path: must not contain .."}},
		{"path with dots in a name", func(s *Spec) { s.Services[1].Path = "services/..api/v1.." }, nil},
		{"path ./", func(s *Spec) { s.Services[1].Path = "./services/api/" }, nil},
		{"absolute dockerfile", func(s *Spec) { s.Services[0].Dockerfile = "/Dockerfile" }, []string{"services[0].dockerfile: must be a relative path"}},
		{"dockerfile with ..", func(s *Spec) { s.Services[0].Dockerfile = "../Dockerfile" }, []string{"services[0].dockerfile: must not contain .."}},
		{"empty path and dockerfile are the defaults", func(s *Spec) { s.Services[1].Path, s.Services[1].Dockerfile = "", "" }, nil},
		{"worker without a port", func(s *Spec) { s.Services[1].Port = 0 }, nil},
		{"public service without a port", func(s *Spec) { s.Services[0].Port, s.Services[0].HealthCheck = 0, "" },
			[]string{"services[0].public: needs a port: the traffic gateway sends requests to it"}},
		{"health check without a port", func(s *Spec) { s.Services[1].Port, s.Services[1].HealthCheck = 0, "/healthz" },
			[]string{"services[1].healthCheck: needs a port: the probes request the path on it"}},
		{"port missing on a public service with a health check", func(s *Spec) { s.Services[0].Port = 0 },
			[]string{"services[0].healthCheck: needs a port", "services[0].public: needs a port"}},
		{"only workers", func(s *Spec) {
			s.Services[0].Port, s.Services[0].Public, s.Services[0].HealthCheck = 0, false, ""
			s.Services[1].Port = 0
		}, nil},
		{"port negative", func(s *Spec) { s.Services[0].Port = -1 }, []string{"services[0].port: must be between 1 and 65535"}},
		{"port too large", func(s *Spec) { s.Services[0].Port = 65536 }, []string{"services[0].port: must be between 1 and 65535"}},
		{"port 1 and 65535", func(s *Spec) { s.Services[0].Port, s.Services[1].Port = 1, 65535 }, nil},
		{"two public services", func(s *Spec) { s.Services[1].Public = true }, []string{"services[1].public: only one service may be public; services[0] is"}},
		{"no public service", func(s *Spec) { s.Services[0].Public = false }, nil},
		{"replicas 5", func(s *Spec) { s.Services[0].Replicas = 5 }, nil},
		{"replicas 6", func(s *Spec) { s.Services[0].Replicas = 6 }, []string{"services[0].replicas: must be between 1 and 5"}},
		{"replicas negative", func(s *Spec) { s.Services[0].Replicas = -1 }, []string{"services[0].replicas: must be between 1 and 5"}},
		{"replicas 0 is the default", func(s *Spec) { s.Services[0].Replicas = 0 }, nil},
		{"memory and cpu", func(s *Spec) { s.Services[0].Memory, s.Services[0].CPU = "1Gi", "500m" }, nil},
		{"memory at the bounds", func(s *Spec) { s.Services[0].Memory, s.Services[1].Memory = "256Mi", "1Gi" }, nil},
		{"one replica of 1.5Gi fills the total", func(s *Spec) { s.Services = s.Services[:1]; s.Services[0].Memory = "1.5Gi" }, nil},
		{"cpu as cores", func(s *Spec) { s.Services[0].CPU, s.Services[1].CPU = "0.5", "0.05" }, nil},
		{"memory below 256Mi", func(s *Spec) { s.Services[0].Memory = "255Mi" }, []string{"services[0].memory: must be between 256Mi and 1536Mi"}},
		{"memory above 1536Mi", func(s *Spec) { s.Services[0].Memory = "1537Mi" }, []string{"services[0].memory: must be between 256Mi and 1536Mi"}},
		{"memory 2Gi", func(s *Spec) { s.Services[0].Memory = "2Gi" }, []string{"services[0].memory: must be between 256Mi and 1536Mi"}},
		{"memory negative", func(s *Spec) { s.Services[0].Memory = "-1Gi" }, []string{"services[0].memory: must be between"}},
		{"memory in MB", func(s *Spec) { s.Services[0].Memory = "512MB" }, []string{"services[0].memory: must be a Kubernetes quantity such as 512Mi or 1Gi"}},
		{"memory a word", func(s *Spec) { s.Services[0].Memory = "lots" }, []string{"services[0].memory: must be a Kubernetes quantity"}},
		{"cpu below 50m", func(s *Spec) { s.Services[1].CPU = "49m" }, []string{"services[1].cpu: must be between 50m and 500m"}},
		{"cpu 1", func(s *Spec) { s.Services[1].CPU = "1" }, []string{"services[1].cpu: must be between 50m and 500m"}},
		{"cpu not a quantity", func(s *Spec) { s.Services[1].CPU = "half" }, []string{"services[1].cpu: must be a Kubernetes quantity such as 250m or 0.5"}},
		// Whole bytes and millicores only, as the chart and Kubernetes write them.
		{"cpu 50.5m", func(s *Spec) { s.Services[1].CPU = "50.5m" }, []string{"services[1].cpu: must be a whole number of millicores, such as 250m or 0.5"}},
		{"cpu 0.0505", func(s *Spec) { s.Services[1].CPU = "0.0505" }, []string{"services[1].cpu: must be a whole number of millicores"}},
		{"cpu 0.0501", func(s *Spec) { s.Services[1].CPU = "0.0501" }, []string{"services[1].cpu: must be a whole number of millicores"}},
		{"memory 0.3Gi", func(s *Spec) { s.Services[0].Memory = "0.3Gi" }, []string{"services[0].memory: must be a whole number of bytes, such as 512Mi or 1Gi"}},
		{"memory 1e9 bytes", func(s *Spec) { s.Services[0].Memory = "1e+09" }, nil},
		// Values past int64 must not wrap around into the range.
		{"cpu that wraps to 250m", func(s *Spec) { s.Services[1].CPU = "18446744073709551866m" }, []string{"services[1].cpu: must be between 50m and 500m"}},
		{"memory that wraps to 512Mi", func(s *Spec) { s.Services[0].Memory = "18446744074246422528" }, []string{"services[0].memory: must be between 256Mi and 1536Mi"}},
		{"no total with wrong replicas", func(s *Spec) { s.Services[0].Memory, s.Services[0].Replicas = "1Gi", 9 },
			[]string{"services[0].replicas: must be between 1 and 5"}},
		// (replicas + 1) x memory: one pod more per Deployment while it rolls out.
		{"memory total 3Gi", func(s *Spec) { s.Services[0].Memory, s.Services[1].Memory = "1Gi", "512Mi" }, nil},
		{"memory total over 3Gi", func(s *Spec) { s.Services[0].Memory, s.Services[1].Memory = "1Gi", "513Mi" },
			[]string{"services: need at most 3Gi of memory during a rolling update: the sum of (replicas + 1) x memory, counting an unset memory as 256Mi, is 3074Mi"}},
		{"max memory with 2 replicas", func(s *Spec) { s.Services[0].Memory, s.Services[0].Replicas = "1536Mi", 2 },
			[]string{"services: need at most 3Gi of memory during a rolling update: the sum of (replicas + 1) x memory, counting an unset memory as 256Mi, is 5Gi"}},
		{"twelve pods of unset memory", func(s *Spec) { s.Services = services(4); s.Services[0].Replicas = 5 }, nil},
		{"fourteen pods of unset memory", func(s *Spec) { s.Services = services(5); s.Services[0].Replicas = 5 },
			[]string{"services: need at most 3Gi of memory during a rolling update: the sum of (replicas + 1) x memory, counting an unset memory as 256Mi, is 3584Mi"}},
		{"cpu total 1500m", func(s *Spec) { s.Services[0].CPU, s.Services[0].Replicas, s.Services[1].CPU = "450m", 2, "50m" }, nil},
		{"cpu total over 1500m", func(s *Spec) { s.Services[0].CPU, s.Services[0].Replicas, s.Services[1].CPU = "500m", 2, "100m" },
			[]string{"services: need at most 1500m of CPU during a rolling update: the sum of (replicas + 1) x cpu, counting an unset cpu as 50m, is 1700m"}},
		{"no total with a wrong memory", func(s *Spec) { s.Services[0].Memory, s.Services[0].Replicas, s.Services[1].Memory = "1536Mi", 5, "9Gi" },
			[]string{"services[1].memory: must be between"}},
		{"health check without slash", func(s *Spec) { s.Services[0].HealthCheck = "healthz" }, []string{"services[0].healthCheck: must be an HTTP path"}},
		{"health check with a space", func(s *Spec) { s.Services[0].HealthCheck = "/health z" }, []string{"services[0].healthCheck: must be an HTTP path"}},
		{"health check with a query", func(s *Spec) { s.Services[0].HealthCheck = "/healthz?full=1" }, nil},
		{"env keys", func(s *Spec) {
			s.Services[0].Env = map[string]string{"LOG_LEVEL": "info", "_X": "", "A1": "x", "FEATURE_FLAGS_2": "a,b"}
		}, nil},
		{"env key lowercase", func(s *Spec) { s.Services[0].Env = map[string]string{"log_level": "info"} }, []string{"services[0].env.log_level: must match"}},
		{"env key starts with a digit", func(s *Spec) { s.Services[0].Env = map[string]string{"1X": "x"} }, []string{"services[0].env.1X: must match"}},
		{"env key with a hyphen", func(s *Spec) { s.Services[0].Env = map[string]string{"LOG-LEVEL": "x"} }, []string{"services[0].env.LOG-LEVEL: must match"}},
		{"env key empty", func(s *Spec) { s.Services[0].Env = map[string]string{"": "x"} }, []string{"services[0].env.: must match"}},
		{"env sets PORT", func(s *Spec) { s.Services[0].Env = map[string]string{"PORT": "80"} }, []string{"services[0].env.PORT: is set by the platform"}},
		{"env sets DATABASE_URL", func(s *Spec) { s.Services[0].Env = map[string]string{"DATABASE_URL": "postgres://x"} }, []string{"services[0].env.DATABASE_URL: is set by the platform"}},
		{"env sets the database parts", func(s *Spec) {
			s.Services[0].Env = map[string]string{"PGHOST": "h", "PGPORT": "1", "PGDATABASE": "d", "PGUSER": "u", "PGPASSWORD": "p", "JDBC_DATABASE_URL": "j"}
		}, []string{
			"services[0].env.JDBC_DATABASE_URL: is set by the platform (PORT, DATABASE_URL, PGHOST, PGPORT, PGDATABASE, PGUSER, PGPASSWORD, JDBC_DATABASE_URL and S3_* are reserved)",
			"services[0].env.PGDATABASE: is set by the platform", "services[0].env.PGHOST: is set by the platform", "services[0].env.PGPASSWORD: is set by the platform",
			"services[0].env.PGPORT: is set by the platform", "services[0].env.PGUSER: is set by the platform",
		}},
		{"env maps the database variables", func(s *Spec) {
			s.Services[0].Env = map[string]string{"PGSSLMODE": "require", "KC_DB_URL_HOST": "$(PGHOST)", "SPRING_DATASOURCE_URL": "$(JDBC_DATABASE_URL)", "AWS_ACCESS_KEY_ID": "$(S3_ACCESS_KEY)"}
		}, nil},
		{"env sets S3 variables", func(s *Spec) {
			s.Services[1].Env = map[string]string{"S3_BUCKET": "b", "S3_ANYTHING": "x", "S3": "ok", "MY_S3_BUCKET": "ok"}
		}, []string{"services[1].env.S3_ANYTHING: is set by the platform", "services[1].env.S3_BUCKET: is set by the platform"}},
		{"no resources", func(s *Spec) { s.Resources = nil }, nil},
		{"postgres and s3", func(s *Spec) { s.Resources = append(s.Resources, Resource{Name: "photos", Type: TypeS3}) }, nil},
		{"two postgres", func(s *Spec) { s.Resources = append(s.Resources, Resource{Name: "cache", Type: TypePostgres}) },
			[]string{"resources[1].type: only one postgres resource is allowed; resources[0] is one"}},
		{"two s3", func(s *Spec) {
			s.Resources = []Resource{{Name: "a", Type: TypeS3}, {Name: "b", Type: TypeS3}}
		}, []string{"resources[1].type: only one s3 resource is allowed"}},
		{"unknown type", func(s *Spec) { s.Resources[0].Type = "mysql" }, []string{"resources[0].type: must be postgres or s3"}},
		{"type missing", func(s *Spec) { s.Resources[0].Type = "" }, []string{"resources[0].type: is required"}},
		{"resource name missing", func(s *Spec) { s.Resources[0].Name = "" }, []string{"resources[0].name: is required"}},
		{"resource name invalid", func(s *Spec) { s.Resources[0].Name = "DB" }, []string{"resources[0].name: must be"}},
		{"resource names repeat", func(s *Spec) {
			s.Resources = []Resource{{Name: "data", Type: TypePostgres}, {Name: "data", Type: TypeS3}}
		}, []string{`resources[1].name: "data" is also the name of resources[0]`}},
		{"a resource may share a service's name", func(s *Spec) { s.Resources[0].Name = "web" }, nil},
		{"several at once, in field order", func(s *Spec) {
			s.Name = ""
			s.Services[0].Port = 70000
			s.Services[1].Public = true
			s.Services[1].Memory = "4Gi"
			s.Resources[0].Type = "redis"
		}, []string{"name: is required", "services[0].port: must be between", "services[1].public: only one", "services[1].memory: must be between", "resources[0].type: must be"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := valid()
			c.edit(&s)
			got := s.Validate()
			if len(got) != len(c.want) {
				t.Fatalf("Validate = %v, want %d errors %v", got, len(c.want), c.want)
			}
			for i, w := range c.want {
				if !strings.HasPrefix(got[i].Error(), w) {
					t.Errorf("error %d = %q, want it to start with %q", i, got[i].Error(), w)
				}
			}
		})
	}
}

func services(n int) []Service {
	out := make([]Service, n)
	for i := range out {
		out[i] = Service{Name: "s" + string(rune('a'+i)), Path: ".", Dockerfile: "Dockerfile", Port: 8000 + i, Replicas: 1}
	}
	return out
}

func TestCheck(t *testing.T) {
	s, errs := Check([]byte(full))
	if s == nil || s.Name != "moimroom" || len(errs) != 0 {
		t.Fatalf("Check(valid) = %v, %v", s, errs)
	}

	s, errs = Check([]byte("name: shop\nservices:\n  - name: web\n    public: true\n"))
	if s == nil || s.Services[0].Replicas != 1 {
		t.Fatalf("Check(invalid) spec = %v, want the decoded spec with defaults", s)
	}
	if want := []FieldError{{Path: "services[0].public", Message: "needs a port: the traffic gateway sends requests to it"}}; !reflect.DeepEqual(errs, want) {
		t.Fatalf("Check(invalid) errors = %v, want %v", errs, want)
	}

	s, errs = Check([]byte("name: x\nreplicas: 2\n"))
	if s != nil || len(errs) != 1 || errs[0].Path != "" || !strings.Contains(errs[0].Message, `unknown field "replicas"`) {
		t.Fatalf("Check(undecodable) = %v, %v", s, errs)
	}
	if errs[0].Error() != errs[0].Message {
		t.Errorf("a file-wide error prints %q, want the bare message", errs[0].Error())
	}
}

// The gitops file gets the checked value in Kubernetes' form, never the
// text as written: YAML turns an unquoted 1e9 into "1e+09".
func TestQuantities(t *testing.T) {
	s, err := Parse([]byte("name: x\nservices:\n  - name: jobs\n    memory: 1e9\n    cpu: 0.25\n"))
	if err != nil {
		t.Fatal(err)
	}
	if sv := s.Services[0]; sv.Memory != "1e+09" || sv.CPU != "0.25" {
		t.Fatalf("decoded memory %q, cpu %q", sv.Memory, sv.CPU)
	}
	for _, c := range []struct{ memory, cpu, wantMemory, wantCPU string }{
		{"1e+09", "0.25", "1000000000", "250m"},
		{"1.5Gi", "0.5", "1536Mi", "500m"},
		{"256Mi", "50m", "256Mi", "50m"},
		{"", "", "", ""},
	} {
		m, cpu, err := (Service{Memory: c.memory, CPU: c.cpu}).Quantities()
		if err != nil || m != c.wantMemory || cpu != c.wantCPU {
			t.Errorf("Quantities(%q, %q) = %q, %q, %v; want %q, %q", c.memory, c.cpu, m, cpu, err, c.wantMemory, c.wantCPU)
		}
	}
	for _, sv := range []Service{{Memory: "0.3Gi"}, {CPU: "50.5m"}, {CPU: "18446744073709551866m"}, {Memory: "18446744074246422528"}, {Memory: "lots"}} {
		if m, cpu, err := sv.Quantities(); err == nil {
			t.Errorf("Quantities(%q, %q) = %q, %q; want an error", sv.Memory, sv.CPU, m, cpu)
		}
	}
}

func TestReserved(t *testing.T) {
	for name, want := range map[string]bool{
		EnvPort: true, EnvDatabaseURL: true,
		EnvS3Endpoint: true, EnvS3Bucket: true, EnvS3Region: true, EnvS3PathStyle: true, EnvS3AccessKey: true, EnvS3SecretKey: true,
		"S3_FUTURE": true,
		"PGHOST":    true, "PGPORT": true, "PGDATABASE": true, "PGUSER": true, "PGPASSWORD": true, "JDBC_DATABASE_URL": true,
		"PORTS": false, "DATABASE_URL_RO": false, "S3": false, "MY_S3_BUCKET": false, "port": false,
		// Only the six names: the app sets the rest of libpq's, and AWS_* is never set.
		"PGSSLMODE": false, "PGOPTIONS": false, "PG": false, "JDBC_URL": false, "AWS_ACCESS_KEY_ID": false, "AWS_SECRET_ACCESS_KEY": false,
	} {
		if Reserved(name) != want {
			t.Errorf("Reserved(%q) = %v, want %v", name, !want, want)
		}
	}
}

func TestDockerfilePath(t *testing.T) {
	for _, c := range []struct{ path, dockerfile, want string }{
		{".", "Dockerfile", "Dockerfile"},
		{"web", "Dockerfile", "web/Dockerfile"},
		{"./web/", "deploy/Dockerfile.prod", "web/deploy/Dockerfile.prod"},
	} {
		if got := (Service{Path: c.path, Dockerfile: c.dockerfile}).DockerfilePath(); got != c.want {
			t.Errorf("DockerfilePath(%q, %q) = %q, want %q", c.path, c.dockerfile, got, c.want)
		}
	}
}

// The server turns a Spec into the API's AppSpec and FieldErrors into
// AppSpecFieldErrors by their JSON. Both must match api/openapi.yaml, also
// for a spec that breaks the rules (AppSpecValidation returns those too).
func TestJSONMatchesAPI(t *testing.T) {
	doc, err := openapi3.NewLoader().LoadFromData(specdoc.OpenAPI)
	if err != nil {
		t.Fatal(err)
	}
	if err := doc.Validate(context.Background()); err != nil {
		t.Fatal(err)
	}
	visit := func(schema string, v any) {
		t.Helper()
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		var generic any
		if err := json.Unmarshal(b, &generic); err != nil {
			t.Fatal(err)
		}
		if err := doc.Components.Schemas[schema].Value.VisitJSON(generic); err != nil {
			t.Errorf("%s does not match %s: %v", b, schema, err)
		}
	}

	for _, in := range []string{
		full,
		"name: x\n",
		"name: Bad\nservices:\n  - name: web\n    port: 70000\n    replicas: 9\nresources:\n  - name: db\n    type: mysql\n",
		"name: x\nservices:\n  - name: jobs\n    public: true\n    memory: lots\n    cpu: 2\n",
	} {
		s, errs := Check([]byte(in))
		visit("AppSpec", s)
		for _, e := range errs {
			visit("AppSpecFieldError", e)
		}

		// The same JSON decodes into apiv1.AppSpec and encodes back unchanged.
		b, _ := json.Marshal(s)
		var api apiv1.AppSpec
		if err := json.Unmarshal(b, &api); err != nil {
			t.Fatal(err)
		}
		back, _ := json.Marshal(api)
		var x, y any
		_ = json.Unmarshal(b, &x)
		_ = json.Unmarshal(back, &y)
		if !reflect.DeepEqual(x, y) {
			t.Errorf("apiv1.AppSpec round trip changed\n%s\nto\n%s", b, back)
		}
	}
}
