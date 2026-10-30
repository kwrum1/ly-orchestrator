//go:build linux

package service

import (
	"os/exec"
	"syscall"
)

func setCommandProcessGroup(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func killCommandProcessGroup(command *exec.Cmd) {
	if command.Process == nil {
		return
	}
	// Negative PID addresses the whole process group, so a systemctl helper
	// that forked diagnostics is killed as well.
	_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
}
