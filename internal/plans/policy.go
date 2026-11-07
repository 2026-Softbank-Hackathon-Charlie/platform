package plans

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"net"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"
)

// Policy is the server-side trust boundary for submitted plans.
//
// The engine was written for an operator running it locally: plan.Parse expands
// the process environment into DSNs and tokens, and the engine reads files and
// runs binaries named in the plan. Behind an API those become remote inputs, so
// every field that reaches the server's environment, filesystem, processes or
// network is constrained here before a plan is accepted.
//
// Environment references are checked field by field after JSON decoding and
// before anything is expanded (checkRefs). A DSN may reference one variable,
// and only as its whole password; gateway.token may only be ${GatewayTokenEnv};
// nothing else may contain "$". Whatever the server reports about a plan
// (summaries, errors) is built from the plan as submitted, never from expanded
// values.
type Policy struct {
	// PasswordEnvs may be the password of either DSN, sent to any database
	// host the other rules allow. Rollback swaps the DSNs, so a variable
	// allowed in one DSN is allowed in both.
	PasswordEnvs []string
	// HostPasswordEnvs maps a password variable to the only database hosts
	// ("host:port") it may be sent to: an app's passwordEnv is the password of
	// that app's database in one environment (apps.Catalog.PasswordHosts).
	HostPasswordEnvs map[string][]string
	// GatewayTokenEnv is the variable gateway.token may reference, as
	// "${GatewayTokenEnv}" and nothing else. Empty allows no token.
	GatewayTokenEnv string
	// GatewayURL is the only gateway a plan may target in "http" mode: its
	// admin root (the gateway's default app) or one app's scope under it,
	// GatewayAppURL (ADR 0012). Empty means http mode is rejected.
	GatewayURL string
	// AppDBHosts maps each registered app to its database hosts ("host:port")
	// in every environment (apps.Catalog.AppHosts). A plan whose databases
	// belong to an app must use that app's gateway scope, and an app's scope
	// only fits a plan whose source and target databases are both the app's.
	AppDBHosts map[string][]string
	// DSNHostSuffixes, when set, restricts database hosts (e.g. ".ts.net").
	DSNHostSuffixes []string
	// RcloneFlags lists allowed rclone flag names (without values).
	RcloneFlags []string
	// Buckets are the only values files.source and files.target may take:
	// "remote:bucket" or "remote:bucket/path", exactly as registered in the
	// apps file or PLATFORM_PLAN_BUCKETS. rclone reads a whole backend
	// definition from the remote part of a path (":local:/data",
	// ":s3,access_key_id=...:bucket", "gcs,service_account_file=...:b"), so
	// any other value could copy the server's own files or send them to
	// storage the team does not own. Empty rejects every plan with files.
	Buckets []string
}

// DefaultPolicy matches the variable names used in the engine's example plan.
func DefaultPolicy() Policy {
	return Policy{
		PasswordEnvs:    []string{"SRC_PGPASSWORD", "DST_PGPASSWORD"},
		GatewayTokenEnv: "GATEWAY_TOKEN",
		RcloneFlags:     []string{"--checksum", "--size-only", "--fast-list", "--transfers", "--checkers"},
	}
}

// withRegistered returns p with the registered apps' password variables and
// database hosts (AppDatabases.RegistryHosts) added to HostPasswordEnvs and
// AppDBHosts, and their buckets (AppDatabases.RegistryBuckets) to Buckets.
// A variable or app p already has keeps p's entry; a bucket that is not a
// plain remote:bucket is left out.
func (p Policy) withRegistered(passwordHosts, appHosts map[string][]string, buckets []string) Policy {
	merge := func(own, more map[string][]string) map[string][]string {
		out := maps.Clone(own)
		if out == nil {
			out = map[string][]string{}
		}
		for k, v := range more {
			if _, ok := out[k]; !ok {
				out[k] = v
			}
		}
		return out
	}
	p.HostPasswordEnvs = merge(p.HostPasswordEnvs, passwordHosts)
	p.AppDBHosts = merge(p.AppDBHosts, appHosts)
	p.Buckets = slices.Clone(p.Buckets)
	for _, b := range buckets {
		if CheckBucket(b) == nil && !slices.Contains(p.Buckets, b) {
			p.Buckets = append(p.Buckets, b)
		}
	}
	return p
}

// passwordEnvName is the form every password variable must have.
var passwordEnvName = regexp.MustCompile(`^[A-Z][A-Z0-9_]*_PGPASSWORD$`)

// CheckPasswordEnv refuses a password variable name that could name a server
// variable used for something else. A plan can send a password variable's
// value to a database host, so the name must end in _PGPASSWORD (upper case)
// and must not start with PLATFORM_ (the server's own settings, such as its
// database URL) or AWS_ (cloud credentials). GATEWAY_TOKEN, PGPASSWORD and
// the like never qualify.
func CheckPasswordEnv(name string) error {
	switch {
	case !passwordEnvName.MatchString(name):
		return fmt.Errorf("password variable %q must be upper case and end in _PGPASSWORD", name)
	case strings.HasPrefix(name, "PLATFORM_"), strings.HasPrefix(name, "AWS_"):
		return fmt.Errorf("password variable %q must not start with PLATFORM_ or AWS_", name)
	}
	return nil
}

// bucketName is a registered bucket: a configured rclone remote, then a
// bucket and an optional path. No leading ":" (an inline backend), no ","
// (inline options), no spaces or quotes.
var bucketName = regexp.MustCompile(`^[A-Za-z0-9_-]+:[A-Za-z0-9][A-Za-z0-9._-]*(/[A-Za-z0-9._-]+)*$`)

// CheckBucket refuses a bucket that is not a plain "remote:bucket[/path]".
func CheckBucket(b string) error {
	if !bucketName.MatchString(b) {
		return fmt.Errorf("bucket %q must be remote:bucket or remote:bucket/path (letters, digits, '.', '_', '-')", b)
	}
	_, path, _ := strings.Cut(b, ":")
	for _, seg := range strings.Split(path, "/") {
		if seg == "." || seg == ".." {
			return fmt.Errorf("bucket %q must not contain . or .. path segments", b)
		}
	}
	return nil
}

// RcloneTypeEnv is the variable rclone reads the backend type of a bucket's
// remote from (RCLONE_CONFIG_<REMOTE>_TYPE). The server's environment, not
// the plan, defines remotes and their credentials.
func RcloneTypeEnv(bucket string) string {
	remote, _, _ := strings.Cut(bucket, ":")
	return "RCLONE_CONFIG_" + strings.ToUpper(strings.ReplaceAll(remote, "-", "_")) + "_TYPE"
}

// planID becomes part of file names and URLs (engine state file, run id).
var planID = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// expandedFields are the fields plan.Parse runs os.ExpandEnv on. They are
// decoded with encoding/json exactly as plan.Parse decodes them, so JSON
// escapes (a "$" written as a unicode escape) are resolved before they are
// checked; a check of the raw bytes would miss them.
type expandedFields struct {
	Source struct {
		DSN string `json:"dsn"`
	} `json:"source"`
	Target struct {
		DSN string `json:"dsn"`
	} `json:"target"`
	Gateway struct {
		URL   string `json:"url"`
		Token string `json:"token"`
	} `json:"gateway"`
}

// dbHosts are the "host:port" the DSNs connect to, read from the plan as
// submitted. Empty when a DSN is missing.
// all holds every host of both DSNs, fallback hosts included.
// refs are the password variables the DSNs reference.
type dbHosts struct {
	source, target string
	all            []string
	refs           []string
}

// checkRefs runs before plan.Parse, which expands the environment.
func (p Policy) checkRefs(raw []byte) (dbHosts, error) {
	var f expandedFields
	if err := json.NewDecoder(bytes.NewReader(raw)).Decode(&f); err != nil {
		return dbHosts{}, fmt.Errorf("decode plan: %w", err)
	}
	var h dbHosts
	for _, d := range []struct {
		role, dsn string
		first     *string
	}{{"source", f.Source.DSN, &h.source}, {"target", f.Target.DSN, &h.target}} {
		hosts, ref, err := p.checkDSN(d.role, d.dsn)
		if err != nil {
			return dbHosts{}, err
		}
		if len(hosts) > 0 {
			*d.first = hosts[0]
		}
		h.all = append(h.all, hosts...)
		if ref != "" && !slices.Contains(h.refs, ref) {
			h.refs = append(h.refs, ref)
		}
	}
	if strings.Contains(f.Gateway.URL, "$") {
		return dbHosts{}, fmt.Errorf("gateway.url must not reference environment variables")
	}
	if t := f.Gateway.Token; t != "" && (p.GatewayTokenEnv == "" || t != "${"+p.GatewayTokenEnv+"}") {
		return dbHosts{}, fmt.Errorf("gateway.token must be empty or exactly ${%s}", p.GatewayTokenEnv)
	}
	return h, nil
}

// dsnRef is a DSN whose only reference is its whole password:
// postgres://user:${VAR}@... with no other "$".
var dsnRef = regexp.MustCompile(`^(postgres(?:ql)?://[^:@/?#$]*):\$\{([^}]*)\}(@[^$]*)$`)

// dsnPlaceholder stands in for the password while the template is parsed.
const dsnPlaceholder = "password"

// checkDSN checks the DSN as submitted and returns its hosts ("host:port"),
// the first one and then the fallbacks, and the variable it references, if
// any.
func (p Policy) checkDSN(role, dsn string) ([]string, string, error) {
	if dsn == "" {
		return nil, "", nil // plan.Parse reports the missing DSN
	}
	if !strings.HasPrefix(dsn, "postgres://") && !strings.HasPrefix(dsn, "postgresql://") {
		return nil, "", fmt.Errorf("%s.dsn must be a postgres:// URL", role)
	}
	onlyPassword := fmt.Errorf("%s.dsn may reference an environment variable only as its whole password, as in postgres://user:${VAR}@host:5432/db", role)
	tmpl, ref := dsn, ""
	if strings.Contains(dsn, "$") {
		m := dsnRef.FindStringSubmatch(dsn)
		if m == nil {
			return nil, "", onlyPassword
		}
		tmpl, ref = m[1]+":"+dsnPlaceholder+m[3], m[2]
	}
	// Errors never echo the DSN: it may hold a literal password.
	u, err := url.Parse(tmpl)
	if err != nil || u.Host == "" {
		return nil, "", fmt.Errorf("%s.dsn is not a postgres:// URL with a host", role)
	}
	if pw, _ := u.User.Password(); ref != "" && pw != dsnPlaceholder {
		return nil, "", onlyPassword
	}
	cfg, err := pgconn.ParseConfig(tmpl)
	if err != nil {
		return nil, "", fmt.Errorf("%s.dsn is not a valid PostgreSQL connection string", role)
	}
	hosts := []string{net.JoinHostPort(cfg.Host, strconv.Itoa(int(cfg.Port)))}
	for _, fb := range cfg.Fallbacks {
		hosts = append(hosts, net.JoinHostPort(fb.Host, strconv.Itoa(int(fb.Port))))
	}
	if len(p.DSNHostSuffixes) > 0 {
		for _, hp := range hosts {
			h, _, _ := net.SplitHostPort(hp)
			if !slices.ContainsFunc(p.DSNHostSuffixes, func(s string) bool { return strings.HasSuffix(h, s) }) {
				return nil, "", fmt.Errorf("%s.dsn host %q is not an allowed database host", role, h)
			}
		}
	}
	if ref != "" {
		if err := p.checkPasswordRef(role, ref, hosts); err != nil {
			return nil, "", err
		}
	}
	return hosts, ref, nil
}

// checkPasswordRef allows ref as the password of a DSN connecting to hosts.
func (p Policy) checkPasswordRef(role, ref string, hosts []string) error {
	if slices.Contains(p.PasswordEnvs, ref) {
		return nil
	}
	allowed, ok := p.HostPasswordEnvs[ref]
	if !ok {
		return fmt.Errorf("%s.dsn references environment variable %q, which is not an allowed password variable", role, ref)
	}
	for _, h := range hosts {
		if !slices.ContainsFunc(allowed, func(a string) bool { return strings.EqualFold(a, h) }) {
			return fmt.Errorf("%s.dsn sends ${%s} to %s, but it is the password of %s only", role, ref, h, strings.Join(allowed, ", "))
		}
	}
	return nil
}

// checkParsed validates fields that reach the filesystem, processes or network.
func (p Policy) checkParsed(pl *plan.Plan, hosts dbHosts) error {
	if pl.Decommission != nil {
		return fmt.Errorf("decommission is not accepted by the platform; source retirement uses the server-owned retention policy")
	}
	if !planID.MatchString(pl.ID) {
		return fmt.Errorf("id %q must be lowercase letters, digits and '-' (at most 63)", pl.ID)
	}
	for role, ep := range map[string]plan.Endpoint{"source": pl.Source, "target": pl.Target} {
		if ep.Kubeconfig != "" {
			return fmt.Errorf("%s.kubeconfig is not allowed; the server uses its own kubeconfig and selects clusters by context", role)
		}
	}
	if err := checkManifest(pl.Target.Manifest); err != nil {
		return err
	}
	if err := checkWrittenRecords(pl.Verify.WrittenRecords); err != nil {
		return err
	}
	if pl.Files != nil {
		if pl.Files.Rclone != "rclone" {
			return fmt.Errorf("files.rclone is not allowed; the server runs its own rclone")
		}
		for _, f := range pl.Files.Flags {
			if !strings.HasPrefix(f, "-") {
				continue // a value of the previous flag
			}
			name, _, _ := strings.Cut(f, "=")
			if !slices.Contains(p.RcloneFlags, name) {
				return fmt.Errorf("rclone flag %q is not allowed", name)
			}
		}
		if len(p.Buckets) == 0 {
			return fmt.Errorf("files is disabled on this server: no bucket is registered (PLATFORM_APPS_FILE, PLATFORM_PLAN_BUCKETS or PLATFORM_APP_FILES)")
		}
		for _, f := range []struct{ field, value string }{{"files.source", pl.Files.Source}, {"files.target", pl.Files.Target}} {
			if !slices.Contains(p.Buckets, f.value) {
				return fmt.Errorf("%s %q is not a registered bucket", f.field, f.value)
			}
		}
		if pl.Files.Source == pl.Files.Target {
			return fmt.Errorf("files.source and files.target must be different buckets")
		}
	}
	if pl.Gateway.Mode == "http" {
		if p.GatewayURL == "" {
			return fmt.Errorf("gateway.mode http is disabled on this server")
		}
		if err := p.checkGatewayScope(pl.Gateway.URL, hosts); err != nil {
			return err
		}
	}
	if pl.Target.App != nil {
		// Cold mode (ADR 0012 of the engine) scales a workload and sends smoke
		// requests to a URL from the plan; the policy has no rules for those yet.
		return fmt.Errorf("target.app is not accepted by this server yet")
	}
	return nil
}

// GatewayAppURL is the admin URL of one app's scope in the gateway at root
// (ADR 0012): the engine and the runner use it as the gateway's base URL and
// then act on that app alone.
func GatewayAppURL(root, app string) string {
	return strings.TrimRight(root, "/") + "/apps/" + app
}

// checkGatewayScope decides the plan's app from every host of both DSNs,
// fallback hosts included. No host of a registered app: only the gateway
// admin root (the default app). Hosts of one app: every host must be that
// app's and gateway.url exactly GatewayAppURL of it, never the root. Hosts
// of several apps: refused. Anything else would hold or route the wrong
// app's traffic.
func (p Policy) checkGatewayScope(raw string, hosts dbHosts) error {
	u := strings.TrimRight(raw, "/")
	var owners []string
	for app, appHosts := range p.AppDBHosts {
		if slices.ContainsFunc(hosts.all, func(h string) bool { return hasHost(appHosts, h) }) {
			owners = append(owners, app)
		}
	}
	slices.Sort(owners)
	switch len(owners) {
	case 0:
		if u != strings.TrimRight(p.GatewayURL, "/") {
			return fmt.Errorf("gateway.url must be the platform gateway: the plan's databases belong to no registered app")
		}
		return nil
	case 1:
	default:
		return fmt.Errorf("the plan's databases belong to several apps (%s); a plan moves one app", strings.Join(owners, ", "))
	}
	app := owners[0]
	for _, h := range hosts.all {
		if !hasHost(p.AppDBHosts[app], h) {
			return fmt.Errorf("every database host of a plan for app %s must be the app's; %s is not", app, h)
		}
	}
	if u != GatewayAppURL(p.GatewayURL, app) {
		return fmt.Errorf("gateway.url must be the platform gateway's scope of app %s (%s), not the gateway root or another scope", app, GatewayAppURL(p.GatewayURL, app))
	}
	return nil
}

func hasHost(hosts []string, h string) bool {
	return h != "" && slices.ContainsFunc(hosts, func(a string) bool { return strings.EqualFold(a, h) })
}

func safeRelPath(field, p string) error {
	if p == "" {
		return nil
	}
	if filepath.IsAbs(p) || strings.HasPrefix(p, "/") || strings.HasPrefix(p, `\`) || filepath.VolumeName(p) != "" {
		return fmt.Errorf("%s must be a relative path", field)
	}
	if slices.Contains(strings.Split(strings.ReplaceAll(p, `\`, "/"), "/"), "..") {
		return fmt.Errorf("%s must not leave the server's working directory", field)
	}
	return nil
}
