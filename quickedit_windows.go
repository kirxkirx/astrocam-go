//go:build windows

package main

import (
	"fmt"
	"syscall"
	"unsafe"
)

func disableQuickEditMode() {
	kernel32, err := syscall.LoadDLL("kernel32.dll")
	if err != nil {
		return
	}
	defer kernel32.Release()
	
	getStdHandle, _ := kernel32.FindProc("GetStdHandle")
	getConsoleMode, _ := kernel32.FindProc("GetConsoleMode")
	setConsoleMode, _ := kernel32.FindProc("SetConsoleMode")
	
	handle, _, _ := getStdHandle.Call(^uintptr(10) + 1) // STD_INPUT_HANDLE
	if handle == 0 {
		return
	}
	
	var mode uint32
	ret, _, _ := getConsoleMode.Call(handle, uintptr(unsafe.Pointer(&mode)))
	if ret == 0 {
		return
	}
	
	// Disable QuickEdit (0x0040) and set Extended flags (0x0080)
	newMode := (mode &^ 0x0040) | 0x0080
	
	ret, _, _ = setConsoleMode.Call(handle, uintptr(newMode))
	if ret != 0 {
		fmt.Println("Windows QuickEdit mode disabled (text selection will not freeze the program)")
	}
}
