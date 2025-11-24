//go:build !windows

package main

// No-op on non-Windows systems
func disableQuickEditMode() {}
