//go:build !linux

package main

// diskSpins reports whether a folder is on a spinning hard disk. Only Linux
// tells, so elsewhere it says no, and several files are read at once.
func diskSpins(dir string) bool { return false }
