package proc

import "syscall"

// sysProcAttr puts the child in its own process group and kills it if the
// daemon dies, so no child outlives a crashed daemon.
func sysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
}
