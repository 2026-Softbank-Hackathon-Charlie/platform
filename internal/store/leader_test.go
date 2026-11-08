package store_test

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
)

func TestLeadershipAcrossPools(t *testing.T) {
	_, u := newSchema(t)
	a, b := openSchema(t, u), openSchema(t, u)
	defer a.Close()
	defer b.Close()
	ctx := t.Context()
	raw := make([]byte, 6)
	_, _ = rand.Read(raw)
	name := "test-" + hex.EncodeToString(raw) // advisory locks are database-wide

	if addr, err := b.LeaderAddr(ctx, name); err != nil || addr != "" {
		t.Fatalf("before any leader: %q, %v", addr, err)
	}
	la, err := a.TryLeadership(ctx, name, "10.0.0.1:8080")
	if err != nil {
		t.Fatal(err)
	}
	if err := la.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if lb, err := b.TryLeadership(ctx, name, "10.0.0.2:8080"); !errors.Is(err, store.ErrConflict) {
		if lb != nil {
			lb.Release()
		}
		t.Fatalf("second leader: %v", err)
	}
	if addr, err := b.LeaderAddr(ctx, name); err != nil || addr != "10.0.0.1:8080" {
		t.Fatalf("leader address: %q, %v", addr, err)
	}
	// Another name is another leadership.
	if other, err := b.LeaderAddr(ctx, name+"x"); err != nil || other != "" {
		t.Fatalf("other name: %q, %v", other, err)
	}

	la.Release()
	if err := la.Check(ctx); err == nil {
		t.Fatal("released leadership still checks")
	}
	if addr, err := b.LeaderAddr(ctx, name); err != nil || addr != "" {
		t.Fatalf("after release: %q, %v", addr, err)
	}
	lb, err := b.TryLeadership(ctx, name, "10.0.0.2:8080")
	if err != nil {
		t.Fatal(err)
	}
	defer lb.Release()
	if addr, err := a.LeaderAddr(ctx, name); err != nil || addr != "10.0.0.2:8080" {
		t.Fatalf("new leader address: %q, %v", addr, err)
	}
	if _, err := a.TryLeadership(ctx, "bad name", "x:1"); err == nil {
		t.Fatal("a name with a space was accepted")
	}
}
