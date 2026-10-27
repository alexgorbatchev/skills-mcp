//go:build !linux

package proc

import "syscall"

// sysProcAttr starts the child in a new session: it leads its own process
// group and has no controlling terminal to prompt on. Pdeathsig is
// Linux-only.
func sysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}
