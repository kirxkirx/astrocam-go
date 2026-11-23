//go:build !windows

package main

import (
	"os"
	"syscall"
)

// errCrossDevice is what a rename returns when the destination is on another
// filesystem than the source.
const errCrossDevice = syscall.EXDEV

// syncDir makes the entries of a directory - a file just renamed into it -
// survive a power cut. Without it the rename can still be lost after the
// original has already been deleted from another filesystem.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
