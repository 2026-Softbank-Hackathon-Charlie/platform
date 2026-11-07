package reach

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"
)

// fakeResolver answers per host; a host without an entry does not exist.
type fakeResolver struct {
	hosts map[string][]string
	err   error // returned for every lookup when set
	seen  []string
}

func (f *fakeResolver) LookupHost(_ context.Context, host string) ([]string, error) {
	f.seen = append(f.seen, host)
	if f.err != nil {
		return nil, f.err
	}
	if a, ok := f.hosts[host]; ok {
		return a, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
}

// fakeDial accepts the addresses in open and refuses the rest.
type fakeDial struct {
	open map[string]bool
	seen []string
}

func (f *fakeDial) dial(_ context.Context, _, address string) (net.Conn, error) {
	f.seen = append(f.seen, address)
	if f.open[address] {
		c, _ := net.Pipe()
		return c, nil
	}
	return nil, errors.New("dial " + address + ": connection refused")
}

// fakeLogin answers every login with err and records the DSNs it was given.
type fakeLogin struct {
	err  error
	seen []string
}

func (f *fakeLogin) login(_ context.Context, dsn string) error {
	f.seen = append(f.seen, dsn)
	return f.err
}

func pgError(code string) error {
	// the engine's own message wraps the server's error like this
	return fmt.Errorf("failed to connect to `user=app database=app`: failed SASL auth: %w",
		&pgconn.PgError{Severity: "FATAL", Code: code, Message: "password authentication failed for user \"app\""})
}

func pl(src, dst string) *plan.Plan {
	return &plan.Plan{Source: plan.Endpoint{DSN: src}, Target: plan.Endpoint{DSN: dst}}
}

const (
	srcDSN = "postgres://app:src-secret@db-gke.example.svc.cluster.local/app"
	dstDSN = "postgres://app:dst-secret@db-eks.example.svc.cluster.local:6432/app"
)

// both names resolve and both ports are open
func bothUp() (*fakeResolver, *fakeDial) {
	return &fakeResolver{hosts: map[string][]string{"db-gke.example.svc.cluster.local": {"10.0.0.5"}, "db-eks.example.svc.cluster.local": {"10.0.0.6"}}},
		&fakeDial{open: map[string]bool{"10.0.0.5:5432": true, "10.0.0.6:6432": true}}
}

func TestSourceReachable(t *testing.T) {
	res, d := bothUp()
	l := &fakeLogin{}
	r, err := Checker{Resolver: res, Dial: d.dial, Login: l.login}.Check(context.Background(), pl(srcDSN, dstDSN))
	if err != nil || r.Status != Pass || r.Code != CodeReachable || r.Source.Status != Pass || r.Target.Status != Pass {
		t.Fatalf("%+v %v", r, err)
	}
	// the port of each DSN is the one dialled (default 5432, explicit 6432)
	if len(d.seen) != 2 || d.seen[0] != "10.0.0.5:5432" || d.seen[1] != "10.0.0.6:6432" {
		t.Fatalf("dialled %v", d.seen)
	}
	// only the source logs in
	if len(l.seen) != 1 || l.seen[0] != srcDSN {
		t.Fatalf("logins %v", l.seen)
	}
}

// The case that made this check: the name is unknown where the API runs.
func TestSourceNameDoesNotResolve(t *testing.T) {
	res := &fakeResolver{} // nothing resolves
	d := &fakeDial{}
	l := &fakeLogin{}
	r, err := Checker{Resolver: res, Dial: d.dial, Login: l.login}.Check(context.Background(), pl(srcDSN, dstDSN))
	if err != nil || r.Status != Fail || r.Code != CodeDNS || r.Source.Code != CodeDNS {
		t.Fatalf("%+v %v", r, err)
	}
	if len(d.seen) != 0 || len(l.seen) != 0 {
		t.Fatalf("dialled %v and logged in %v although nothing resolved", d.seen, l.seen)
	}
	if !strings.Contains(r.Message, "does not resolve") || !strings.Contains(r.Message, "approve again") || !strings.Contains(r.Message, "submit a new plan") {
		t.Fatalf("message %q", r.Message)
	}
}

func TestSourceResolvesButPortIsClosed(t *testing.T) {
	res := &fakeResolver{hosts: map[string][]string{"db-gke.example.svc.cluster.local": {"10.0.0.5", "10.0.0.7"}}}
	d := &fakeDial{} // every dial is refused
	l := &fakeLogin{}
	r, _ := Checker{Resolver: res, Dial: d.dial, Login: l.login}.Check(context.Background(), pl(srcDSN, ""))
	if r.Status != Fail || r.Code != CodeTCP {
		t.Fatalf("%+v", r)
	}
	if len(d.seen) != 2 || len(l.seen) != 0 {
		t.Fatalf("each address is tried once and no login follows: dialled %v, logins %v", d.seen, l.seen)
	}
}

func TestAnyOpenAddressIsEnough(t *testing.T) {
	res := &fakeResolver{hosts: map[string][]string{"db-gke.example.svc.cluster.local": {"10.0.0.5", "10.0.0.7"}}}
	d := &fakeDial{open: map[string]bool{"10.0.0.7:5432": true}}
	r, _ := Checker{Resolver: res, Dial: d.dial, Login: (&fakeLogin{}).login}.Check(context.Background(), pl(srcDSN, ""))
	if r.Status != Pass {
		t.Fatalf("%+v", r)
	}
}

// The case that followed it: the route works and the database says no.
func TestSourceRefusesTheLogin(t *testing.T) {
	for name, code := range map[string]string{
		"wrong password":       "28P01",
		"rejected user or hba": "28000",
	} {
		t.Run(name, func(t *testing.T) {
			res, d := bothUp()
			l := &fakeLogin{err: pgError(code)}
			r, err := Checker{Resolver: res, Dial: d.dial, Login: l.login}.Check(context.Background(), pl(srcDSN, dstDSN))
			if err != nil || r.Status != Fail || r.Code != CodeAuth || r.Source.Code != CodeAuth {
				t.Fatalf("%+v %v", r, err)
			}
			// the target is never logged in to, so its result is the connection's
			if r.Target.Status != Pass || len(l.seen) != 1 {
				t.Fatalf("target %+v, logins %v", r.Target, l.seen)
			}
			if !strings.Contains(r.Message, "refuses the login") || !strings.Contains(r.Message, "passwordEnv") {
				t.Fatalf("message %q", r.Message)
			}
		})
	}
}

// Anything else that goes wrong while logging in says nothing about the credentials.
func TestOtherLoginFailuresAreUnknown(t *testing.T) {
	for name, err := range map[string]error{
		"too many connections": pgError("53300"),
		"database starting":    pgError("57P03"),
		"timeout":              context.DeadlineExceeded,
		"tls":                  errors.New("tls: failed to verify certificate"),
	} {
		t.Run(name, func(t *testing.T) {
			res, d := bothUp()
			r, _ := Checker{Resolver: res, Dial: d.dial, Login: (&fakeLogin{err: err}).login}.Check(context.Background(), pl(srcDSN, dstDSN))
			if r.Status != Unknown || r.Code != CodeInconclusive {
				t.Fatalf("%+v", r)
			}
			if !strings.Contains(r.Message, "not refused") {
				t.Fatalf("message %q", r.Message)
			}
		})
	}
}

// A resolver that failed for another reason says nothing about the name.
func TestResolverFailureIsUnknownNotMissing(t *testing.T) {
	for name, err := range map[string]error{
		"timeout":   &net.DNSError{Err: "i/o timeout", Name: "x", IsTimeout: true},
		"temporary": &net.DNSError{Err: "server misbehaving", Name: "x", IsTemporary: true},
		"other":     errors.New("resolver exploded"),
	} {
		t.Run(name, func(t *testing.T) {
			d := &fakeDial{}
			r, _ := Checker{Resolver: &fakeResolver{err: err}, Dial: d.dial, Login: (&fakeLogin{}).login}.Check(context.Background(), pl(srcDSN, dstDSN))
			if r.Status != Unknown || r.Code != CodeInconclusive || len(d.seen) != 0 {
				t.Fatalf("%+v dialled %v", r, d.seen)
			}
			if !strings.Contains(r.Message, "not refused") {
				t.Fatalf("message %q", r.Message)
			}
		})
	}
}

// The target may not exist yet: it is recorded and never decides.
func TestTargetNeverDecides(t *testing.T) {
	res := &fakeResolver{hosts: map[string][]string{"db-gke.example.svc.cluster.local": {"10.0.0.5"}}} // the target name is unknown
	d := &fakeDial{open: map[string]bool{"10.0.0.5:5432": true}}
	r, _ := Checker{Resolver: res, Dial: d.dial, Login: (&fakeLogin{}).login}.Check(context.Background(), pl(srcDSN, dstDSN))
	if r.Status != Pass || r.Target.Status != Fail || r.Target.Code != CodeDNS {
		t.Fatalf("%+v", r)
	}
}

func TestIPHostSkipsTheLookup(t *testing.T) {
	res := &fakeResolver{}
	d := &fakeDial{open: map[string]bool{"10.1.2.3:5432": true}}
	r, _ := Checker{Resolver: res, Dial: d.dial, Login: (&fakeLogin{}).login}.Check(context.Background(), pl("postgres://app:pw@10.1.2.3/app", ""))
	if r.Status != Pass || len(res.seen) != 0 {
		t.Fatalf("%+v looked up %v", r, res.seen)
	}
}

func TestDSNWithoutAHostIsUnknown(t *testing.T) {
	for name, dsn := range map[string]string{
		"empty":       "",
		"unparsable":  "postgres://app:pw@%zz/app",
		"unix socket": "host=/var/run/postgresql user=app",
	} {
		t.Run(name, func(t *testing.T) {
			l := &fakeLogin{}
			r, _ := Checker{Resolver: &fakeResolver{}, Dial: (&fakeDial{}).dial, Login: l.login}.Check(context.Background(), pl(dsn, ""))
			if r.Status != Unknown || r.Code != CodeUnparsed || len(l.seen) != 0 {
				t.Fatalf("%+v logins %v", r, l.seen)
			}
		})
	}
}

// The caller giving up establishes nothing: not a closed port.
func TestCancelledContextIsUnknown(t *testing.T) {
	res := &fakeResolver{hosts: map[string][]string{"db-gke.example.svc.cluster.local": {"10.0.0.5"}}}
	cctx, cancel := context.WithCancel(context.Background())
	cancel()
	r, err := Checker{Resolver: res, Dial: (&fakeDial{}).dial, Login: (&fakeLogin{}).login}.Check(cctx, pl(srcDSN, ""))
	if r.Status != Unknown || !errors.Is(err, context.Canceled) {
		t.Fatalf("%+v %v", r, err)
	}
}

func TestSlowDialIsBoundedByTheTimeout(t *testing.T) {
	res := &fakeResolver{hosts: map[string][]string{"db-gke.example.svc.cluster.local": {"10.0.0.5"}}}
	slow := func(ctx context.Context, _, _ string) (net.Conn, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	start := time.Now()
	r, _ := Checker{Timeout: 50 * time.Millisecond, Resolver: res, Dial: slow, Login: (&fakeLogin{}).login}.Check(context.Background(), pl(srcDSN, ""))
	if r.Status != Fail || r.Code != CodeTCP || time.Since(start) > 2*time.Second {
		t.Fatalf("%+v after %v", r, time.Since(start))
	}
}

// A login that hangs is bounded by the same timeout, and says nothing.
func TestSlowLoginIsBoundedByTheTimeout(t *testing.T) {
	res, d := bothUp()
	slow := func(ctx context.Context, _ string) error {
		<-ctx.Done()
		return ctx.Err()
	}
	start := time.Now()
	r, _ := Checker{Timeout: 50 * time.Millisecond, Resolver: res, Dial: d.dial, Login: slow}.Check(context.Background(), pl(srcDSN, dstDSN))
	if r.Status != Unknown || time.Since(start) > 2*time.Second {
		t.Fatalf("%+v after %v", r, time.Since(start))
	}
}

// Nothing in a result may carry an address, a credential or a server message,
// whatever failed.
func TestResultHoldsNoAddressOrSecret(t *testing.T) {
	res, d := bothUp()
	for name, c := range map[string]Checker{
		"no dns":     {Resolver: &fakeResolver{}, Dial: d.dial, Login: (&fakeLogin{}).login},
		"no tcp":     {Resolver: res, Dial: (&fakeDial{}).dial, Login: (&fakeLogin{}).login},
		"auth":       {Resolver: res, Dial: d.dial, Login: (&fakeLogin{err: pgError("28P01")}).login},
		"login oops": {Resolver: res, Dial: d.dial, Login: (&fakeLogin{err: errors.New("dial db-gke src-secret postgres://app")}).login},
	} {
		t.Run(name, func(t *testing.T) {
			r, _ := c.Check(context.Background(), pl(srcDSN, dstDSN))
			b, _ := json.Marshal(r)
			for _, leak := range []string{"src-secret", "dst-secret", "db-gke", "db-eks", "example.svc", "postgres://", "password authentication", "SASL"} {
				if strings.Contains(string(b), leak) {
					t.Fatalf("result leaks %q: %s", leak, b)
				}
			}
		})
	}
}
