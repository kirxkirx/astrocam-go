//go:build windows

package main

import "syscall"

// errCrossDevice is what a rename returns when the destination is on another
// drive than the source: ERROR_NOT_SAME_DEVICE, "The system cannot move the
// file to a different disk drive."
const errCrossDevice = syscall.Errno(17)

// syncDir does nothing on Windows. Go cannot open a folder with the write
// access that FlushFileBuffers needs, and NTFS commits a rename through its
// own journal; finishPartialCopies at the next start covers what is missed.
func syncDir(dir string) error {
	return nil
}
