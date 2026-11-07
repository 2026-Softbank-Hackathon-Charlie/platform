//go:build !linux

package gateway

// Exclusive owner failover is Linux-only. Preserve existing development
// behavior on systems which cannot fsync an open directory.
func syncStateDirectory(string) error { return nil }
