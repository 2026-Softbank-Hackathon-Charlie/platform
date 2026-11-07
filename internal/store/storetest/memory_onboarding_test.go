package storetest

import (
	"testing"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
)

func TestMemoryOnboarding(t *testing.T) {
	ConformanceOnboarding(t, func(t *testing.T) store.Store { return NewMemory() })
}
