package capacity

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func i(n int) *int         { return &n }
func i64(n int64) *int64   { return &n }
func f(n float64) *float64 { return &n }

const mib = 1 << 20

var defaults = Thresholds{MaxLagBytes: 16 * mib, CatchupTimeout: 10 * time.Minute}

func check(r Report, id string) Check {
	for _, c := range r.Checks {
		if c.ID == id {
			return c
		}
	}
	return Check{}
}

func TestConnections(t *testing.T) {
	for name, tc := range map[string]struct {
		sessions, max *int
		status        Status
		code          string
	}{
		"enough":             {i(340), i(500), Pass, "ok"},
		"exactly required":   {i(340), i(408), Pass, "ok"}, // ceil(340*1.2) = 408
		"one short":          {i(340), i(407), Fail, "target_connections_insufficient"},
		"far short":          {i(340), i(200), Fail, "target_connections_insufficient"},
		"idle source":        {i(0), i(10), Pass, "ok"},
		"source unknown":     {nil, i(200), Unknown, "source_not_measured"},
		"target unknown":     {i(340), nil, Unknown, "target_not_measured"},
		"rounds headroom up": {i(1), i(1), Fail, "target_connections_insufficient"}, // ceil(1.2) = 2
	} {
		t.Run(name, func(t *testing.T) {
			r := Evaluate(Measurements{SourceSessions: tc.sessions, TargetMaxConnections: tc.max, TargetMaxConnectionsFrom: "target"}, defaults)
			c := check(r, CheckConnections)
			if c.Status != tc.status || c.Code != tc.code || r.Allowed != (tc.status != Fail) {
				t.Fatalf("%+v allowed=%v", c, r.Allowed)
			}
		})
	}
	c := check(Evaluate(Measurements{SourceSessions: i(340), TargetMaxConnections: i(200), TargetMaxConnectionsFrom: "manifest"}, defaults), CheckConnections)
	if c.Message != "target max_connections 200 (from manifest), required 408 (source client sessions 340 x 1.2)" {
		t.Fatalf("message %q", c.Message)
	}
}

func TestStandbySettings(t *testing.T) {
	for name, tc := range map[string]struct {
		m      Measurements
		status Status
		code   string
	}{
		"both equal": {Measurements{SourceMaxConnections: i(200), TargetMaxConnections: i(200), SourceMaxWorkerProcesses: i(8), TargetMaxWorkerProcesses: i(8)}, Pass, "ok"},
		"both above": {Measurements{SourceMaxConnections: i(100), TargetMaxConnections: i(300), SourceMaxWorkerProcesses: i(8), TargetMaxWorkerProcesses: i(16)}, Pass, "ok"},
		"connections below": {Measurements{SourceMaxConnections: i(200), TargetMaxConnections: i(199), SourceMaxWorkerProcesses: i(8), TargetMaxWorkerProcesses: i(8)},
			Fail, "target_below_source"},
		"workers below": {Measurements{SourceMaxConnections: i(100), TargetMaxConnections: i(100), SourceMaxWorkerProcesses: i(16), TargetMaxWorkerProcesses: i(8)},
			Fail, "target_below_source"},
		"workers unknown, connections ok": {Measurements{SourceMaxConnections: i(100), TargetMaxConnections: i(100), SourceMaxWorkerProcesses: i(8)}, Pass, "ok"},
		"workers unknown, connections below": {Measurements{SourceMaxConnections: i(100), TargetMaxConnections: i(50), SourceMaxWorkerProcesses: i(8)},
			Fail, "target_below_source"},
		"source unknown": {Measurements{TargetMaxConnections: i(100), TargetMaxWorkerProcesses: i(8)}, Unknown, "not_measured"},
		"nothing":        {Measurements{}, Unknown, "not_measured"},
	} {
		t.Run(name, func(t *testing.T) {
			r := Evaluate(tc.m, defaults)
			c := check(r, CheckStandbySettings)
			if c.Status != tc.status || c.Code != tc.code || r.Allowed != (tc.status != Fail) {
				t.Fatalf("%+v allowed=%v", c, r.Allowed)
			}
		})
	}
	// The sessions check passes here (2 x 1.2 <= 150); only the standby rule fails.
	r := Evaluate(Measurements{SourceSessions: i(2), SourceMaxConnections: i(200), TargetMaxConnections: i(150), TargetMaxConnectionsFrom: "manifest",
		SourceMaxWorkerProcesses: i(8), TargetMaxWorkerProcesses: i(4), TargetMaxWorkerProcessesFrom: "manifest"}, defaults)
	if check(r, CheckConnections).Status != Pass || r.Allowed {
		t.Fatalf("%+v", r)
	}
	if c := check(r, CheckStandbySettings); c.Message != "a hot standby needs at least the source's value: max_connections 150 (from manifest), source 200; max_worker_processes 4 (from manifest), source 8" {
		t.Fatalf("message %q", c.Message)
	}
}

func TestCatchup(t *testing.T) {
	window := 5 * time.Second
	for name, tc := range map[string]struct {
		before, after *int64
		status        Status
		code          string
	}{
		"caught up":    {i64(20 * mib), i64(1 * mib), Pass, "caught_up"},
		"at the limit": {i64(16 * mib), i64(16 * mib), Pass, "caught_up"},
		// 100 MiB gained in 5s = 20 MiB/s; 984 MiB left takes ~49s.
		"catching up in time": {i64(1100 * mib), i64(1000 * mib), Pass, "catching_up"},
		// 1 MiB in 5s = 0.2 MiB/s; ~984 MiB left takes ~82 min > 10 min.
		"too slow":       {i64(1001 * mib), i64(1000 * mib), Fail, "catchup_too_slow"},
		"standing":       {i64(500 * mib), i64(500 * mib), Fail, "not_catching_up"},
		"falling behind": {i64(500 * mib), i64(600 * mib), Fail, "not_catching_up"},
		"no replica":     {nil, nil, Unknown, "no_replica"},
		"half sample":    {i64(500 * mib), nil, Unknown, "no_replica"},
	} {
		t.Run(name, func(t *testing.T) {
			r := Evaluate(Measurements{LagBefore: tc.before, LagAfter: tc.after, Window: window}, defaults)
			c := check(r, CheckCatchup)
			if c.Status != tc.status || c.Code != tc.code || r.Allowed != (tc.status != Fail) {
				t.Fatalf("%+v allowed=%v", c, r.Allowed)
			}
		})
	}
}

// Nothing measured: nothing blocks, everything says so.
func TestUnknownNeverBlocks(t *testing.T) {
	r := Evaluate(Measurements{}, defaults)
	if !r.Allowed || len(r.Failed()) != 0 {
		t.Fatalf("%+v", r)
	}
	for _, c := range r.Checks {
		if c.Status != Unknown || c.Message == "" {
			t.Fatalf("%+v", c)
		}
	}
}

func TestInitialCopyIsInformational(t *testing.T) {
	r := Evaluate(Measurements{SourceBytes: i64(42 << 30), WALBytesPerSecond: f(3.1 * mib)}, defaults)
	c := check(r, CheckInitialCopy)
	if c.Status != Info || c.Message != "source 42.0 GiB, changes 3.1 MiB/s" || !r.Allowed {
		t.Fatalf("%+v", c)
	}
}

func TestManifestMaxConnections(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	set := write("set.yaml", "apiVersion: postgresql.cnpg.io/v1\nkind: Cluster\nspec:\n  postgresql:\n    parameters:\n      max_connections: \"300\"\n      shared_buffers: 256MB\n")
	if n, ok := ManifestMaxConnections(set); !ok || n != 300 {
		t.Fatalf("set: %d %v", n, ok)
	}
	workers := write("workers.yaml", "spec:\n  postgresql:\n    parameters:\n      max_worker_processes: \"16\"\n")
	if n, ok := ManifestParameter(workers, "max_worker_processes"); !ok || n != 16 {
		t.Fatalf("workers: %d %v", n, ok)
	}
	if _, ok := ManifestParameter(set, "max_worker_processes"); ok {
		t.Fatal("max_worker_processes read from a manifest that does not set it")
	}
	for name, path := range map[string]string{
		"unset":      write("unset.yaml", "kind: Cluster\nspec:\n  instances: 2\n"),
		"not number": write("bad.yaml", "spec:\n  postgresql:\n    parameters:\n      max_connections: lots\n"),
		"zero":       write("zero.yaml", "spec:\n  postgresql:\n    parameters:\n      max_connections: \"0\"\n"),
		"not yaml":   write("junk.yaml", "spec: [unclosed"),
		"missing":    filepath.Join(dir, "missing.yaml"),
		"no path":    "",
	} {
		if n, ok := ManifestMaxConnections(path); ok {
			t.Errorf("%s: got %d", name, n)
		}
	}
}

func TestMessagesHoldNoAddresses(t *testing.T) {
	r := Evaluate(Measurements{SourceSessions: i(5), TargetMaxConnections: i(1), TargetMaxConnectionsFrom: "target",
		LagBefore: i64(900 * mib), LagAfter: i64(900 * mib), Window: time.Second, SourceBytes: i64(1)}, defaults)
	for _, c := range r.Checks {
		if strings.Contains(c.Message, "postgres://") || strings.Contains(c.Message, ".ts.net") {
			t.Fatalf("%+v", c)
		}
	}
}
