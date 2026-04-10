//go:build windows

package main

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

var (
	modkernel32      = syscall.NewLazyDLL("kernel32.dll")
	procLockFileEx   = modkernel32.NewProc("LockFileEx")
	procUnlockFileEx = modkernel32.NewProc("UnlockFileEx")
)

const (
	lockfileExclusiveLock = 0x02
	lockfileFailImmediately = 0x01
)

// fileLock holds an OS-level exclusive lock on a file.
// The lock is automatically released by the OS when the process exits,
// regardless of how it terminates (graceful shutdown, crash, reboot).
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

	ol := new(syscall.Overlapped)
	r1, _, err := procLockFileEx.Call(
		f.Fd(),
		uintptr(lockfileExclusiveLock|lockfileFailImmediately),
		0,
		1, 0,
		uintptr(unsafe.Pointer(ol)),
	)
	if r1 == 0 {
		f.Close()
		return nil, fmt.Errorf("another instance of astrocam-go is already running (lock file: %s)", path)
	}

	return &fileLock{file: f}, nil
}

// release explicitly releases the file lock.
func (l *fileLock) release() {
	if l.file != nil {
		ol := new(syscall.Overlapped)
		procUnlockFileEx.Call(
			l.file.Fd(),
			0,
			1, 0,
			uintptr(unsafe.Pointer(ol)),
		)
		l.file.Close()
		l.file = nil
	}
}
