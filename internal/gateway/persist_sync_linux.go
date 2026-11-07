//go:build linux

package gateway

import "os"

// File fsync alone does not make the rename durable across a host crash.
func syncStateDirectory(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
