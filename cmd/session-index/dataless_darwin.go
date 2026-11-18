//go:build darwin

package main

import (
	"os"
	"syscall"
)

// sfDataless is SF_DATALESS from macOS <sys/stat.h>: the file is a cloud
// placeholder whose content is not on disk, so reading it triggers a download.
const sfDataless = 0x40000000

// isDataless reports whether path is a dataless cloud placeholder. Errors
// count as not dataless; the read that follows reports them.
func isDataless(path string) bool {
	fi, err := os.Stat(path)
	if err != nil {
		return false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && st.Flags&sfDataless != 0
}
