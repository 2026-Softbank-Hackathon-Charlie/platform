package storetest

import (
	"testing"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
)

func TestMemoryDeploy(t *testing.T) {
	ConformanceDeploy(t, func(t *testing.T) store.Store { return NewMemory() })
}
