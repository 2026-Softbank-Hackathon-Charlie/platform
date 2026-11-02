package security

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestAWSLive reads a real account with the collector's read-only calls and
// logs what it found. It runs only when PLATFORM_TEST_AWS_LIVE is set, with
// credentials from the default chain, e.g.:
//
//	PLATFORM_TEST_AWS_LIVE=1 AWS_PROFILE=hopcharlie go test -run TestAWSLive -v ./internal/security
func TestAWSLive(t *testing.T) {
	if os.Getenv("PLATFORM_TEST_AWS_LIVE") == "" {
		t.Skip("PLATFORM_TEST_AWS_LIVE is not set")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	for _, r := range NewAWSCollector(os.Getenv("PLATFORM_AWS_REGION")).Collect(ctx) {
		t.Logf("%-16s %-14s %d findings: %s", r.Name, r.Status, len(r.Findings), r.Message)
		for _, f := range r.Findings {
			t.Logf("    %-8s %s [%s]", f.Severity, f.Title, f.Resource)
		}
	}
}
