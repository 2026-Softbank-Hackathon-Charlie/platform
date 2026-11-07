package runs

import (
	"context"
	"errors"
	"testing"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/engine"
	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/fileguard"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store/storetest"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

func TestFileGuardWiredOnStartAndResumeButRollbackExempt(t *testing.T) {
	for _, name := range []string{"start", "resume", "resume-halted", "rollback", "resume-rollback"} {
		t.Run(name, func(t *testing.T) {
			p := &plan.Plan{ID: "content", Source: plan.Endpoint{Env: "a", Namespace: "n", Cluster: "a"}, Target: plan.Endpoint{Env: "b", Namespace: "n", Cluster: "b"}, Files: &plan.FilesSpec{Source: "src:bucket", Target: "dst:bucket"}}
			guard := fileguard.New(p.Files.Source, p.Files.Target, nil, "", nil, nil)
			rollback := name == "rollback" || name == "resume-rollback"
			id := ""
			if name == "resume" || name == "resume-halted" || name == "resume-rollback" {
				id = "existing-run"
			}
			doc := apiv1.Run{}
			if rollback {
				origin := "original-run"
				doc.RollbackOf = &origin
			}
			called, guards := false, 0
			r := New(context.Background(), Config{Store: storetest.NewMemory(), FileSyncer: func(got *plan.Plan) engine.FileSyncer {
				guards++
				if got != p {
					t.Fatal("wrong plan")
				}
				return guard
			}, Factory: func(_ *plan.Plan, opts engine.Options) (Engine, error) {
				called = true
				if opts.RunID != id {
					t.Fatal("resume id lost")
				}
				if rollback && opts.Files != nil {
					t.Fatal("recovery exemption lost")
				}
				if !rollback && opts.Files != guard {
					t.Fatal("unguarded transfer adapter")
				}
				return nil, errors.New("stop before any engine/infra execution")
			}})
			_, err := r.launch(context.Background(), p, id, doc, name == "resume-halted")
			if !errors.Is(err, ErrEngineInit) || !called || (rollback && guards != 0) || (!rollback && guards != 1) {
				t.Fatalf("launch wiring: %v called=%v guards=%d", err, called, guards)
			}
		})
	}
}

func TestMissingFileGuardCannotFallBackToRawRclone(t *testing.T) {
	called := false
	r := New(context.Background(), Config{Store: storetest.NewMemory(), FileSyncer: func(*plan.Plan) engine.FileSyncer { return nil }, Factory: func(*plan.Plan, engine.Options) (Engine, error) {
		called = true
		return nil, errors.New("must not construct")
	}})
	p := &plan.Plan{Files: &plan.FilesSpec{}}
	_, err := r.launch(context.Background(), p, "", apiv1.Run{}, false)
	if !errors.Is(err, ErrEngineInit) || called {
		t.Fatalf("nil guard fallback: %v called=%v", err, called)
	}
}
