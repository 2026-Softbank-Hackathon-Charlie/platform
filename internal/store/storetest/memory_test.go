package storetest

import (
	"testing"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
)

func TestMemory(t *testing.T) {
	Conformance(t, func(t *testing.T) store.Store { return NewMemory() })
}
