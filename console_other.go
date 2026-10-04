//go:build !windows

package main

// ownsConsole reports whether the console window was opened just for this
// program. Only Windows closes it when the program ends.
func ownsConsole() bool { return false }
