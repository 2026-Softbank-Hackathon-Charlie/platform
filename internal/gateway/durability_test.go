package gateway

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func breakStateFile(t *testing.T, path string) func() {
	t.Helper()
	previous, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	return func() {
		t.Helper()
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, previous, 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestHoldMustBeDurableBeforeAcknowledgement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	tg := newTestGW(t, Config{StateFile: path}, "src")
	repair := breakStateFile(t, path)
	if err := tg.client.HoldWrites(context.Background()); err == nil {
		t.Fatal("acknowledged an unpersisted hold")
	}
	if st, _ := tg.client.State(context.Background(), 0); st.WritesHeld {
		t.Fatal("failed new hold was not reverted")
	}
	resp, err := http.Get(tg.admin.URL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 503 {
		t.Fatal("durability failure was hidden")
	}
	repair()
	if err := tg.client.HoldWrites(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := tg.client.ReleaseWrites(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestReleaseCannotBypassDurabilityFailure(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.json")
	tg := newTestGW(t, Config{StateFile: path}, "src", "dst")
	if err := tg.client.HoldWrites(ctx); err != nil {
		t.Fatal(err)
	}
	repair := breakStateFile(t, path)
	if err := tg.client.Route(ctx, "dst"); err != nil {
		t.Fatal(err)
	}
	if err := tg.client.ReleaseWrites(ctx); err == nil {
		t.Fatal("released without durable route")
	}
	st, _ := tg.client.State(ctx, 0)
	if !st.WritesHeld || st.WriteBackend != "dst" {
		t.Fatal("lost in-memory cutover state")
	}
	repair()
	if err := tg.client.ReleaseWrites(ctx); err != nil {
		t.Fatal(err)
	}
	disk, err := loadState(path)
	if err != nil || disk.WriteBackend != "dst" || disk.WritesHeld {
		t.Fatalf("incorrect saved cutover: %+v %v", disk, err)
	}
}

func TestRouteCannotSkipDrain(t *testing.T) {
	tg := newTestGW(t, Config{}, "src", "dst")
	unblock := tg.backends["src"].blockWrites()
	defer unblock()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	first := tg.asyncWrite(ctx, "old-writer")
	waitFor(t, "old write running", func() bool { return tg.backends["src"].activeWrites.Load() == 1 })
	held := make(chan error, 1)
	go func() { held <- tg.client.HoldWrites(ctx) }()
	waitFor(t, "hold in progress", func() bool { tg.g.mu.Lock(); defer tg.g.mu.Unlock(); return tg.g.held })
	wantAPIError(t, tg.client.Route(ctx, "dst"), 409, "writes_draining")
	unblock()
	if r := recv(t, first); r.err != nil || r.status != 200 {
		t.Fatalf("%+v", r)
	}
	if err := <-held; err != nil {
		t.Fatal(err)
	}
	if err := tg.client.Route(ctx, "dst"); err != nil {
		t.Fatal(err)
	}
	if err := tg.client.ReleaseWrites(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestRepeatedHoldPersistenceFailureDoesNotReleaseExistingHold(t *testing.T) {
	ctx := context.Background()
	file := filepath.Join(t.TempDir(), "state.json")
	tg := newTestGW(t, Config{StateFile: file}, "src")
	if err := tg.client.HoldWrites(ctx); err != nil {
		t.Fatal(err)
	}
	repair := breakStateFile(t, file)
	if err := tg.client.HoldWrites(ctx); err == nil {
		t.Fatal("acknowledged a failed save")
	}
	st, err := tg.client.State(ctx, 0)
	if err != nil || !st.WritesHeld {
		t.Fatalf("existing hold was released: %+v %v", st, err)
	}
	repair()
	if err := tg.client.ReleaseWrites(ctx); err != nil {
		t.Fatal(err)
	}
}
