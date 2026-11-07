package store_test

import (
	"errors"
	"testing"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
)

func TestClusterLeaseAcrossPools(t *testing.T) {
	_, u := newSchema(t)
	a, b := openSchema(t, u), openSchema(t, u)
	defer a.Close()
	defer b.Close()
	lease, err := a.TryClusterLease(t.Context(), []string{"a/app/old", "b/app/new", "a/app/old"})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if err := lease.Check(t.Context()); err != nil {
		t.Fatal(err)
	}
	if blocked, err := b.TryClusterLease(t.Context(), []string{"b/app/new"}); !errors.Is(err, store.ErrConflict) {
		if blocked != nil {
			blocked.Release()
		}
		t.Fatalf("competing lease=%v", err)
	}
	// A failed multi-key acquisition releases all earlier keys.
	if blocked, err := b.TryClusterLease(t.Context(), []string{"0-free", "b/app/new"}); !errors.Is(err, store.ErrConflict) {
		if blocked != nil {
			blocked.Release()
		}
		t.Fatalf("partial=%v", err)
	}
	free, err := a.TryClusterLease(t.Context(), []string{"0-free"})
	if err != nil {
		t.Fatal(err)
	}
	free.Release()
	lease.Release()
	resumed, err := b.TryClusterLease(t.Context(), []string{"b/app/new"})
	if err != nil {
		t.Fatal(err)
	}
	resumed.Release()
}
