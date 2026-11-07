package plans

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

func TestPlatformRejectsCLIDecommissionOption(t *testing.T) {
	p := &plan.Plan{ID: "app", Decommission: &plan.DecommissionSpec{Selector: "app=app"}}
	err := DefaultPolicy().checkParsed(p, dbHosts{})
	if err == nil || !strings.Contains(err.Error(), "decommission is not accepted") {
		t.Fatalf("CLI-only decommission accepted: %v", err)
	}
}

func TestThreeEnvironmentReversePlanPreservesSourceRewireOptIn(t *testing.T) {
	raw := []byte(`{"id":"mig-gke-to-onprem","allowSourceRewire":true,"source":{"env":"gke","namespace":"mig","cluster":"mig-db","dsn":"source"},"target":{"env":"onprem","namespace":"mig","cluster":"mig-db","dsn":"target"}}`)
	reversed, err := reversePlan(raw)
	if err != nil {
		t.Fatal(err)
	}
	var p apiv1.EnginePlan
	if err := json.Unmarshal(reversed, &p); err != nil {
		t.Fatal(err)
	}
	if p.AllowSourceRewire == nil || !*p.AllowSourceRewire || p.Source.Env != "onprem" || p.Target.Env != "gke" {
		t.Fatalf("reverse dropped topology opt-in or endpoints: %+v", p)
	}
}
