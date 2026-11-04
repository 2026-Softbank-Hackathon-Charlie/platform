package storetest

import (
	"testing"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
)

func TestMemorySecrets(t *testing.T) {
	ConformanceSecrets(t, func(t *testing.T) store.Store { return NewMemory() })
}
