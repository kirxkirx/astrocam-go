//go:build !windows

package main

import (
	"fmt"
	"os"
	"syscall"
)

// fileLock holds an OS-level exclusive lock on a file.
// The lock is automatically released by the OS when the process exits,
// regardless of how it terminates (graceful shutdown, SIGKILL, crash, reboot).
type fileLock struct {
	file *os.File
}

// acquireFileLock attempts to take an exclusive lock on the given path.
// Returns an error if another instance already holds the lock.
func acquireFileLock(path string) (*fileLock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("could not open lock file %s: %w", path, err)
	}

	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("another instance of astrocam-go is already running (lock file: %s)", path)
	}

	return &fileLock{file: f}, nil
}

// release explicitly releases the file lock.
func (l *fileLock) release() {
	if l.file != nil {
		syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
		l.file.Close()
		l.file = nil
	}
}
