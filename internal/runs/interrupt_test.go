package runs

import (
	"context"
	"testing"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store/storetest"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

// A waiting run ends as soon as stopCh closes (its phase becomes done and
// finish forgets it), so InterruptWaiting must count before it closes the
// channel. The test holds r.mu while InterruptWaiting runs: counting after the
// close would let the run end first and report 0.
func TestInterruptWaitingCountsRunThatEndsAtOnce(t *testing.T) {
	r := New(context.Background(), Config{Store: storetest.NewMemory()})
	l := &live{id: "run_1"}
	l.doc.Phase = apiv1.RunPhaseAwaitingSwitch
	r.live[l.id] = l
	ended := make(chan struct{})
	go func() { // plays drive and finish: the run ends as soon as stopCh closes
		<-r.stopCh
		l.mu.Lock()
		l.doc.Phase = apiv1.RunPhaseDone
		l.mu.Unlock()
		close(ended)
	}()

	r.mu.Lock()
	got := make(chan int)
	go func() { got <- r.InterruptWaiting() }()
	select {
	case <-ended: // stopCh closed before the count could take r.mu
	case <-time.After(100 * time.Millisecond):
	}
	r.mu.Unlock()
	if n := <-got; n != 1 {
		t.Fatalf("InterruptWaiting = %d, want 1", n)
	}
	<-ended
}
