// Package appspec reads platform.yaml, the file at the root of an app's
// repository that tells the HopCharlie platform how to build and run the app
// (AppSpec in api/openapi.yaml). The server checks it when an app is
// registered and before every deployment; the CLI checks it locally with the
// same code.
//
// A spec names the app, lists its services (each built from a Dockerfile in
// the repository) and the resources the platform provisions for it. The app
// chart injects PORT into every service, DATABASE_URL for a postgres
// resource and the S3_* variables for an s3 resource.
package appspec

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"
)

// FileName is the spec's name; it lives at the repository root.
const FileName = "platform.yaml"

// MaxSize is the largest platform.yaml the platform reads, in bytes.
const MaxSize = 64 << 10

// Limits and defaults.
const (
	MaxServices = 5
	MaxReplicas = 5

	DefaultPath       = "."
	DefaultDockerfile = "Dockerfile"
	DefaultReplicas   = 1
)

// Resource types.
const (
	TypePostgres = "postgres"
	TypeS3       = "s3"
)

// The variables the platform sets in every service. An app's env may not set
// them, and no S3_ variable at all (EnvS3Prefix), so a later one cannot clash.
const (
	// EnvPort is the service's port.
	EnvPort = "PORT"
	// EnvDatabaseURL is key uri of the secret <app>-<resource>-app of the
	// postgres resource's CNPG Cluster <app>-<resource>.
	EnvDatabaseURL = "DATABASE_URL"
	// The s3 resource's bucket and credentials.
	EnvS3Endpoint  = "S3_ENDPOINT"
	EnvS3Bucket    = "S3_BUCKET"
	EnvS3Region    = "S3_REGION"
	EnvS3PathStyle = "S3_PATH_STYLE"
	EnvS3AccessKey = "S3_ACCESS_KEY"
	EnvS3SecretKey = "S3_SECRET_KEY"

	EnvS3Prefix = "S3_"
)

// Reserved reports whether the platform sets the variable name, so that an
// app's env may not.
func Reserved(name string) bool {
	return name == EnvPort || name == EnvDatabaseURL || strings.HasPrefix(name, EnvS3Prefix)
}

// Spec is platform.yaml. Its JSON is the API's AppSpec.
type Spec struct {
	Name      string     `json:"name" yaml:"name"`
	Services  []Service  `json:"services" yaml:"services"`
	Resources []Resource `json:"resources,omitempty" yaml:"resources,omitempty"`
}

// Service is one container of the app, built from a Dockerfile.
type Service struct {
	Name string `json:"name" yaml:"name"`
	// Path is the docker build context, relative to the repository root.
	Path string `json:"path" yaml:"path,omitempty"`
	// Dockerfile is relative to Path (DockerfilePath).
	Dockerfile string `json:"dockerfile" yaml:"dockerfile,omitempty"`
	// Port is where the service listens; it also gets it as PORT.
	Port int `json:"port" yaml:"port"`
	// HealthCheck is an HTTP path that answers 2xx once the service is ready.
	HealthCheck string `json:"healthCheck,omitempty" yaml:"healthCheck,omitempty"`
	// Public serves the service at https://<app>.hopcharlie.dev. At most one service is public.
	Public   bool `json:"public" yaml:"public,omitempty"`
	Replicas int  `json:"replicas" yaml:"replicas,omitempty"`
	// Env holds variables besides the ones the platform sets (Reserved).
	Env map[string]string `json:"env,omitempty" yaml:"env,omitempty"`
}

// DockerfilePath is the Dockerfile's path from the repository root.
func (s Service) DockerfilePath() string { return path.Join(s.Path, s.Dockerfile) }

// Resource is something the platform provisions for the app.
type Resource struct {
	Name string `json:"name" yaml:"name"`
	Type string `json:"type" yaml:"type"` // TypePostgres or TypeS3
}

// FieldError is one broken rule. Its JSON is the API's AppSpecFieldError.
type FieldError struct {
	// Path is where in the file, like "services[0].port"; empty for the file
	// as a whole.
	Path    string `json:"path"`
	Message string `json:"message"`
}

func (e FieldError) Error() string {
	if e.Path == "" {
		return e.Message
	}
	return e.Path + ": " + e.Message
}

// Parse decodes platform.yaml strictly (unknown keys, duplicate keys and
// values of the wrong type are errors) and applies the defaults: path ".",
// dockerfile "Dockerfile" and, for 0, replicas 1. It does not check the
// rules; see Validate. An env value that is not a string is a FieldError.
func Parse(data []byte) (Spec, error) {
	if len(data) > MaxSize {
		return Spec{}, fmt.Errorf("%s is %d bytes; at most %d are read", FileName, len(data), MaxSize)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return Spec{}, errors.New(FileName + " is empty")
	}
	var s Spec
	if err := yaml.UnmarshalStrict(data, &s); err != nil {
		return Spec{}, fmt.Errorf("%s: %s", FileName, decodeMessage(err))
	}
	if err := checkEnvStrings(data); err != nil {
		return Spec{}, err
	}
	if s.Services == nil {
		s.Services = []Service{}
	}
	for i := range s.Services {
		sv := &s.Services[i]
		if sv.Path == "" {
			sv.Path = DefaultPath
		}
		if sv.Dockerfile == "" {
			sv.Dockerfile = DefaultDockerfile
		}
		if sv.Replicas == 0 {
			sv.Replicas = DefaultReplicas
		}
	}
	return s, nil
}

// checkEnvStrings refuses env values that YAML reads as numbers, booleans or
// null. Decoding into a string would take them, changed: 0755 becomes "493",
// yes "true" and 1.10 "1.1". Call it after the strict decode succeeded.
func checkEnvStrings(data []byte) error {
	j, err := yaml.YAMLToJSON(data)
	if err != nil {
		return fmt.Errorf("%s: %s", FileName, decodeMessage(err))
	}
	var raw struct {
		Services []struct {
			Env map[string]any `json:"env"`
		} `json:"services"`
	}
	if err := json.Unmarshal(j, &raw); err != nil {
		return fmt.Errorf("%s: %s", FileName, decodeMessage(err))
	}
	for i, sv := range raw.Services {
		for _, k := range sortedKeys(sv.Env) {
			if _, ok := sv.Env[k].(string); !ok {
				return FieldError{
					Path:    fmt.Sprintf("services[%d].env.%s", i, k),
					Message: "must be a string; quote it (YAML reads 0755 as 493 and yes as true)",
				}
			}
		}
	}
	return nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// decodeMessage drops the YAML library's layer prefixes, which name its
// internals rather than the file.
func decodeMessage(err error) string {
	msg := err.Error()
	for _, p := range []string{"error converting YAML to JSON: ", "error unmarshaling JSON: ", "while decoding JSON: "} {
		msg = strings.TrimPrefix(msg, p)
	}
	return msg
}

// Check parses and validates platform.yaml. The spec is nil when the file
// does not decode; the errors are empty when it is valid. A file that does
// not decode gives one FieldError, with an empty Path unless Parse named one.
func Check(data []byte) (*Spec, []FieldError) {
	s, err := Parse(data)
	if err != nil {
		var fe FieldError
		if !errors.As(err, &fe) {
			fe = FieldError{Message: err.Error()}
		}
		return nil, []FieldError{fe}
	}
	return &s, s.Validate()
}

var (
	appName  = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`)
	partName = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,22}[a-z0-9])?$`)
	envKey   = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)
)

// Validate returns every rule s breaks, in field order; nil when it is
// valid. An empty path or dockerfile and replicas 0 count as their defaults.
//
//   - name matches ^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$.
//   - 1 to MaxServices services and at most one resource per type. Service
//     names, and resource names, match ^[a-z]([a-z0-9-]{0,22}[a-z0-9])?$
//     (so <app>-<name> and its suffixes fit a 63-character Kubernetes name)
//     and are unique.
//   - port is 1 to 65535, replicas 1 to MaxReplicas, at most one service is
//     public, and healthCheck, when set, is a path starting with /.
//   - path and dockerfile are relative, use / and have no "..".
//   - env keys match ^[A-Z_][A-Z0-9_]*$ and are not Reserved.
func (s Spec) Validate() []FieldError {
	var errs []FieldError
	add := func(path, format string, args ...any) {
		errs = append(errs, FieldError{Path: path, Message: fmt.Sprintf(format, args...)})
	}

	switch {
	case s.Name == "":
		add("name", "is required")
	case !appName.MatchString(s.Name):
		add("name", "must be 1 to 32 lowercase letters, digits and hyphens, starting and ending with a letter or digit")
	}

	switch {
	case len(s.Services) == 0:
		add("services", "needs at least one service")
	case len(s.Services) > MaxServices:
		add("services", "has %d services; at most %d are allowed", len(s.Services), MaxServices)
	}
	services, public := map[string]int{}, -1
	for i, sv := range s.Services {
		p := fmt.Sprintf("services[%d]", i)
		checkName(add, p+".name", sv.Name, services, "services", i)
		checkRelative(add, p+".path", sv.Path)
		checkRelative(add, p+".dockerfile", sv.Dockerfile)
		switch {
		case sv.Port == 0:
			add(p+".port", "is required")
		case sv.Port < 1 || sv.Port > 65535:
			add(p+".port", "must be between 1 and 65535")
		}
		if sv.HealthCheck != "" && (!strings.HasPrefix(sv.HealthCheck, "/") || strings.ContainsFunc(sv.HealthCheck, isSpaceOrControl)) {
			add(p+".healthCheck", "must be an HTTP path starting with /")
		}
		if sv.Public {
			if public >= 0 {
				add(p+".public", "only one service may be public; services[%d] is", public)
			} else {
				public = i
			}
		}
		if sv.Replicas < 0 || sv.Replicas > MaxReplicas {
			add(p+".replicas", "must be between 1 and %d", MaxReplicas)
		}
		for _, k := range sortedKeys(sv.Env) {
			switch {
			case !envKey.MatchString(k):
				add(p+".env."+k, "must match ^[A-Z_][A-Z0-9_]*$")
			case Reserved(k):
				add(p+".env."+k, "is set by the platform (PORT, DATABASE_URL and S3_* are reserved)")
			}
		}
	}

	resources, types := map[string]int{}, map[string]int{}
	for i, r := range s.Resources {
		p := fmt.Sprintf("resources[%d]", i)
		checkName(add, p+".name", r.Name, resources, "resources", i)
		switch r.Type {
		case TypePostgres, TypeS3:
			if j, ok := types[r.Type]; ok {
				add(p+".type", "only one %s resource is allowed; resources[%d] is one", r.Type, j)
			} else {
				types[r.Type] = i
			}
		case "":
			add(p+".type", "is required")
		default:
			add(p+".type", "must be %s or %s", TypePostgres, TypeS3)
		}
	}
	return errs
}

// checkName checks a service or resource name and that it is unique in seen.
func checkName(add func(string, string, ...any), path, name string, seen map[string]int, list string, i int) {
	switch {
	case name == "":
		add(path, "is required")
	case !partName.MatchString(name):
		add(path, "must be 1 to 24 lowercase letters, digits and hyphens, starting with a letter and ending with a letter or digit")
	default:
		if j, ok := seen[name]; ok {
			add(path, "%q is also the name of %s[%d]", name, list, j)
		} else {
			seen[name] = i
		}
	}
}

// checkRelative checks a path inside the repository. Empty is the default.
func checkRelative(add func(string, string, ...any), field, p string) {
	switch {
	case strings.HasPrefix(p, "/") || strings.Contains(p, `\`):
		add(field, "must be a relative path with / separators")
	case slices.Contains(strings.Split(p, "/"), ".."):
		add(field, "must not contain ..")
	}
}

func isSpaceOrControl(r rune) bool { return r <= ' ' || r == 0x7f }
