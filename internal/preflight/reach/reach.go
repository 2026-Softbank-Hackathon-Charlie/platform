// Package reach checks, before a plan is approved, that the platform API can
// reach the plan's source database the way the engine will: the name in the
// DSN resolves, a TCP connection to its port opens, and the database accepts
// the DSN's login.
//
// The engine's own precheck fails on the same problem, but only after the
// operator approved and started the run (a failed run, 60 ms in). A plan is
// written where its databases resolve (the AI runner's sandbox, a laptop on
// the tailnet) and executed where the API runs, so a name that resolves in one
// place can be unknown in the other; the rehearsal never sees that difference
// (ADR 0050).
//
// Only the source decides. The target database may not exist yet (the engine
// creates it from the manifest), so its result is reported and never blocks.
// Whatever the check cannot establish (a DNS timeout, a DSN without a host, a
// login that failed for a reason other than a refused password) is Unknown,
// never a pass and never a refusal.
package reach

import (
	"context"
	"errors"
	"net"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"
)

// Status of a check.
type Status string

const (
	Pass    Status = "pass"
	Fail    Status = "fail"
	Unknown Status = "unknown" // not established; never treated as a pass
)

// Codes of a Result.
const (
	CodeReachable    = "reachable"    // the name resolves and a connection opens
	CodeDNS          = "dns"          // the name does not resolve from here
	CodeTCP          = "tcp"          // the name resolves, but no connection to its port opens
	CodeAuth         = "auth"         // the port accepts connections, but the database refuses the DSN's login
	CodeUnparsed     = "unparsed"     // no host and port could be read from the DSN
	CodeInconclusive = "inconclusive" // a timeout or a resolver error: neither reachable nor missing
)

// Side is one endpoint's outcome.
type Side struct {
	Status Status `json:"status"`
	Code   string `json:"code"`
}

// Result is the check's outcome. Source decides; Target is for the record.
// Message and Result hold statuses and codes only, never a host name, a DSN
// or a server message.
type Result struct {
	Status  Status `json:"status"`
	Code    string `json:"code"`
	Message string `json:"message"`
	Source  Side   `json:"source"`
	Target  Side   `json:"target"`
}

// Resolver looks up a host name; *net.Resolver is one.
type Resolver interface {
	LookupHost(ctx context.Context, host string) ([]string, error)
}

// DialFunc opens a TCP connection; (*net.Dialer).DialContext is one.
type DialFunc func(ctx context.Context, network, address string) (net.Conn, error)

// LoginFunc opens a database session with the DSN and closes it again.
type LoginFunc func(ctx context.Context, dsn string) error

// DefaultTimeout bounds the check of one side (name lookup and connection).
const DefaultTimeout = 4 * time.Second

// maxAddrs is how many of a name's addresses are tried.
const maxAddrs = 4

// Checker resolves and dials the DSN hosts of a plan.
type Checker struct {
	Timeout  time.Duration // per side; 0 means DefaultTimeout
	Resolver Resolver      // nil means net.DefaultResolver
	Dial     DialFunc      // nil means a net.Dialer
	Login    LoginFunc     // nil means a pgx connection
}

// Check looks at the source and the target. The error is only the context's.
func (c Checker) Check(ctx context.Context, p *plan.Plan) (Result, error) {
	src := c.side(ctx, p.Source.DSN, true)
	dst := c.side(ctx, p.Target.DSN, false)
	r := Result{Status: src.Status, Code: src.Code, Source: src, Target: dst}
	switch {
	case src.Status == Pass:
		r.Message = "the source database name resolves and its port accepts connections from the platform API"
	case src.Code == CodeDNS:
		r.Message = "the source database name does not resolve from the platform API (dns): the run would stop at its precheck. " +
			"Make the DB route exist where the API runs, then approve again; if dbHost in the apps catalog changes, submit a new plan, which keeps the dbHost it was made with"
	case src.Code == CodeTCP:
		r.Message = "the source database name resolves, but its port does not accept connections from the platform API (tcp): the run would stop at its precheck. " +
			"Check the network path (proxy, NetworkPolicy, tailnet policy) between the API and the database"
	case src.Code == CodeAuth:
		r.Message = "the source database accepts connections from the platform API but refuses the login of the plan's database user (auth): the run would stop at its precheck. " +
			"Check that the password variable the apps catalog names for this database (passwordEnv) holds that user's current password, and that the route leads to the intended database"
	default:
		r.Message = "whether the platform API can reach the source database could not be established (" + src.Code + "); the plan is not refused for it"
	}
	return r, ctx.Err()
}

// side checks one DSN's host and port, and with login also that the database
// accepts the DSN's user.
func (c Checker) side(ctx context.Context, dsn string, login bool) Side {
	host, port := hostPort(dsn)
	if host == "" {
		return Side{Unknown, CodeUnparsed}
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	sctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	addrs := []string{host}
	if net.ParseIP(host) == nil {
		var res Resolver = net.DefaultResolver
		if c.Resolver != nil {
			res = c.Resolver
		}
		found, err := res.LookupHost(sctx, host)
		if err != nil {
			var dnsErr *net.DNSError
			if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
				return Side{Fail, CodeDNS}
			}
			return Side{Unknown, CodeInconclusive}
		}
		if len(found) == 0 {
			return Side{Fail, CodeDNS}
		}
		addrs = found
	}
	if len(addrs) > maxAddrs {
		addrs = addrs[:maxAddrs]
	}

	dial := c.Dial
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}
	open := false
	for _, a := range addrs {
		conn, err := dial(sctx, "tcp", net.JoinHostPort(a, strconv.Itoa(int(port))))
		if err == nil {
			_ = conn.Close()
			open = true
			break
		}
	}
	if !open {
		if ctx.Err() != nil {
			return Side{Unknown, CodeInconclusive} // the caller gave up: nothing was established
		}
		return Side{Fail, CodeTCP}
	}
	if !login {
		return Side{Pass, CodeReachable}
	}
	return c.loginSide(sctx, dsn)
}

// loginSide opens a session with the DSN. Only a refused login blocks: a wrong
// password (28P01) or a rejected user (28000, which includes a pg_hba.conf
// reject). Anything else (a timeout, TLS, too many connections, a database
// that is starting) says nothing about the credentials.
func (c Checker) loginSide(ctx context.Context, dsn string) Side {
	login := c.Login
	if login == nil {
		login = pgLogin
	}
	err := login(ctx, dsn)
	if err == nil {
		return Side{Pass, CodeReachable}
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && (pgErr.Code == "28P01" || pgErr.Code == "28000") {
		return Side{Fail, CodeAuth}
	}
	return Side{Unknown, CodeInconclusive}
}

// pgLogin opens and closes a session. Its errors are classified, never shown.
func pgLogin(ctx context.Context, dsn string) error {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return err
	}
	if cfg.ConnectTimeout == 0 || cfg.ConnectTimeout > 3*time.Second {
		cfg.ConnectTimeout = 3 * time.Second
	}
	if cfg.RuntimeParams == nil {
		cfg.RuntimeParams = map[string]string{}
	}
	cfg.RuntimeParams["application_name"] = "hopcharlie-reach-preflight"
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return err
	}
	return conn.Close(ctx)
}

// hostPort reads the host and port of a DSN, or "" when it has none to check
// (an empty DSN, one that does not parse, a unix socket).
func hostPort(dsn string) (string, uint16) {
	if dsn == "" {
		return "", 0
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil || cfg.Host == "" || cfg.Host[0] == '/' {
		return "", 0
	}
	return cfg.Host, cfg.Port
}
