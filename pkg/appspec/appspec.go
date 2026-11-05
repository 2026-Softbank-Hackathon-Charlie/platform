// Package appspec reads platform.yaml, the file at the root of an app's
// repository that tells the HopCharlie platform how to build and run the app
// (AppSpec in api/openapi.yaml). The server checks it when an app is
// registered and before every deployment; the CLI checks it locally with the
// same code.
//
// A spec names the app, lists its services (each built from a Dockerfile in
// the repository) and the resources the platform provisions for it. The app
// chart injects PORT into every service with a port, DATABASE_URL and its
// parts (PGHOST and the rest of Reserved) for a postgres resource and the
// S3_* variables for an s3 resource.
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

	"k8s.io/apimachinery/pkg/api/resource"
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

// A service's memory (its container's request and limit, in bytes) and cpu
// (its request, in millicores), ADR 0024. Every namespace has a ResourceQuota
// of requests.cpu 2, requests.memory 4Gi, limits.memory 8Gi and 20 pods. The
// totals bound what the services need during a rolling update, when each
// Deployment runs one pod more than its replicas, and leave the rest of the
// quota to the database pod.
const (
	MinMemory = 256 << 20
	MaxMemory = 1536 << 20
	MinCPU    = 50
	MaxCPU    = 500

	// MaxTotalMemory bounds the sum over services of (replicas + 1) x memory,
	// with an unset memory counted as UnsetMemory, the default limit.
	MaxTotalMemory = 3 << 30
	UnsetMemory    = 256 << 20
	// MaxTotalCPU bounds the sum over services of (replicas + 1) x cpu, with
	// an unset cpu counted as UnsetCPU, the default request.
	MaxTotalCPU = 1500
	UnsetCPU    = 50
)

// Resource types.
const (
	TypePostgres = "postgres"
	TypeS3       = "s3"
)

// The variables the platform sets in every service. An app's env may not set
// them, and no S3_ variable at all (EnvS3Prefix), so a later one cannot clash.
// Other PG variables, like PGSSLMODE, stay the app's.
const (
	// EnvPort is the service's port; a service without one does not get it.
	EnvPort = "PORT"
	// EnvDatabaseURL is key uri of the secret <app>-<resource>-app of the
	// postgres resource's CNPG Cluster <app>-<resource>; the others are its
	// keys host, port, dbname, username, password and jdbc-uri.
	EnvDatabaseURL     = "DATABASE_URL"
	EnvPGHost          = "PGHOST"
	EnvPGPort          = "PGPORT"
	EnvPGDatabase      = "PGDATABASE"
	EnvPGUser          = "PGUSER"
	EnvPGPassword      = "PGPASSWORD"
	EnvJDBCDatabaseURL = "JDBC_DATABASE_URL"
	// The s3 resource's bucket and credentials.
	EnvS3Endpoint  = "S3_ENDPOINT"
	EnvS3Bucket    = "S3_BUCKET"
	EnvS3Region    = "S3_REGION"
	EnvS3PathStyle = "S3_PATH_STYLE"
	EnvS3AccessKey = "S3_ACCESS_KEY"
	EnvS3SecretKey = "S3_SECRET_KEY"

	EnvS3Prefix = "S3_"
)

// ReservedNames lists the Reserved names, for messages.
const ReservedNames = "PORT, DATABASE_URL, PGHOST, PGPORT, PGDATABASE, PGUSER, PGPASSWORD, JDBC_DATABASE_URL and S3_*"

// Reserved reports whether the platform sets the variable name, so that an
// app's env may not.
func Reserved(name string) bool {
	switch name {
	case EnvPort, EnvDatabaseURL, EnvPGHost, EnvPGPort, EnvPGDatabase, EnvPGUser, EnvPGPassword, EnvJDBCDatabaseURL:
		return true
	}
	return strings.HasPrefix(name, EnvS3Prefix)
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
	// Port is where the service listens; it also gets it as PORT. 0 is a
	// worker: no Service, no probes, ready once it runs.
	Port int `json:"port,omitempty" yaml:"port,omitempty"`
	// HealthCheck is an HTTP path that answers 2xx once the service is ready.
	HealthCheck string `json:"healthCheck,omitempty" yaml:"healthCheck,omitempty"`
	// Public serves the service at https://<app>.hopcharlie.dev. At most one service is public.
	Public   bool `json:"public" yaml:"public,omitempty"`
	Replicas int  `json:"replicas" yaml:"replicas,omitempty"`
	// Memory is a Kubernetes quantity, the container's memory request and
	// limit; CPU one for its CPU request (no limit). Empty keeps the
	// environment's defaults.
	Memory string `json:"memory,omitempty" yaml:"memory,omitempty"`
	CPU    string `json:"cpu,omitempty" yaml:"cpu,omitempty"`
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
	appName  = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,30}[a-z0-9]$`)
	partName = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,22}[a-z0-9])?$`)
	envKey   = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)
)

// Validate returns every rule s breaks, in field order; nil when it is
// valid. An empty path or dockerfile and replicas 0 count as their defaults.
//
//   - name matches ^[a-z0-9][a-z0-9-]{1,30}[a-z0-9]$ (3 to 32 characters)
//     and has no "--", which separates the app from the branch in instance
//     names (InstanceName).
//   - 1 to MaxServices services and at most one resource per type. Service
//     names, and resource names, match ^[a-z]([a-z0-9-]{0,22}[a-z0-9])?$
//     (so <app>-<name> and its suffixes fit a 63-character Kubernetes name)
//     and are unique.
//   - port, when set, is 1 to 65535; a public service and a healthCheck
//     need one. replicas is 1 to MaxReplicas, at most one service is public,
//     and healthCheck, when set, is a path starting with /.
//   - memory, when set, is a quantity from MinMemory to MaxMemory and cpu
//     one from MinCPU to MaxCPU. The sums over services of (replicas + 1) x
//     memory and x cpu, counting unset ones as UnsetMemory and UnsetCPU, are
//     at most MaxTotalMemory and MaxTotalCPU.
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
		add("name", "must be 3 to 32 lowercase letters, digits and hyphens, starting and ending with a letter or digit")
	case strings.Contains(s.Name, "--"):
		add("name", "must not contain --, which separates the app from the branch in instance names")
	}

	switch {
	case len(s.Services) == 0:
		add("services", "needs at least one service")
	case len(s.Services) > MaxServices:
		add("services", "has %d services; at most %d are allowed", len(s.Services), MaxServices)
	}
	services, public := map[string]int{}, -1
	var memory, cpu int64 // the totals; -1 once a service's value is wrong
	for i, sv := range s.Services {
		p := fmt.Sprintf("services[%d]", i)
		checkName(add, p+".name", sv.Name, services, "services", i)
		checkRelative(add, p+".path", sv.Path)
		checkRelative(add, p+".dockerfile", sv.Dockerfile)
		if sv.Port < 0 || sv.Port > 65535 {
			add(p+".port", "must be between 1 and 65535")
		}
		if sv.HealthCheck != "" && (!strings.HasPrefix(sv.HealthCheck, "/") || strings.ContainsFunc(sv.HealthCheck, isSpaceOrControl)) {
			add(p+".healthCheck", "must be an HTTP path starting with /")
		}
		if sv.HealthCheck != "" && sv.Port == 0 {
			add(p+".healthCheck", "needs a port: the probes request the path on it")
		}
		if sv.Public {
			if public >= 0 {
				add(p+".public", "only one service may be public; services[%d] is", public)
			} else {
				public = i
			}
			if sv.Port == 0 {
				add(p+".public", "needs a port: the traffic gateway sends requests to it")
			}
		}
		if sv.Replicas < 0 || sv.Replicas > MaxReplicas {
			add(p+".replicas", "must be between 1 and %d", MaxReplicas)
		}
		pods := int64(max(sv.Replicas, 1) + 1) // one more during a rolling update
		m, ok := quantity(add, p+".memory", sv.Memory, "512Mi or 1Gi", MinMemory, MaxMemory, UnsetMemory, (*resource.Quantity).Value, mebibytes)
		memory = addTotal(memory, ok, pods*m)
		c, ok := quantity(add, p+".cpu", sv.CPU, "250m or 0.5", MinCPU, MaxCPU, UnsetCPU, (*resource.Quantity).MilliValue, millicores)
		cpu = addTotal(cpu, ok, pods*c)
		for _, k := range sortedKeys(sv.Env) {
			switch {
			case !envKey.MatchString(k):
				add(p+".env."+k, "must match ^[A-Z_][A-Z0-9_]*$")
			case Reserved(k):
				add(p+".env."+k, "is set by the platform (%s are reserved)", ReservedNames)
			}
		}
	}
	if memory > MaxTotalMemory {
		add("services", "need at most %s of memory during a rolling update: the sum of (replicas + 1) x memory, counting an unset memory as %s, is %s",
			mebibytes(MaxTotalMemory), mebibytes(UnsetMemory), mebibytes(memory))
	}
	if cpu > MaxTotalCPU {
		add("services", "need at most %s of CPU during a rolling update: the sum of (replicas + 1) x cpu, counting an unset cpu as %s, is %s",
			millicores(MaxTotalCPU), millicores(UnsetCPU), millicores(cpu))
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

// quantity checks a service's memory or cpu, a Kubernetes quantity whose
// value (bytes or millicores) is lo to hi, and returns that value, or unset
// when it is empty. ok is false when the quantity is wrong.
func quantity(add func(string, string, ...any), path, s, example string, lo, hi, unset int64, value func(*resource.Quantity) int64, format func(int64) string) (v int64, ok bool) {
	if s == "" {
		return unset, true
	}
	q, err := resource.ParseQuantity(s)
	if err != nil {
		add(path, "must be a Kubernetes quantity such as %s", example)
		return 0, false
	}
	if v = value(&q); v < lo || v > hi {
		add(path, "must be between %s and %s", format(lo), format(hi))
		return 0, false
	}
	return v, true
}

// addTotal adds v to a total that is -1 once a value was wrong.
func addTotal(total int64, ok bool, v int64) int64 {
	if !ok || total < 0 {
		return -1
	}
	return total + v
}

// mebibytes formats bytes as Gi when whole, else as Mi rounded up.
func mebibytes(b int64) string {
	if b%(1<<30) == 0 {
		return fmt.Sprintf("%dGi", b>>30)
	}
	return fmt.Sprintf("%dMi", (b+1<<20-1)>>20)
}

func millicores(m int64) string { return fmt.Sprintf("%dm", m) }

// Canonical returns a memory or cpu quantity the way Kubernetes writes it
// back (1.5Gi is 1536Mi, 0.5 is 500m), so the gitops file matches what the
// cluster stores.
func Canonical(quantity string) (string, error) {
	q, err := resource.ParseQuantity(quantity)
	if err != nil {
		return "", err
	}
	return q.String(), nil
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
