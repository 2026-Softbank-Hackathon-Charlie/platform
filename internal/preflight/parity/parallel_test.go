package parity

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestIndependentCapabilitiesStartTogetherAndKeepOrder(t *testing.T) {
	r := request()
	r.Mandatory = append([]Capability(nil), capabilities[:]...)
	started := make(chan Capability, len(capabilities))
	release := make(chan struct{})
	probes := map[Capability]Probe{}
	for _, c := range capabilities {
		probes[c] = probeFunc(func(ctx context.Context, target Target) (Result, error) {
			started <- c
			select {
			case <-release:
				return passing(c).Check(ctx, target)
			case <-ctx.Done():
				return Result{}, ctx.Err()
			}
		})
	}
	done := make(chan Report, 1)
	go func() { report, _ := Evaluate(context.Background(), r, probes); done <- report }()
	allStarted := true
	for range capabilities {
		select {
		case <-started:
		case <-time.After(time.Second):
			allStarted = false
		}
		if !allStarted {
			break
		}
	}
	close(release)
	report := <-done
	if !allStarted {
		t.Fatal("independent capabilities waited for an earlier probe to finish")
	}
	if !report.Passed || len(report.Results) != len(capabilities) {
		t.Fatalf("%+v", report)
	}
	for i, c := range capabilities {
		if report.Results[i].Capability != c {
			t.Fatal("completion order changed the report contract")
		}
	}
}

func TestCancellationJoinsEveryStartedCapability(t *testing.T) {
	r := request()
	r.Mandatory = append([]Capability(nil), capabilities[:]...)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started, exited := make(chan struct{}, len(capabilities)), make(chan struct{}, len(capabilities))
	probes := map[Capability]Probe{}
	for _, c := range capabilities {
		probes[c] = probeFunc(func(ctx context.Context, _ Target) (Result, error) {
			started <- struct{}{}
			<-ctx.Done()
			exited <- struct{}{}
			return Result{}, errors.New("secret provider error")
		})
	}
	done := make(chan error, 1)
	go func() {
		report, err := Evaluate(ctx, r, probes)
		if report.Passed {
			done <- errors.New("cancelled report passed")
			return
		}
		done <- err
	}()
	for range capabilities {
		select {
		case <-started:
		case <-time.After(time.Second):
			cancel()
			<-done
			t.Fatal("capabilities did not start independently")
		}
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("%v", err)
	}
	if len(exited) != len(capabilities) {
		t.Fatal("evaluation returned before probes exited")
	}
}
