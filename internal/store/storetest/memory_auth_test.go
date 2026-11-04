package storetest

import (
	"testing"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
)

func TestMemoryAuth(t *testing.T) {
	ConformanceAuth(t, func(t *testing.T) store.Store { return NewMemory() })
}
