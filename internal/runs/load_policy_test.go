package runs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/engine"
	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"
)

func TestEngineFactoryRejectsInvalidOperatorLoadPolicy(t *testing.T) {
	policy := filepath.Join(t.TempDir(), "load.json")
	if err := os.WriteFile(policy, []byte(`{"prometheusURL":"http://localhost","unknown":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOPMIGRATE_LOAD_POLICY", policy)
	p, err := plan.Parse([]byte(`{"id":"load","source":{"env":"gke","namespace":"n","cluster":"a","dsn":"postgres://x@a/db"},"target":{"env":"eks","namespace":"n","cluster":"b","dsn":"postgres://x@b/db"}}`))
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "uncreated")
	_, err = EngineFactory(p, engine.Options{StateDir: dir})
	if err == nil || !strings.Contains(err.Error(), "invalid DB load policy JSON") {
		t.Fatalf("invalid server policy was not rejected: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("state created before policy validation: %v", err)
	}
}
