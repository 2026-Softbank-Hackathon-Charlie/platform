//go:build !linux

package main

import "errors"

type stateLock struct{}

func openStateLock(string) (*stateLock, error) {
	return nil, errors.New("exclusive state is supported only on Linux with a shared local PVC")
}
func (*stateLock) TryLock() (bool, error) {
	return false, errors.New("exclusive state is unsupported on this operating system")
}
func (*stateLock) Close() error { return nil }
