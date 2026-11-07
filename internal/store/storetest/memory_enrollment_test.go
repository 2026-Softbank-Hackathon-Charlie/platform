package storetest

import (
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	"testing"
)

func TestMemoryEnrollmentConformance(t *testing.T) {
	ConformanceEnrollment(t, func(*testing.T) store.Store { return NewMemory() })
}
