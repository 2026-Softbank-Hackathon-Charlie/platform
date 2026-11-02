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
type persistedState struct {
	WriteBackend string         `json:"writeBackend"`
	ReadWeights  map[string]int `json:"readWeights"`
	WritesHeld   bool           `json:"writesHeld"`
	HeldSince    *time.Time     `json:"heldSince,omitempty"`

	seq uint64 // orders snapshots taken under g.mu; not written
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

// snapshotLocked copies the routing state. Caller holds g.mu.
func (g *Gateway) snapshotLocked() persistedState {
	g.seq++
	st := persistedState{
		WriteBackend: g.writeBackend,
		ReadWeights:  make(map[string]int, len(g.readWeights)),
		WritesHeld:   g.held,
		seq:          g.seq,
	}
	for k, v := range g.readWeights {
		st.ReadWeights[k] = v
	}
	if g.held {
		hs := g.heldSince.UTC()
		st.HeldSince = &hs
	}
	return st
}

// save writes a snapshot atomically (temp file + rename). Snapshots are taken
// under g.mu but written outside it, so the proxy path never waits on disk; a
// snapshot older than the last one written is skipped.
func (g *Gateway) save(st persistedState) {
	if err := g.trySave(st); err != nil {
		// Routing already changed in memory; failing the admin call would
		// make the engine roll back a cutover over a disk problem.
		g.log.Error("persist gateway state", "file", g.cfg.StateFile, "err", err)
	}
}

func (g *Gateway) trySave(st persistedState) error {
	if g.cfg.StateFile == "" {
		return nil
	}
	g.saveMu.Lock()
	defer g.saveMu.Unlock()
	if st.seq <= g.savedSeq {
		return nil
	}
	if err := writeFileAtomic(g.cfg.StateFile, st); err != nil {
		return err
	}
	g.savedSeq = st.seq
	return nil
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
