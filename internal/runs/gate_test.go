package runs

import (
	"strings"
	"testing"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

func TestEvaluateGate(t *testing.T) {
	rate, p95, minReq := float32(0.01), 800, 20
	g := apiv1.GateThresholds{MaxErrorRate: &rate, MaxP95Millis: &p95, MinRequests: &minReq}
	cases := []struct {
		name string
		s    apiv1.BackendStats
		pass bool
		why  string
	}{
		{"healthy", apiv1.BackendStats{Requests: 100, ErrorRate: 0.005, P95Millis: 300}, true, "100 requests"},
		{"at the limits", apiv1.BackendStats{Requests: 20, ErrorRate: 0.01, P95Millis: 800}, true, ""},
		{"too few requests", apiv1.BackendStats{Requests: 19}, false, "only 19 requests"},
		{"no data at all", apiv1.BackendStats{}, false, "only 0 requests"},
		{"errors", apiv1.BackendStats{Requests: 100, ErrorRate: 0.05, P95Millis: 100}, false, "error rate 5.00%"},
		{"slow", apiv1.BackendStats{Requests: 100, P95Millis: 801}, false, "p95 801ms"},
	}
	for _, tc := range cases {
		got := evaluateGate(tc.s, g)
		if got.Passed != tc.pass || !strings.Contains(got.Reason, tc.why) {
			t.Errorf("%s: %+v", tc.name, got)
		}
	}
}
