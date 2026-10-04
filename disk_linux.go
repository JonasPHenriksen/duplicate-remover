//go:build linux

package main

import (
	"fmt"
	"os"
	"strings"
	"syscall"
)

// diskSpins reports whether a folder is on a spinning hard disk, which reads one
// file at a time faster than several at once. Linux tells, through /sys: for a
// partition, its disk does.
func diskSpins(dir string) bool {
	var st syscall.Stat_t
	if syscall.Stat(dir, &st) != nil {
		return false
	}
	dev := uint64(st.Dev)
	major := uint32(dev>>8)&0xfff | uint32(dev>>32)&^0xfff
	minor := uint32(dev)&0xff | uint32(dev>>12)&^0xff
	block := fmt.Sprintf("/sys/dev/block/%d:%d", major, minor)
	for _, p := range []string{block + "/queue/rotational", block + "/../queue/rotational"} {
		if data, err := os.ReadFile(p); err == nil {
			return strings.TrimSpace(string(data)) == "1"
		}
	}
	return false
}
