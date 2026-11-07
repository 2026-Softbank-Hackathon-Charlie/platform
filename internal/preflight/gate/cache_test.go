package gate

import (
	"testing"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"
)

func TestInspectionBindingAllowsReverseButNotDifferentAppOrPolicy(t *testing.T) {
	g := contentGate(t, nil)
	p := contentPlan()
	p.Gateway.URL = "http://gateway/apps/shop"
	first := g.inspectionBinding(p)
	reversed := *p
	reversed.Source, reversed.Target = p.Target, p.Source
	reversed.Files = &plan.FilesSpec{Source: p.Files.Target, Target: p.Files.Source}
	reversed.ID = "a-different-successful-return-run"
	if first == "" || g.inspectionBinding(&reversed) != first {
		t.Fatal("normal reverse move cannot reuse exact content classification")
	}
	for _, change := range []string{"namespace", "cluster", "context", "kubeconfig", "environment", "bucket", "app route", "policy"} {
		t.Run(change, func(t *testing.T) {
			q := *p
			files := *p.Files
			q.Files = &files
			copyGate := *g
			switch change {
			case "namespace":
				q.Target.Namespace = "another-app"
			case "cluster":
				q.Target.Cluster = "another-database"
			case "context":
				q.Target.Context = "another-cluster"
			case "kubeconfig":
				q.Target.Kubeconfig = "another-config"
			case "environment":
				q.Target.Env = "another-env"
			case "bucket":
				q.Files.Target = "gcs:other-app"
			case "app route":
				q.Gateway.URL = "http://gateway/apps/another-app"
			case "policy":
				copyGate.digest = "a-new-policy-revision"
			}
			if copyGate.inspectionBinding(&q) == first {
				t.Fatal("changed execution scope reused content classification")
			}
		})
	}
}
