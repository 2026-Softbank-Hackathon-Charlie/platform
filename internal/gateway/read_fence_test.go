package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type readFenceSnapshot struct {
	Supported  *bool             `json:"sourceReadExclusionSupported"`
	Exclusions map[string]string `json:"readExclusions"`
	InFlight   map[string]int    `json:"readInFlight"`
}

func readFenceState(t *testing.T, c *Client) readFenceSnapshot {
	t.Helper()
	var state readFenceSnapshot
	if err := c.do(context.Background(), http.MethodGet, "/v1/state", nil, &state); err != nil {
		t.Fatal(err)
	}
	return state
}

func fenceBody(backend, lease string) string {
	body, _ := json.Marshal(map[string]string{"backend": backend, "lease": lease})
	return string(body)
}

func newReadFenceGW(t *testing.T) *testGW {
	t.Helper()
	return newTestGW(t, Config{StateFile: filepath.Join(t.TempDir(), "state.json"), DrainTimeout: time.Second}, "src", "dst")
}

func excludeReads(t *testing.T, tg *testGW, path, backend, lease string) {
	t.Helper()
	status, code := tg.adminDo(t, http.MethodPost, path+"/v1/reads/exclude", "Bearer "+testToken, fenceBody(backend, lease))
	if status != http.StatusOK {
		t.Fatalf("exclude: %d %s", status, code)
	}
}

func TestReadExclusionCapabilityRequiresDurability(t *testing.T) {
	for _, durable := range []bool{false, true} {
		cfg := Config{}
		if durable {
			cfg.StateFile = filepath.Join(t.TempDir(), "state.json")
		}
		tg := newTestGW(t, cfg, "src")
		state := readFenceState(t, tg.client)
		if state.Supported == nil || *state.Supported != durable || len(state.InFlight) != 1 || state.InFlight["src"] != 0 {
			t.Fatalf("durable=%v: %+v", durable, state)
		}
	}
}

func TestReadExclusionLeaseAndControlGuards(t *testing.T) {
	tg := newReadFenceGW(t)
	ctx := context.Background()
	if err := tg.client.HoldWrites(ctx); err != nil {
		t.Fatal(err)
	}
	if err := tg.client.SetReadWeights(ctx, map[string]int{"dst": 100}); err != nil {
		t.Fatal(err)
	}
	excludeReads(t, tg, "", "src", "run-a")
	state := readFenceState(t, tg.client)
	if state.Exclusions["src"] != "run-a" || state.InFlight["src"] != 0 {
		t.Fatalf("missing drained source lease: %+v", state)
	}
	for _, test := range []struct{ path, body string }{
		{"/v1/reads/exclude", fenceBody("src", "run-b")},
		{"/v1/reads/include", fenceBody("src", "run-b")},
		{"/v1/route", `{"backend":"src"}`},
		{"/v1/weights", `{"readWeights":{"src":100}}`},
		{"/v1/writes/release", ""},
	} {
		status, _ := tg.adminDo(t, http.MethodPost, test.path, "Bearer "+testToken, test.body)
		if status != http.StatusConflict {
			t.Fatalf("guard %s: %d", test.path, status)
		}
	}
	status, code := tg.adminDo(t, http.MethodPost, "/v1/reads/include", "Bearer "+testToken, fenceBody("src", "run-a"))
	if status != http.StatusOK {
		t.Fatalf("include: %d %s", status, code)
	}
	if len(readFenceState(t, tg.client).Exclusions) != 0 {
		t.Fatal("matching include kept the lease")
	}
	if err := tg.client.ReleaseWrites(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestReadExclusionPreventsWriteBackendFallback(t *testing.T) {
	tg := newReadFenceGW(t)
	ctx := context.Background()
	if err := tg.client.HoldWrites(ctx); err != nil {
		t.Fatal(err)
	}
	if err := tg.client.SetReadWeights(ctx, map[string]int{"dst": 100}); err != nil {
		t.Fatal(err)
	}
	excludeReads(t, tg, "", "src", "run-a")
	tg.g.byName["dst"].healthy.Store(false)
	response := tg.hostDo(ctx, http.MethodGet, "unscoped", "/orders", "")
	if response.err != nil || response.status != http.StatusServiceUnavailable {
		t.Fatalf("no safe backend: %+v", response)
	}
	if tg.backends["src"].reads.Load() != 0 {
		t.Fatal("excluded source received a fallback read")
	}
	tg.g.byName["dst"].healthy.Store(true)
	tg.backends["dst"].readStatus.Store(http.StatusServiceUnavailable)
	response = tg.hostDo(ctx, http.MethodGet, "unscoped", "/orders", "")
	if response.err != nil || response.status != http.StatusServiceUnavailable || tg.backends["src"].reads.Load() != 0 {
		t.Fatalf("excluded source received a retry: %+v", response)
	}
}

func TestReadExclusionPersistenceFailureStaysClosed(t *testing.T) {
	tg := newReadFenceGW(t)
	if err := tg.client.HoldWrites(context.Background()); err != nil {
		t.Fatal(err)
	}
	good := tg.g.cfg.StateFile
	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocker, []byte("block"), 0o600); err != nil {
		t.Fatal(err)
	}
	tg.g.cfg.StateFile = filepath.Join(blocker, "state.json")
	defer func() { tg.g.cfg.StateFile = good }()
	status, code := tg.adminDo(t, http.MethodPost, "/v1/reads/exclude", "Bearer "+testToken, fenceBody("src", "run-a"))
	if status != http.StatusServiceUnavailable || code != "persistence_unavailable" {
		t.Fatalf("failed persistence acknowledged exclusion: %d %s", status, code)
	}
	if readFenceState(t, tg.client).Exclusions["src"] != "run-a" {
		t.Fatal("failed persistence reopened reads in memory")
	}
	status, code = tg.adminDo(t, http.MethodPost, "/v1/reads/include", "Bearer "+testToken, fenceBody("src", "run-a"))
	if status != http.StatusServiceUnavailable || code != "persistence_unavailable" || readFenceState(t, tg.client).Exclusions["src"] != "run-a" {
		t.Fatalf("failed include reopened source: %d %s", status, code)
	}
}

func TestReadExclusionRejectsInvalidInput(t *testing.T) {
	tg := newReadFenceGW(t)
	for _, body := range []string{fenceBody("src", ""), fenceBody("unknown", "run-a"), fenceBody("src", strings.Repeat("x", 129)), fenceBody("src", "bad lease")} {
		status, _ := tg.adminDo(t, http.MethodPost, "/v1/reads/exclude", "Bearer "+testToken, body)
		if status != http.StatusBadRequest {
			t.Fatalf("invalid request %q: %d", body, status)
		}
	}
}
