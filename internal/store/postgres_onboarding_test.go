package store_test

import (
	"testing"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store/storetest"
)

func TestPostgresOnboardingConformance(t *testing.T) {
	testDatabaseURL(t)
	storetest.ConformanceOnboarding(t, func(t *testing.T) store.Store {
		_, u := newSchema(t)
		return openSchema(t, u)
	})
}
