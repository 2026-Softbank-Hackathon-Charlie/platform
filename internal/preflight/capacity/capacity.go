// Package capacity measures, before a migration starts, whether it would fail
// for reasons visible in advance: a target that cannot accept the source's
// connections, or a replica that cannot catch up within the plan's timeout.
//
// Every number comes from the live source and target databases. A value that
// cannot be measured is unknown and never blocks: the engine's own lag gate
// and the observe phase still guard the run. Only a measured, clear failure
// denies.
package capacity

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// Status of one check.
type Status string

const (
	Pass    Status = "pass"
	Fail    Status = "fail"
	Unknown Status = "unknown"
	Info    Status = "info" // reported, never decides
)

// Check IDs.
const (
	CheckConnections = "connections"
	// CheckStandbySettings: the target starts as a hot standby of the source,
	// and PostgreSQL refuses to start one whose max_connections or
	// max_worker_processes is below the primary's.
	CheckStandbySettings = "standby_settings"
	CheckCatchup         = "catchup"
	CheckInitialCopy     = "initial_copy"
)

// ConnectionHeadroom is how many more connections than the source has now the
// target must accept: reconnecting applications briefly exceed steady state.
const ConnectionHeadroom = 1.2

// Measurements are what Prober read. Nil means not measured.
type Measurements struct {
	SourceBytes          *int64   `json:"sourceBytes,omitempty"`
	WALBytesPerSecond    *float64 `json:"walBytesPerSecond,omitempty"`
	SourceSessions       *int     `json:"sourceSessions,omitempty"`
	SourceMaxConnections *int     `json:"sourceMaxConnections,omitempty"`
	// SourceMaxWorkerProcesses matters because a hot standby needs at least
	// the primary's value to start.
	SourceMaxWorkerProcesses *int `json:"sourceMaxWorkerProcesses,omitempty"`
	TargetMaxConnections     *int `json:"targetMaxConnections,omitempty"`
	// TargetMaxConnectionsFrom is "target" (read from the running target) or
	// "manifest" (the target does not exist yet).
	TargetMaxConnectionsFrom string `json:"targetMaxConnectionsFrom,omitempty"`
	TargetMaxWorkerProcesses *int   `json:"targetMaxWorkerProcesses,omitempty"`
	// TargetMaxWorkerProcessesFrom is "target" or "manifest", as above.
	TargetMaxWorkerProcessesFrom string `json:"targetMaxWorkerProcessesFrom,omitempty"`
	// LagBefore and LagAfter are the replica's lag in bytes at the start and
	// end of the window, measured as the engine does. Nil without a replica.
	LagBefore *int64        `json:"lagBefore,omitempty"`
	LagAfter  *int64        `json:"lagAfter,omitempty"`
	Window    time.Duration `json:"window"`
}

// Thresholds come from the plan, whose engine defaults are already applied.
type Thresholds struct {
	MaxLagBytes    int64
	CatchupTimeout time.Duration
}

// Check is one judgement. Message holds numbers only, never addresses.
type Check struct {
	ID      string `json:"id"`
	Status  Status `json:"status"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Report is the outcome. Allowed is false only when a check failed.
type Report struct {
	Allowed      bool         `json:"allowed"`
	Checks       []Check      `json:"checks"`
	Measurements Measurements `json:"measurements"`
}

// Failed returns the failed checks.
func (r Report) Failed() []Check {
	var out []Check
	for _, c := range r.Checks {
		if c.Status == Fail {
			out = append(out, c)
		}
	}
	return out
}

// Evaluate judges measurements against the plan's thresholds.
func Evaluate(m Measurements, t Thresholds) Report {
	r := Report{Allowed: true, Measurements: m, Checks: []Check{connections(m), standbySettings(m), catchup(m, t), initialCopy(m)}}
	for _, c := range r.Checks {
		if c.Status == Fail {
			r.Allowed = false
		}
	}
	return r
}

func connections(m Measurements) Check {
	c := Check{ID: CheckConnections, Status: Unknown}
	switch {
	case m.SourceSessions == nil:
		c.Code, c.Message = "source_not_measured", "source client sessions could not be read"
		return c
	case m.TargetMaxConnections == nil:
		c.Code, c.Message = "target_not_measured", "target max_connections is unknown (target unreachable and not set in the manifest)"
		return c
	}
	need := int(math.Ceil(float64(*m.SourceSessions) * ConnectionHeadroom))
	have := *m.TargetMaxConnections
	detail := fmt.Sprintf("target max_connections %d (from %s), required %d (source client sessions %d x %.1f)",
		have, m.TargetMaxConnectionsFrom, need, *m.SourceSessions, ConnectionHeadroom)
	if have < need {
		c.Status, c.Code, c.Message = Fail, "target_connections_insufficient", detail
		return c
	}
	c.Status, c.Code, c.Message = Pass, "ok", detail
	return c
}

// standbySettings fails when a measured target setting is below the source's.
// A setting that is unknown on either side is skipped; when none can be
// compared the check is unknown.
func standbySettings(m Measurements) Check {
	c := Check{ID: CheckStandbySettings, Status: Unknown}
	type setting struct {
		name      string
		src, dst  *int
		dstSource string
	}
	var compared, short []string
	for _, s := range []setting{
		{"max_connections", m.SourceMaxConnections, m.TargetMaxConnections, m.TargetMaxConnectionsFrom},
		{"max_worker_processes", m.SourceMaxWorkerProcesses, m.TargetMaxWorkerProcesses, m.TargetMaxWorkerProcessesFrom},
	} {
		if s.src == nil || s.dst == nil {
			continue
		}
		d := fmt.Sprintf("%s %d (from %s), source %d", s.name, *s.dst, s.dstSource, *s.src)
		compared = append(compared, d)
		if *s.dst < *s.src {
			short = append(short, d)
		}
	}
	switch {
	case len(short) > 0:
		c.Status, c.Code = Fail, "target_below_source"
		c.Message = "a hot standby needs at least the source's value: " + strings.Join(short, "; ")
	case len(compared) == 0:
		c.Code, c.Message = "not_measured", "source or target max_connections and max_worker_processes are unknown"
	default:
		c.Status, c.Code, c.Message = Pass, "ok", strings.Join(compared, "; ")
	}
	return c
}

func catchup(m Measurements, t Thresholds) Check {
	c := Check{ID: CheckCatchup, Status: Unknown}
	if m.LagBefore == nil || m.LagAfter == nil || m.Window <= 0 {
		c.Code, c.Message = "no_replica", "no streaming replica to measure; the engine's lag gate decides"
		return c
	}
	before, after := *m.LagBefore, *m.LagAfter
	if after <= t.MaxLagBytes {
		c.Status, c.Code = Pass, "caught_up"
		c.Message = fmt.Sprintf("lag %s <= %s", bytes(after), bytes(t.MaxLagBytes))
		return c
	}
	rate := float64(before-after) / m.Window.Seconds() // bytes the replica gains per second
	if rate <= 0 {
		c.Status, c.Code = Fail, "not_catching_up"
		c.Message = fmt.Sprintf("lag %s -> %s over %s: the replica is not gaining on the source (limit %s)",
			bytes(before), bytes(after), m.Window, bytes(t.MaxLagBytes))
		return c
	}
	eta := time.Duration(float64(after-t.MaxLagBytes) / rate * float64(time.Second))
	detail := fmt.Sprintf("lag %s -> %s over %s, about %s to reach %s (catchup timeout %s)",
		bytes(before), bytes(after), m.Window, eta.Round(time.Second), bytes(t.MaxLagBytes), t.CatchupTimeout)
	if eta > t.CatchupTimeout {
		c.Status, c.Code, c.Message = Fail, "catchup_too_slow", detail
		return c
	}
	c.Status, c.Code, c.Message = Pass, "catching_up", detail
	return c
}

func initialCopy(m Measurements) Check {
	c := Check{ID: CheckInitialCopy, Status: Info, Code: "measured"}
	switch {
	case m.SourceBytes != nil && m.WALBytesPerSecond != nil:
		c.Message = fmt.Sprintf("source %s, changes %s/s", bytes(*m.SourceBytes), bytes(int64(*m.WALBytesPerSecond)))
	case m.SourceBytes != nil:
		c.Message = fmt.Sprintf("source %s", bytes(*m.SourceBytes))
	default:
		c.Status, c.Code, c.Message = Unknown, "not_measured", "source size could not be read"
	}
	return c
}

// bytes renders n in binary units.
func bytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	v, i := float64(n), 0
	for v >= unit && i < 4 {
		v /= unit
		i++
	}
	return fmt.Sprintf("%.1f %ciB", v, "KMGT"[i-1])
}
