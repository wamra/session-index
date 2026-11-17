//go:build windows

package main

import (
	"os/exec"
	"syscall"
)

// detach starts cmd detached from the console so it outlives the hook process.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x00000008 | 0x00000200} // DETACHED_PROCESS | CREATE_NEW_PROCESS_GROUP
}
