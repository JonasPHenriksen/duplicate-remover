//go:build windows

package main

import (
	"syscall"
	"unsafe"
)

// ownsConsole reports whether the console window was opened just for this
// program, because it was double-clicked or had a folder dropped on it. That
// window closes the moment the program ends.
func ownsConsole() bool {
	var pids [2]uint32
	n, _, _ := syscall.NewLazyDLL("kernel32.dll").NewProc("GetConsoleProcessList").
		Call(uintptr(unsafe.Pointer(&pids[0])), uintptr(len(pids)))
	return n == 1
}
