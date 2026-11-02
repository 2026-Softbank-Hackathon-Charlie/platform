package gateway

import (
	"context"
	"net/http"
	"strings"
	"testing"

	enginegw "github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/gateway"
)

// The platform's client can stand in wherever the engine wants a gateway.
var _ enginegw.Gateway = (*Client)(nil)

// TestEngineClientCompatibility drives the admin API with the engine's own
// client, in the order the engine uses it: precheck, fence, cutover, and the
// rollback path.
func TestEngineClientCompatibility(t *testing.T) {
	tg := newTestGW(t, Config{}, "gke", "eks")
	ctx := context.Background()
	eng := enginegw.NewHTTP(tg.admin.URL+"/", testToken) // NewHTTP trims the slash
	state := func() (writeBackend string, held bool, eksReads int) {
		st, err := tg.client.State(ctx, 0)
		if err != nil {
			t.Fatal(err)
		}
		return st.WriteBackend, st.WritesHeld, st.ReadWeights["eks"]
	}

	if err := eng.Health(ctx); err != nil {
		t.Fatalf("precheck Health: %v", err)
	}

	// fence
	if err := eng.HoldWrites(ctx); err != nil {
		t.Fatalf("HoldWrites: %v", err)
	}
	if wb, held, _ := state(); wb != "gke" || !held {
		t.Fatalf("after hold: writeBackend=%s held=%v", wb, held)
	}
	pending := tg.asyncWrite(ctx, "during-fence")
	waitFor(t, "write held", func() bool { return tg.heldWrites() == 1 })

	// cutover: route, then release
	if err := eng.Route(ctx, "eks"); err != nil {
		t.Fatalf("Route: %v", err)
	}
	if wb, held, eks := state(); wb != "eks" || !held || eks != 100 {
		t.Fatalf("after route: writeBackend=%s held=%v eksReads=%d", wb, held, eks)
	}
	if err := eng.ReleaseWrites(ctx); err != nil {
		t.Fatalf("ReleaseWrites: %v", err)
	}
	if _, held, _ := state(); held {
		t.Fatal("still held after release")
	}
	r := recv(t, pending)
	if r.status != http.StatusOK || r.backend != "eks" {
		t.Fatalf("held write: %+v", r)
	}
	if tg.backends["eks"].got("during-fence") != 1 || tg.backends["gke"].got("during-fence") != 0 {
		t.Fatal("held write must reach only the new backend")
	}

	// rollback: hold, route back to the source, release.
	if err := eng.HoldWrites(ctx); err != nil {
		t.Fatal(err)
	}
	if err := eng.Route(ctx, "gke"); err != nil {
		t.Fatal(err)
	}
	if err := eng.ReleaseWrites(ctx); err != nil {
		t.Fatal(err)
	}
	if err := eng.ReleaseWrites(ctx); err != nil {
		t.Fatalf("second release must be a no-op: %v", err)
	}
	if wb, held, _ := state(); wb != "gke" || held {
		t.Fatalf("after rollback: writeBackend=%s held=%v", wb, held)
	}

	err := enginegw.NewHTTP(tg.admin.URL, "wrong").HoldWrites(ctx)
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("wrong token: %v", err)
	}
	if err := eng.Route(ctx, "aks"); err == nil || !strings.Contains(err.Error(), "unknown_backend") {
		t.Fatalf("unknown backend: %v", err)
	}
}
