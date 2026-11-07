package runs

import "testing"

// A step sends enough probe reads that probeFactor x the minimum are expected
// on the target at its weight, at most maxProbes.
func TestProbeCountScalesWithWeight(t *testing.T) {
	for _, c := range []struct{ weight, want int }{
		{1, 300}, {10, 300}, {25, 240}, {50, 120}, {75, 80}, {100, 60},
	} {
		if got := probeCount(20, c.weight); got != c.want {
			t.Errorf("probeCount(20, %d) = %d, want %d", c.weight, got, c.want)
		}
	}
}
