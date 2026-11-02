package storetest

import (
	"testing"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
)

func TestMemoryAgent(t *testing.T) {
	ConformanceAgent(t, func(t *testing.T) store.Store { return NewMemory() })
}
