package proc

import "syscall"

// sysProcAttr starts the child in a new session: it leads its own process
// group, has no controlling terminal to prompt on, and is killed if the
// daemon dies, so no child outlives a crashed daemon.
func sysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true, Pdeathsig: syscall.SIGKILL}
}
