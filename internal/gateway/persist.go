package gateway

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// persistedState is what survives a restart. A gateway that restarts while
// writes are held must keep holding: releasing them to the old backend in the
// middle of a cutover would split writes across two databases.
//
// The top-level fields are the default app, in the format gateways wrote
// before there were other apps, so either version reads the other's file for
// the default app.
type persistedState struct {
	WriteBackend string         `json:"writeBackend"`
	ReadWeights  map[string]int `json:"readWeights"`
	WritesHeld   bool           `json:"writesHeld"`
	HeldSince    *time.Time     `json:"heldSince,omitempty"`
	// Apps are the apps registered through the admin API, by name.
	Apps map[string]persistedApp `json:"apps,omitempty"`
}

// persistedApp is one registered app: its backends, which are not in the
// configuration, and its routing in the same fields as the default app.
type persistedApp struct {
	HealthPath string            `json:"healthPath,omitempty"`
	Backends   map[string]string `json:"backends"`
	persistedState
}

func loadState(path string) (*persistedState, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var st persistedState
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, fmt.Errorf("state file %s: %w", path, err)
	}
	return &st, nil
}

// persistLocked copies one app's routing. Caller holds rt.mu.
func (rt *router) persistLocked() persistedApp {
	pa := persistedApp{
		HealthPath: rt.healthPath,
		Backends:   make(map[string]string, len(rt.backends)),
		persistedState: persistedState{
			WriteBackend: rt.writeBackend,
			ReadWeights:  make(map[string]int, len(rt.readWeights)),
			WritesHeld:   rt.held,
		},
	}
	for _, b := range rt.backends {
		pa.Backends[b.name] = b.url.String()
	}
	for k, v := range rt.readWeights {
		pa.ReadWeights[k] = v
	}
	if rt.held {
		hs := rt.heldSince.UTC()
		pa.HeldSince = &hs
	}
	return pa
}

func (rt *router) persist() persistedApp {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.persistLocked()
}

// snapshot reads every app's routing as it is now.
func (g *Gateway) snapshot() persistedState {
	g.appsMu.RLock()
	defer g.appsMu.RUnlock()
	st := g.router.persist().persistedState // backends come from the configuration
	if len(g.apps) > 0 {
		st.Apps = make(map[string]persistedApp, len(g.apps))
		for name, rt := range g.apps {
			st.Apps[name] = rt.persist()
		}
	}
	return st
}

// save writes the state of every app after a change. It reads the state when
// it writes, under saveMu, so a later write never holds older state than an
// earlier one, and the proxy path never waits on disk.
func (g *Gateway) save() {
	if err := g.trySave(); err != nil {
		// Routing already changed in memory; failing the admin call would
		// make the engine roll back a cutover over a disk problem.
		g.log.Error("persist gateway state", "file", g.cfg.StateFile, "err", err)
	}
}

func (g *Gateway) trySave() error {
	if g.cfg.StateFile == "" {
		return nil
	}
	g.saveMu.Lock()
	defer g.saveMu.Unlock()
	err := writeFileAtomic(g.cfg.StateFile, g.snapshot())
	g.persistenceFailed.Store(err != nil)
	return err
}

func writeFileAtomic(path string, st persistedState) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // no-op after a successful rename
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
