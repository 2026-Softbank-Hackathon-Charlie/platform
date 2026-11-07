package plans

import (
	"strings"
	"testing"
)

func TestWrittenGenerationDoesNotExposeSourceCredentials(t *testing.T) {
	t.Setenv("WRITTEN_TEST_MISSING_PASSWORD", "")
	_, err := WrittenGeneration(t.Context(), "postgres://private-user:${WRITTEN_TEST_MISSING_PASSWORD}@private-host/app")
	if err == nil || strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), "PASSWORD") {
		t.Fatalf("source identity error leaked credentials: %v", err)
	}
}
