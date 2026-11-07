//go:build linux

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// Never unlink this file: replacing its inode would create two lock domains.
// In particular, do not lock the JSON file, which persistence atomically replaces.
type stateLock struct{ file *os.File }

func openStateLock(state string) (*stateLock, error) {
	if state == "" {
		return nil, errors.New("exclusive state requires a state file")
	}
	if err := os.MkdirAll(filepath.Dir(state), 0700); err != nil {
		return nil, err
	}
	fd, err := unix.Open(state+".lock", unix.O_RDWR|unix.O_CREAT|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, fmt.Errorf("open exclusive state lock: %w", err)
	}
	f := os.NewFile(uintptr(fd), state+".lock")
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		f.Close()
		return nil, errors.New("exclusive state lock must be a regular file")
	}
	var fs unix.Statfs_t
	if err := unix.Fstatfs(fd, &fs); err != nil {
		f.Close()
		return nil, err
	}
	// These remote filesystems do not provide the local-kernel lock semantics
	// required here. Other filesystems still need an operator-verified local PVC.
	if fs.Type == unix.NFS_SUPER_MAGIC || fs.Type == unix.CIFS_SUPER_MAGIC || fs.Type == unix.FUSE_SUPER_MAGIC {
		f.Close()
		return nil, errors.New("exclusive state requires a local filesystem, not a network or FUSE filesystem")
	}
	return &stateLock{file: f}, nil
}

func (l *stateLock) TryLock() (bool, error) {
	err := unix.Flock(int(l.file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
		return false, nil
	}
	return err == nil, err
}

func (l *stateLock) Close() error { return l.file.Close() }
