//go:build !minio

package storage

import (
	"strings"
	"testing"
)

func TestMinIONotBuilt(t *testing.T) {
	err := validMinIO().Validate()
	if err == nil || !strings.Contains(err.Error(), "-tags minio") {
		t.Fatalf("a minio config in the default build: %v", err)
	}
	if _, err := New(t.Context(), validMinIO(), func(string) string { return "x" }); err == nil || !strings.Contains(err.Error(), "-tags minio") {
		t.Fatalf("New: %v", err)
	}
}
