package plans

import (
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
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
type Policy struct {
	// EnvAllow lists environment variables a plan may reference as ${VAR}.
	EnvAllow []string
	// GatewayURL is the only gateway a plan may target in "http" mode.
	// Empty means http mode is rejected.
	GatewayURL string
	// DSNHostSuffixes, when set, restricts database hosts (e.g. ".ts.net").
	DSNHostSuffixes []string
	// RcloneFlags lists allowed rclone flag names (without values).
	RcloneFlags []string
}

// DefaultPolicy matches the variable names used in the engine's example plan.
func DefaultPolicy() Policy {
	return Policy{
		EnvAllow:    []string{"SRC_PGPASSWORD", "DST_PGPASSWORD", "GATEWAY_TOKEN"},
		RcloneFlags: []string{"--checksum", "--size-only", "--fast-list", "--transfers", "--checkers"},
	}
}

var envRef = regexp.MustCompile(`\$\{([^}]*)\}|\$([A-Za-z_][A-Za-z0-9_]*)`)

// checkRaw runs before parsing: env expansion happens inside plan.Parse.
func (p Policy) checkRaw(raw []byte) error {
	for _, m := range envRef.FindAllStringSubmatch(string(raw), -1) {
		name := m[1] + m[2]
		if !slices.Contains(p.EnvAllow, name) {
			return fmt.Errorf("plan references environment variable %q, which is not allowed", name)
		}
	}
	return nil
}

// checkParsed validates fields that reach the filesystem, processes or network.
func (p Policy) checkParsed(pl *plan.Plan) error {
	for role, ep := range map[string]plan.Endpoint{"source": pl.Source, "target": pl.Target} {
		if ep.Kubeconfig != "" {
			return fmt.Errorf("%s.kubeconfig is not allowed; the server uses its own kubeconfig and selects clusters by context", role)
		}
		if err := p.checkDSN(role, ep.DSN); err != nil {
			return err
		}
	}
	if err := safeRelPath("target.manifest", pl.Target.Manifest); err != nil {
		return err
	}
	if err := safeRelPath("verify.writtenRecords", pl.Verify.WrittenRecords); err != nil {
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
	}
	if pl.Gateway.Mode == "http" {
		if p.GatewayURL == "" {
			return fmt.Errorf("gateway.mode http is disabled on this server")
		}
		if strings.TrimRight(pl.Gateway.URL, "/") != strings.TrimRight(p.GatewayURL, "/") {
			return fmt.Errorf("gateway.url must be the platform gateway")
		}
	}
	return nil
}

func (p Policy) checkDSN(role, dsn string) error {
	if len(p.DSNHostSuffixes) == 0 {
		return nil
	}
	cfg, err := pgconn.ParseConfig(dsn)
	if err != nil {
		// Never echo the DSN: it may contain an expanded password.
		return fmt.Errorf("%s.dsn is not a valid PostgreSQL connection string", role)
	}
	hosts := []string{cfg.Host}
	for _, fb := range cfg.Fallbacks {
		hosts = append(hosts, fb.Host)
	}
	for _, h := range hosts {
		ok := slices.ContainsFunc(p.DSNHostSuffixes, func(s string) bool { return strings.HasSuffix(h, s) })
		if !ok {
			return fmt.Errorf("%s.dsn host %q is not an allowed database host", role, h)
		}
	}
	return nil
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
