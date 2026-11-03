package store_test

import (
	"testing"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store/storetest"
)

func TestPostgresDeployConformance(t *testing.T) {
	testDatabaseURL(t)
	storetest.ConformanceDeploy(t, func(t *testing.T) store.Store {
		_, u := newSchema(t)
		return openSchema(t, u)
	})
}
