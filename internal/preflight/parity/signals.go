package parity

import (
	"context"
	"time"
)

// SignalScope is entirely server-bound. Cluster is the expected collector
// resource identity, not a hostname or credentials supplied by an API caller.
type SignalScope struct {
	Target                Target
	Cluster               string
	DeploymentEnvironment string
	Services              []string
}

// SignalCount contains aggregate evidence only, never a log line or prompt.
type SignalCount struct {
	Signal   string
	Samples  int64
	LastSeen time.Time
	Code     string
}

type SignalReader interface {
	SignalCounts(context.Context, SignalScope, time.Time, time.Time) ([]SignalCount, error)
}
