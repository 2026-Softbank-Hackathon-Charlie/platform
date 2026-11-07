package runs

import (
	"testing"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

// TestStoredActive: only a run that has not ended counts, the one whose
// cluster lease makes POST /v1/migrations refuse another run. The halted
// run of moimroom-mig on the live server (phase done, endedAt set) does not.
func TestStoredActive(t *testing.T) {
	ended := time.Now()
	for name, c := range map[string]struct {
		run  apiv1.Run
		want bool
	}{
		"running":          {apiv1.Run{Status: apiv1.RunStatusRunning, Phase: apiv1.RunPhaseSync}, true},
		"awaiting switch":  {apiv1.Run{Status: apiv1.RunStatusAwaitingSwitch, Phase: apiv1.RunPhaseAwaitingSwitch}, true},
		"halted and ended": {apiv1.Run{RunId: "moimroom-mig-eks-to-gke-20261107-021354", Status: apiv1.RunStatusHalted, Phase: apiv1.RunPhaseDone, EndedAt: &ended}, false},
		"interrupted":      {apiv1.Run{Status: apiv1.RunStatusInterrupted, Phase: apiv1.RunPhaseCutover}, false},
		"succeeded":        {apiv1.Run{Status: apiv1.RunStatusSucceeded, Phase: apiv1.RunPhaseDone, EndedAt: &ended}, false},
		"running but done": {apiv1.Run{Status: apiv1.RunStatusRunning, Phase: apiv1.RunPhaseDone}, false},
	} {
		if got := StoredActive(c.run); got != c.want {
			t.Errorf("%s: %v, want %v", name, got, c.want)
		}
	}
}
